//go:build unit

package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type stubOpsRepoWithBreakdown struct {
	stubOpsRepo
	rows      []*OpsAlertErrorBreakdown
	gotFilter *OpsDashboardFilter
}

func (s *stubOpsRepoWithBreakdown) GetAlertErrorBreakdown(_ context.Context, filter *OpsDashboardFilter, _ bool, _ int) ([]*OpsAlertErrorBreakdown, error) {
	s.gotFilter = filter
	return s.rows, nil
}

func TestExplainOpsAlertError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   OpsAlertErrorBreakdown
		want string
	}{
		{"expired oauth token", OpsAlertErrorBreakdown{Owner: "provider", StatusCode: 502, UpstreamStatusCode: 401, Message: "Your authentication token has expired. Please try refreshing it."}, "登录凭证失效"},
		{"no accounts at routing", OpsAlertErrorBreakdown{Owner: "platform", Phase: "routing", StatusCode: 503, Message: "Service temporarily unavailable"}, "无可用账号"},
		{"user balance", OpsAlertErrorBreakdown{Owner: "client", StatusCode: 403, Message: "insufficient balance"}, "用户余额不足"},
		{"codex websocket probe", OpsAlertErrorBreakdown{Owner: "client", StatusCode: 426, Message: "WebSocket upgrade required (Upgrade: websocket)"}, "客户端 WebSocket 探测"},
		{"upstream 429", OpsAlertErrorBreakdown{Owner: "provider", StatusCode: 429, Message: "slow down"}, "上游限流/额度用完"},
		{"stream cut", OpsAlertErrorBreakdown{Owner: "provider", StatusCode: 502, Message: "upstream stream closed before a terminal event"}, "上游流中断"},
		{"unsupported model", OpsAlertErrorBreakdown{Owner: "platform", StatusCode: 404, Message: `Model "x" is not supported by any configured account in this group`}, "模型不支持"},
		{"owner fallback", OpsAlertErrorBreakdown{Owner: "platform", StatusCode: 500, Message: "Failed to get user info"}, "本站内部错误"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			short, meaning, action := explainOpsAlertError(&tc.in)
			require.Equal(t, tc.want, short)
			require.NotEmpty(t, meaning)
			require.NotEmpty(t, action)
		})
	}
}

func TestOpsAlertSampleSufficient(t *testing.T) {
	t.Parallel()

	rate := &OpsAlertRule{MetricType: "error_rate"}
	// The 2026-11-02 email: 1 failure out of 7 requests must not alert.
	require.False(t, opsAlertSampleSufficient(rate, opsAlertSample{Requests: 7, Errors: 1}))
	require.True(t, opsAlertSampleSufficient(rate, opsAlertSample{Requests: 7, Errors: 3}))

	require.True(t, opsAlertSampleSufficient(&OpsAlertRule{MetricType: "error_rate", Filters: map[string]any{"min_errors": float64(0)}}, opsAlertSample{Requests: 1, Errors: 1}))
	require.False(t, opsAlertSampleSufficient(&OpsAlertRule{MetricType: "error_rate", Filters: map[string]any{"min_requests": "20"}}, opsAlertSample{Requests: 10, Errors: 9}))

	// Non-rate rules are never gated.
	require.True(t, opsAlertSampleSufficient(&OpsAlertRule{MetricType: "group_available_accounts"}, opsAlertSample{}))
}

func TestDedupeOpsAlertBatch(t *testing.T) {
	t.Parallel()

	mk := func(id int64, metric, sev string, filters map[string]any) opsAlertFired {
		return opsAlertFired{
			rule:  &OpsAlertRule{ID: id, MetricType: metric, Severity: sev, Filters: filters},
			event: &OpsAlertEvent{ID: id * 10},
		}
	}
	items := []opsAlertFired{
		mk(1, "error_rate", "P1", nil),
		mk(2, "success_rate", "P0", nil),
		mk(8, "error_rate", "P0", nil),
		mk(10, "group_available_accounts", "P0", map[string]any{"group_id": float64(2)}),
		mk(11, "group_available_accounts", "P0", map[string]any{"group_id": float64(4)}),
	}
	out := dedupeOpsAlertBatch(items)
	ids := make([]int64, 0, len(out))
	for _, it := range out {
		ids = append(ids, it.rule.ID)
	}
	// success_rate/error_rate collapse to the first P0 of the family; different groups stay apart.
	require.Equal(t, []int64{2, 10, 11}, ids)
}

