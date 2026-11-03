package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// The alert evaluator reaches this through an optional interface; keep the
// signature pinned so a drift fails the build instead of silently dropping
// diagnoses.
var _ interface {
	GetAlertErrorBreakdown(context.Context, *service.OpsDashboardFilter, bool, int) ([]*service.OpsAlertErrorBreakdown, error)
} = (*opsRepository)(nil)

// GetAlertErrorBreakdown groups the client-visible errors of an alert window by
// (account, model, status, message) so alert emails can say *what* failed instead
// of only a ratio. Business-limited rows are included on purpose: "no available
// accounts" routing failures are business-limited but are exactly what an
// operator needs to see.
//
// Rows that describe site-side problems sort first so that a flood of
// client-side noise (no balance, bad requests) cannot push the real cause out of
// the top N. upstreamOnly narrows to what upstream_error_rate counts.
func (r *opsRepository) GetAlertErrorBreakdown(ctx context.Context, filter *service.OpsDashboardFilter, upstreamOnly bool, limit int) ([]*service.OpsAlertErrorBreakdown, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil ops repository")
	}
	if filter == nil {
		return nil, fmt.Errorf("nil filter")
	}
	if limit <= 0 || limit > 50 {
		limit = 5
	}

	args := []any{filter.StartTime.UTC(), filter.EndTime.UTC()}
	clauses := []string{
		"e.created_at >= $1",
		"e.created_at < $2",
		"e.is_count_tokens = FALSE",
		"COALESCE(e.status_code, 0) >= 400",
	}
	if filter.GroupID != nil && *filter.GroupID > 0 {
		args = append(args, *filter.GroupID)
		clauses = append(clauses, fmt.Sprintf("e.group_id = $%d", len(args)))
	}
	if p := strings.TrimSpace(strings.ToLower(filter.Platform)); p != "" {
		args = append(args, p)
		clauses = append(clauses, fmt.Sprintf("e.platform = $%d", len(args)))
	}
	if upstreamOnly {
		clauses = append(clauses,
			"e.error_owner = 'provider'",
			"NOT COALESCE(e.is_business_limited, FALSE)",
			"COALESCE(e.upstream_status_code, e.status_code, 0) NOT IN (429, 529)")
	}
	args = append(args, limit)

	q := `
SELECT
  COUNT(*) AS cnt,
  COALESCE(e.error_owner, '') AS owner,
  COALESCE(e.error_phase, '') AS phase,
  COALESCE(e.platform, '') AS platform,
  COALESCE(NULLIF(e.requested_model, ''), e.model, '') AS model,
  COALESCE(e.status_code, 0) AS status_code,
  COALESCE(e.upstream_status_code, 0) AS upstream_status_code,
  e.account_id,
  COALESCE(a.name, '') AS account_name,
  LEFT(COALESCE(NULLIF(e.upstream_error_message, ''), e.error_message, ''), 300) AS msg,
  COUNT(DISTINCT e.user_id) AS user_count,
  (ARRAY_AGG(DISTINCT u.email) FILTER (WHERE u.email IS NOT NULL AND u.email <> ''))[1:3] AS user_emails,
  BOOL_OR(COALESCE(e.is_business_limited, FALSE)) AS business_limited,
  MAX(e.created_at) AS last_seen
FROM ops_error_logs e
LEFT JOIN accounts a ON a.id = e.account_id
LEFT JOIN users u ON u.id = e.user_id
WHERE ` + strings.Join(clauses, " AND ") + `
GROUP BY 2, 3, 4, 5, 6, 7, 8, 9, 10
ORDER BY
  MIN(CASE WHEN e.error_owner = 'client'
            OR (COALESCE(e.is_business_limited, FALSE) AND COALESCE(e.error_phase, '') <> 'routing')
           THEN 1 ELSE 0 END),
  cnt DESC, last_seen DESC
LIMIT $` + fmt.Sprintf("%d", len(args))

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*service.OpsAlertErrorBreakdown, 0, limit)
	for rows.Next() {
		var (
			item       service.OpsAlertErrorBreakdown
			accountID  sql.NullInt64
			userEmails pq.StringArray
			lastSeen   time.Time
		)
		if err := rows.Scan(
			&item.Count,
			&item.Owner,
			&item.Phase,
			&item.Platform,
			&item.Model,
			&item.StatusCode,
			&item.UpstreamStatusCode,
			&accountID,
			&item.AccountName,
			&item.Message,
			&item.UserCount,
			&userEmails,
			&item.BusinessLimited,
			&lastSeen,
		); err != nil {
			return nil, err
		}
		if accountID.Valid {
			id := accountID.Int64
			item.AccountID = &id
		}
		item.UserEmails = []string(userEmails)
		item.LastSeenAt = lastSeen
		out = append(out, &item)
	}
	return out, rows.Err()
}