func TestDiagnoseAlert_ErrorRateNamesAccountAndCause(t *testing.T) {
	t.Parallel()

	repo := &stubOpsRepoWithBreakdown{rows: []*OpsAlertErrorBreakdown{
		{Count: 4, Owner: "client", StatusCode: 426, Message: "WebSocket upgrade required (Upgrade: websocket)"},
		{Count: 3, Owner: "provider", Phase: "upstream", Platform: "composite", Model: "gpt-6-astra", StatusCode: 502, UpstreamStatusCode: 401,
			AccountID: int64Ptr(43), AccountName: "acct43@example.com", Message: "Your authentication token has expired.", UserCount: 1, UserEmails: []string{"a@b.c"}},
	}}
	svc := &OpsAlertEvaluatorService{opsRepo: repo}
	rule := &OpsAlertRule{Name: "错误率过高", Severity: "P1", MetricType: "error_rate", Operator: ">", Threshold: 5}
	end := time.Date(2026, 11, 2, 10, 40, 0, 0, time.UTC)

	d := svc.diagnoseAlert(context.Background(), rule, 14.29, opsAlertSample{Requests: 49, Errors: 7}, end.Add(-5*time.Minute), end, "", int64Ptr(7), end)

	require.Equal(t, "账号 #43 acct43@example.com 登录凭证失效", d.Headline, "client-side noise must not win the headline")
	require.Contains(t, d.Summary, "共 49 个请求，失败 7 个")
	require.Contains(t, d.Summary, "其中 4 个是客户端自身问题")
	require.Len(t, d.Causes, 2)
	require.Equal(t, "账号 #43 acct43@example.com · gpt-6-astra · HTTP 502（上游 401）", d.Causes[1].Where)
	require.NotNil(t, repo.gotFilter.GroupID)
	require.Equal(t, int64(7), *repo.gotFilter.GroupID)
}

func TestDiagnoseAlert_GroupAccountsExplainsWhy(t *testing.T) {
	t.Parallel()

	now := time.Now()
	reset := now.Add(30 * time.Hour)
	cool := now.Add(8 * time.Minute)
	availability := &OpsAccountAvailability{
		Group: &GroupAvailability{GroupID: 2, AvailableCount: 0, TotalAccounts: 3},
		Accounts: map[int64]*AccountAvailability{
			31: {AccountID: 31, AccountName: "a@x", Status: StatusActive, Schedulable: true, IsRateLimited: true, RateLimitResetAt: &reset},
			43: {AccountID: 43, AccountName: "b@x", Status: StatusActive, Schedulable: true, TempUnschedulableUntil: &cool, TempUnschedulableReason: "OAuth 401: token expired"},
			45: {AccountID: 45, AccountName: "c@x", Status: StatusError, HasError: true, ErrorMessage: "refresh failed"},
		},
	}
	svc := &OpsAlertEvaluatorService{
		opsRepo: &stubOpsRepo{},
		opsService: &OpsService{getAccountAvailability: func(context.Context, string, *int64) (*OpsAccountAvailability, error) {
			return availability, nil
		}},
	}
	rule := &OpsAlertRule{Name: "GPT 可用账号为 0", Severity: "P0", MetricType: "group_available_accounts", Operator: "<", Threshold: 1}

	d := svc.diagnoseAlert(context.Background(), rule, 0, opsAlertSample{}, now.Add(-time.Minute), now, "", int64Ptr(2), now)

	require.Contains(t, d.Summary, "可用账号 0 个")
	require.Equal(t, "账号状态（可用 0 / 共 3）", d.AccountsTitle)
	require.Len(t, d.Accounts, 3)
	require.Equal(t, "#31 a@x", d.Accounts[0].Name)
	require.Contains(t, d.Accounts[0].State, "限流中")
	require.Contains(t, d.Accounts[1].State, "临时冷却至")
	require.Equal(t, "OAuth 401: token expired", d.Accounts[1].Detail)
	require.Equal(t, "出错，已停止调度", d.Accounts[2].State)
	require.True(t, strings.HasPrefix(d.Headline, "1 个限流、1 个临时冷却、1 个出错，最早 "), d.Headline)
}

func TestBuildOpsAlertDetailsHTMLEscapes(t *testing.T) {
	t.Parallel()

	items := []opsAlertFired{{
		rule:  &OpsAlertRule{Name: "<b>rule</b>", Severity: "P1"},
		event: &OpsAlertEvent{ID: 1},
		diagnosis: &opsAlertDiagnosis{
			Summary: "s",
			Causes:  []opsAlertCause{{Count: 1, Where: "w", Meaning: "m", Action: "a", Message: `<script>alert(1)</script>`}},
		},
	}}
	out := buildOpsAlertDetailsHTML(items, "https://relay.example/admin/ops")
	require.NotContains(t, out, "<script>")
	require.NotContains(t, out, "<b>rule</b>")
	require.Contains(t, out, "&lt;script&gt;")
}

func TestOpsAlertEmailRendersDiagnosis(t *testing.T) {
	t.Parallel()

	fired := time.Date(2026, 11, 2, 10, 40, 26, 0, time.UTC)
	items := []opsAlertFired{{
		rule:      &OpsAlertRule{ID: 1, Name: "错误率过高", Severity: "P1", MetricType: "error_rate", Operator: ">", Threshold: 5},
		event:     &OpsAlertEvent{ID: 349, Status: OpsAlertStatusFiring, FiredAt: fired, MetricValue: float64Ptr(14.29)},
		diagnosis: &opsAlertDiagnosis{Headline: "账号 #43 x 登录凭证失效", Summary: "过去 5 分钟共 7 个请求"},
	}, {
		rule:  &OpsAlertRule{ID: 10, Name: "GPT 可用账号为 0", Severity: "P0", MetricType: "group_available_accounts"},
		event: &OpsAlertEvent{ID: 347, Status: OpsAlertStatusFiring, FiredAt: fired},
	}}
	items = dedupeOpsAlertBatch(items)
	vars := opsAlertEmailVariables(items)
	require.Equal(t, "P0", vars["severity"])
	require.Equal(t, "GPT 可用账号为 0（另有 1 项）", vars["alert_headline"])
	require.Equal(t, "GPT 可用账号为 0、错误率过高", vars["rule_name"])

	tmpl := notificationEmailOfficialTemplates[NotificationEmailEventOpsAlert][notificationEmailLocaleChinese]
	vars["site_name"] = "大牛 API"
	vars["ops_dashboard_url"] = "https://relay.example/admin/ops"
	rendered, err := renderNotificationEmail(NotificationEmailEventOpsAlert, tmpl.Subject, tmpl.HTML, vars,
		map[string]string{"alert_details_html": buildOpsAlertDetailsHTML(items, "https://relay.example/admin/ops")})
	require.NoError(t, err)
	require.Equal(t, "[运维告警][P0] GPT 可用账号为 0（另有 1 项）", rendered.Subject)
	require.Contains(t, rendered.HTML, `<div style="border-left:4px solid #dc2626;`)
	require.Contains(t, rendered.HTML, "登录凭证失效")
	require.Contains(t, rendered.HTML, `href="https://relay.example/admin/ops"`)
}

type stubOpsRepoActiveEvents struct {
	stubOpsRepo
	active map[int64]*OpsAlertEvent
}

func (s *stubOpsRepoActiveEvents) GetActiveAlertEvent(_ context.Context, ruleID int64) (*OpsAlertEvent, error) {
	return s.active[ruleID], nil
}

func TestCoveredByEarlierAlert_OnlyWhenEmailed(t *testing.T) {
	t.Parallel()

	p1 := &OpsAlertRule{ID: 1, Enabled: true, NotifyEmail: true, MetricType: "error_rate", Severity: "P1"}
	p0 := &OpsAlertRule{ID: 8, Enabled: true, NotifyEmail: true, MetricType: "error_rate", Severity: "P0"}
	rules := []*OpsAlertRule{p1, p0}

	repo := &stubOpsRepoActiveEvents{active: map[int64]*OpsAlertEvent{8: {ID: 80, EmailSent: false}}}
	svc := &OpsAlertEvaluatorService{opsRepo: repo}
	// The P0 is firing but its email never went out: the P1 must still be sent.
	require.False(t, svc.coveredByEarlierAlert(context.Background(), rules, p1, map[int64]struct{}{1: {}}))

	repo.active[8].EmailSent = true
	require.True(t, svc.coveredByEarlierAlert(context.Background(), rules, p1, map[int64]struct{}{1: {}}))

	// A lower-severity sibling never covers a higher one.
	repo.active[1] = &OpsAlertEvent{ID: 10, EmailSent: true}
	require.False(t, svc.coveredByEarlierAlert(context.Background(), rules, p0, map[int64]struct{}{8: {}}))

	// A dashboard-only rule (no email) covers nothing.
	p0.NotifyEmail = false
	require.False(t, svc.coveredByEarlierAlert(context.Background(), rules, p1, map[int64]struct{}{1: {}}))
}

func TestDiagnoseAlert_BlockedClientRowsDoNotClaimAllClient(t *testing.T) {
	t.Parallel()

	repo := &stubOpsRepoWithBreakdown{rows: []*OpsAlertErrorBreakdown{
		{Count: 3, Owner: "provider", StatusCode: 502, UpstreamStatusCode: 401, AccountID: int64Ptr(43), Message: "token has expired"},
		{Count: 50, Owner: "client", StatusCode: 403, Message: "insufficient balance", BusinessLimited: true},
	}}
	svc := &OpsAlertEvaluatorService{opsRepo: repo}
	rule := &OpsAlertRule{Name: "错误率过高", Severity: "P1", MetricType: "error_rate", Operator: ">", Threshold: 5}
	now := time.Now()

	d := svc.diagnoseAlert(context.Background(), rule, 30, opsAlertSample{Requests: 10, Errors: 3}, now.Add(-5*time.Minute), now, "", nil, now)
	require.NotContains(t, d.Summary, "客户端自身问题")
	require.Equal(t, "账号 #43 登录凭证失效", d.Headline)
}
