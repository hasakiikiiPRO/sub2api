package service

import (
	"context"
	"fmt"
	"html"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

// Ops alert diagnosis turns a fired rule into something an operator can act on:
// which account/model failed, what the upstream said, what it means in plain
// words, and what to do. The raw metric ("error_rate > 14.29") alone is not
// actionable on a low-traffic relay.

const (
	// Rate rules on a quiet relay fire on a single failed request (1 of 7 = 14%).
	// Require a minimum number of failed requests unless the rule overrides it
	// via filters.min_errors (0 disables the floor).
	opsAlertDefaultMinErrors = 3

	opsAlertBreakdownLimit = 5
	opsAlertAccountLimit   = 8
)

// OpsAlertErrorBreakdown is one (account, model, status, message) bucket of the
// errors seen in an alert window.
type OpsAlertErrorBreakdown struct {
	Count              int64
	Owner              string
	Phase              string
	Platform           string
	Model              string
	StatusCode         int
	UpstreamStatusCode int
	AccountID          *int64
	AccountName        string
	Message            string
	UserCount          int64
	UserEmails         []string
	BusinessLimited    bool
	LastSeenAt         time.Time
}

// opsAlertErrorBreakdownReader is implemented by the SQL ops repository. It is
// kept out of OpsRepository so existing test doubles don't need a new method.
type opsAlertErrorBreakdownReader interface {
	GetAlertErrorBreakdown(ctx context.Context, filter *OpsDashboardFilter, upstreamOnly bool, limit int) ([]*OpsAlertErrorBreakdown, error)
}

// opsAlertSample carries the request/error counts behind a rate metric.
type opsAlertSample struct {
	Requests int64
	Errors   int64
}

type opsAlertCause struct {
	Count   int64
	Where   string
	Short   string
	Meaning string
	Action  string
	Message string
	Users   string
	// Client marks failures caused by the caller (bad request, no balance).
	Client bool
	// Blocked marks business-limited rows, which the error rate does not count.
	Blocked bool
}

type opsAlertAccountLine struct {
	Name   string
	State  string
	Detail string
}

type opsAlertDiagnosis struct {
	// Headline is a one-line cause, used in the subject.
	Headline string
	// Summary restates the metric in plain words.
	Summary string

	Causes []opsAlertCause

	Accounts          []opsAlertAccountLine
	AccountsTitle     string
	AccountsAvailable int64
	AccountsTotal     int64
}

type opsAlertFired struct {
	rule      *OpsAlertRule
	event     *OpsAlertEvent
	diagnosis *opsAlertDiagnosis
}

func isOpsAlertRequestMetric(metricType string) bool {
	switch strings.TrimSpace(metricType) {
	case "error_rate", "success_rate", "upstream_error_rate":
		return true
	}
	return false
}

// opsAlertMetricFamily groups metrics that describe the same symptom so one
// incident does not produce one email per rule (success_rate is 100-error_rate).
func opsAlertMetricFamily(metricType string) string {
	switch m := strings.TrimSpace(metricType); m {
	case "success_rate":
		return "error_rate"
	default:
		return m
	}
}

func opsAlertSeverityRank(severity string) int {
	switch strings.ToUpper(strings.TrimSpace(severity)) {
	case "P0":
		return 3
	case "P1":
		return 2
	case "P2":
		return 1
	default:
		return 0
	}
}

func opsAlertFilterInt(filters map[string]any, key string) (int64, bool) {
	if filters == nil {
		return 0, false
	}
	v, ok := filters[key]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case string:
		var n int64
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// opsAlertSampleSufficient reports whether a rate rule has enough traffic behind
// it to be trusted. Non-rate rules are always sufficient.
func opsAlertSampleSufficient(rule *OpsAlertRule, sample opsAlertSample) bool {
	if rule == nil || !isOpsAlertRequestMetric(rule.MetricType) {
		return true
	}
	minErrors := int64(opsAlertDefaultMinErrors)
	if v, ok := opsAlertFilterInt(rule.Filters, "min_errors"); ok {
		minErrors = v
	}
	minRequests, _ := opsAlertFilterInt(rule.Filters, "min_requests")
	return sample.Errors >= minErrors && sample.Requests >= minRequests
}

func opsAlertMetricLabel(metricType string) string {
	switch strings.TrimSpace(metricType) {
	case "error_rate":
		return "错误率"
	case "success_rate":
		return "成功率"
	case "upstream_error_rate":
		return "上游错误率"
	case "cpu_usage_percent":
		return "CPU 使用率"
	case "memory_usage_percent":
		return "内存使用率"
	case "concurrency_queue_depth":
		return "排队请求数"
	case "group_available_accounts":
		return "分组可用账号数"
	case "group_available_ratio":
		return "分组可用账号占比"
	case "group_rate_limit_ratio":
		return "分组限流账号占比"
	case "account_rate_limited_count":
		return "限流账号数"
	case "account_error_count":
		return "出错账号数"
	case "account_error_ratio":
		return "出错账号占比"
	case "account_temp_unscheduled_count":
		return "临时冷却账号数"
	case "overload_account_count":
		return "过载账号数"
	case "proxy_expired_count":
		return "已过期代理数"
	case "proxy_expiring_soon_count":
		return "即将过期代理数"
	default:
		return strings.TrimSpace(metricType)
	}
}

func opsAlertIsPercentMetric(metricType string) bool {
	switch strings.TrimSpace(metricType) {
	case "error_rate", "success_rate", "upstream_error_rate", "cpu_usage_percent", "memory_usage_percent",
		"group_available_ratio", "group_rate_limit_ratio", "account_error_ratio":
		return true
	}
	return false
}

func formatOpsAlertNumber(metricType string, v float64) string {
	if opsAlertIsPercentMetric(metricType) {
		// Two decimals so SLA-style thresholds (99.95) stay distinguishable.
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".") + "%"
	}
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.2f", v)
}

// formatOpsAlertTime renders a timestamp in the server's configured timezone
// (Asia/Shanghai by default) instead of UTC.
func formatOpsAlertTime(t time.Time, layout string) string {
	if t.IsZero() {
		return "-"
	}
	return t.In(timezone.Location()).Format(layout)
}

func formatOpsAlertUntil(until time.Time, now time.Time) string {
	s := formatOpsAlertTime(until, "01-02 15:04")
	d := until.Sub(now)
	switch {
	case d <= 0:
		return s
	case d < time.Hour:
		return fmt.Sprintf("%s（约 %d 分钟后）", s, int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("%s（约 %.0f 小时后）", s, d.Hours())
	default:
		return fmt.Sprintf("%s（约 %.0f 天后）", s, d.Hours()/24)
	}
}

// explainOpsAlertError maps a raw error bucket to (short label, meaning, action).
func explainOpsAlertError(b *OpsAlertErrorBreakdown) (short, meaning, action string) {
	msg := strings.ToLower(b.Message)
	status := b.StatusCode
	if b.UpstreamStatusCode > 0 {
		status = b.UpstreamStatusCode
	}
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(msg, s) {
				return true
			}
		}
		return false
	}

	authStatus := status == 400 || status == 401 || status == 403
	switch {
	case status == 429 || status == 529:
		if status == 529 {
			return "上游过载",
				"上游服务商过载",
				"上游的问题，一般几分钟内恢复"
		}
		return "上游限流/额度用完",
			"上游账号触发限流或额度用完",
			"通常会自动恢复；经常出现说明号不够用，考虑加号"
	case has("token has expired", "token has been revoked", "invalid_grant") ||
		(authStatus && has("token expired", "refresh token", "refresh_token")) ||
		(b.Owner == "provider" && status == 401):
		return "登录凭证失效",
			"上游账号的登录令牌过期或被吊销，这个号暂时用不了",
			"后台「账号管理」找到该账号点「刷新令牌」；刷新失败就重新授权登录"
	case has("insufficient balance", "insufficient account balance", "余额不足"):
		return "用户余额不足",
			"用户余额不足，请求被拒（客户自己的问题，不是站点故障）",
			"无需处理；需要时提醒该用户充值"
	case has("websocket upgrade required"):
		return "客户端 WebSocket 探测",
			"客户端（如 Codex CLI）先试 WebSocket 连接被拒，会自动改用 HTTP",
			"无需处理，属于正常探测"
	case has("冷却中", "cooling down"):
		return "账号模型冷却中",
			"该账号这个模型最近连续失败，系统自动暂停它几分钟",
			"一般会自动恢复；反复出现就检查该账号"
	case has("no available", "service temporarily unavailable") && (b.Phase == "routing" || b.AccountID == nil):
		return "无可用账号",
			"这个模型/平台当前没有能调度的账号（全在限流、冷却、停用或出错），请求直接被拒",
			"看「账号状态」：等限流恢复、修好出错的号，或补充新账号"
	case status == 429 || has("rate limit", "rate_limit", "quota", "usage limit", "too many requests", "限额", "限流"):
		return "上游限流/额度用完",
			"上游账号触发限流或额度用完",
			"通常会自动恢复；经常出现说明号不够用，考虑加号"
	case status == 529 || has("overloaded"):
		return "上游过载",
			"上游服务商过载",
			"上游的问题，一般几分钟内恢复"
	case status == 499 || status == 504 || has("timeout", "deadline exceeded"):
		return "上游超时",
			"等待上游响应超时",
			"偶发可忽略；频繁出现检查网络或上游"
	case has("stream closed before", "unexpected eof"):
		return "上游流中断",
			"上游流式输出中途断开",
			"偶发可忽略；集中在某个号时检查该号"
	case status == 404 || has("not supported", "model_not_found", "unknown provider"):
		return "模型不支持",
			"请求了本站没有配置的模型",
			"客户端模型名写错，或需要在分组里补模型映射"
	case b.Owner == "client":
		return "客户端请求错误",
			"请求本身有问题（参数/格式等，客户端问题）",
			"一般无需处理"
	case b.Owner == "provider":
		return "上游报错",
			"上游服务商返回错误",
			"看原文判断；集中在某个号时检查该号"
	case b.Owner == "platform":
		return "本站内部错误",
			"本站调度或内部处理出错",
			"到运维看板看错误详情，必要时查容器日志"
	default:
		return "未归类错误",
			"未能自动归类的错误",
			"到运维看板看错误详情"
	}
}

func opsAlertCauseFromBreakdown(b *OpsAlertErrorBreakdown) opsAlertCause {
	short, meaning, action := explainOpsAlertError(b)

	where := make([]string, 0, 4)
	if b.AccountID != nil && *b.AccountID > 0 {
		name := strings.TrimSpace(b.AccountName)
		if name != "" {
			where = append(where, fmt.Sprintf("账号 #%d %s", *b.AccountID, name))
		} else {
			where = append(where, fmt.Sprintf("账号 #%d", *b.AccountID))
		}
	}
	if p := strings.TrimSpace(b.Platform); p != "" && p != "composite" {
		where = append(where, p)
	}
	if m := strings.TrimSpace(b.Model); m != "" {
		where = append(where, m)
	}
	if b.UpstreamStatusCode > 0 && b.UpstreamStatusCode != b.StatusCode {
		where = append(where, fmt.Sprintf("HTTP %d（上游 %d）", b.StatusCode, b.UpstreamStatusCode))
	} else if b.StatusCode > 0 {
		where = append(where, fmt.Sprintf("HTTP %d", b.StatusCode))
	}

	users := ""
	if b.UserCount > 0 {
		if len(b.UserEmails) > 0 {
			users = "用户 " + strings.Join(b.UserEmails, "、")
			if b.UserCount > int64(len(b.UserEmails)) {
				users += fmt.Sprintf(" 等 %d 人", b.UserCount)
			}
		} else {
			users = fmt.Sprintf("%d 个用户", b.UserCount)
		}
	}

	return opsAlertCause{
		Count:   b.Count,
		Where:   strings.Join(where, " · "),
		Short:   short,
		Meaning: meaning,
		Action:  action,
		Message: strings.TrimSpace(b.Message),
		Users:   users,
		Client:  b.Owner == "client" || short == "用户余额不足",
		Blocked: b.BusinessLimited,
	}
}

func opsAlertAccountLineFor(acc *AccountAvailability, now time.Time) (opsAlertAccountLine, bool) {
	if acc == nil {
		return opsAlertAccountLine{}, false
	}
	name := fmt.Sprintf("#%d %s", acc.AccountID, strings.TrimSpace(acc.AccountName))
	line := opsAlertAccountLine{Name: strings.TrimSpace(name)}
	switch {
	case acc.HasError:
		line.State = "出错，已停止调度"
		line.Detail = strings.TrimSpace(acc.ErrorMessage)
	case acc.Status != "" && acc.Status != StatusActive:
		line.State = "已停用（状态 " + acc.Status + "）"
	case !acc.Schedulable:
		line.State = "已手动暂停调度"
	case acc.TempUnschedulableUntil != nil && now.Before(*acc.TempUnschedulableUntil):
		line.State = "临时冷却至 " + formatOpsAlertUntil(*acc.TempUnschedulableUntil, now)
		line.Detail = strings.TrimSpace(acc.TempUnschedulableReason)
	case acc.IsRateLimited && acc.RateLimitResetAt != nil:
		line.State = "限流中，" + formatOpsAlertUntil(*acc.RateLimitResetAt, now) + " 恢复"
	case acc.IsRateLimited:
		line.State = "限流中"
	case acc.IsOverloaded && acc.OverloadUntil != nil:
		line.State = "过载至 " + formatOpsAlertUntil(*acc.OverloadUntil, now)
	case acc.IsAvailable:
		return opsAlertAccountLine{}, false
	default:
		line.State = "不可用"
	}
	return line, true
}

// opsAlertAccountRecovery returns when an unavailable account is expected back
// on its own (rate limit / cooldown), or nil if it needs a human.
func opsAlertAccountRecovery(acc *AccountAvailability, now time.Time) *time.Time {
	if acc == nil || acc.HasError || !acc.Schedulable || (acc.Status != "" && acc.Status != StatusActive) {
		return nil
	}
	var at *time.Time
	pick := func(t *time.Time) {
		if t != nil && now.Before(*t) && (at == nil || t.After(*at)) {
			at = t
		}
	}
	pick(acc.TempUnschedulableUntil)
	if acc.IsRateLimited {
		pick(acc.RateLimitResetAt)
	}
	if acc.IsOverloaded {
		pick(acc.OverloadUntil)
	}
	return at
}

func (s *OpsAlertEvaluatorService) diagnoseAccounts(ctx context.Context, d *opsAlertDiagnosis, rule *OpsAlertRule, platform string, groupID *int64, now time.Time) {
	if s == nil || s.opsService == nil {
		return
	}
	availability, err := s.opsService.GetAccountAvailability(ctx, platform, groupID)
	if err != nil || availability == nil {
		return
	}

	onlyErrors := strings.TrimSpace(rule.MetricType) == "account_error_count" || strings.TrimSpace(rule.MetricType) == "account_error_ratio"

	ids := make([]int64, 0, len(availability.Accounts))
	for id := range availability.Accounts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var (
		rateLimited, cooling, broken, paused, overloaded int
		earliest                                         *time.Time
	)
	for _, id := range ids {
		acc := availability.Accounts[id]
		if acc == nil {
			continue
		}
		d.AccountsTotal++
		if acc.IsAvailable {
			d.AccountsAvailable++
		}
		if onlyErrors && !(acc.HasError && acc.TempUnschedulableUntil == nil) {
			continue
		}
		line, ok := opsAlertAccountLineFor(acc, now)
		if !ok {
			continue
		}
		switch {
		case acc.HasError:
			broken++
		case acc.Status != "" && acc.Status != StatusActive, !acc.Schedulable:
			paused++
		case acc.TempUnschedulableUntil != nil && now.Before(*acc.TempUnschedulableUntil):
			cooling++
		case acc.IsRateLimited:
			rateLimited++
		case acc.IsOverloaded:
			overloaded++
		}
		if rec := opsAlertAccountRecovery(acc, now); rec != nil && (earliest == nil || rec.Before(*earliest)) {
			earliest = rec
		}
		if len(d.Accounts) < opsAlertAccountLimit {
			d.Accounts = append(d.Accounts, line)
		}
	}

	if onlyErrors {
		d.AccountsTitle = "出错的账号"
	} else {
		d.AccountsTitle = fmt.Sprintf("账号状态（可用 %d / 共 %d）", d.AccountsAvailable, d.AccountsTotal)
	}

	parts := make([]string, 0, 4)
	if rateLimited > 0 {
		parts = append(parts, fmt.Sprintf("%d 个限流", rateLimited))
	}
	if cooling > 0 {
		parts = append(parts, fmt.Sprintf("%d 个临时冷却", cooling))
	}
	if broken > 0 {
		parts = append(parts, fmt.Sprintf("%d 个出错", broken))
	}
	if paused > 0 {
		parts = append(parts, fmt.Sprintf("%d 个停用", paused))
	}
	if overloaded > 0 {
		parts = append(parts, fmt.Sprintf("%d 个过载", overloaded))
	}
	if len(parts) > 0 && d.Headline == "" {
		d.Headline = strings.Join(parts, "、")
		if earliest != nil {
			d.Headline += "，最早 " + formatOpsAlertTime(*earliest, "01-02 15:04") + " 自动恢复"
		} else if broken > 0 || paused > 0 {
			d.Headline += "，需要人工处理"
		}
	}
}

// diagnoseAlert gathers the context for a fired rule. It is best-effort: any
// lookup failure just leaves that part of the diagnosis empty.
func (s *OpsAlertEvaluatorService) diagnoseAlert(
	ctx context.Context,
	rule *OpsAlertRule,
	metricValue float64,
	sample opsAlertSample,
	windowStart, windowEnd time.Time,
	platform string,
	groupID *int64,
	now time.Time,
) *opsAlertDiagnosis {
	d := &opsAlertDiagnosis{}
	if rule == nil {
		return d
	}
	metric := strings.TrimSpace(rule.MetricType)
	label := opsAlertMetricLabel(metric)
	windowMinutes := int(windowEnd.Sub(windowStart).Minutes())
	if windowMinutes <= 0 {
		windowMinutes = 1
	}
	threshold := fmt.Sprintf("%s %s", strings.TrimSpace(rule.Operator), formatOpsAlertNumber(metric, rule.Threshold))

	if isOpsAlertRequestMetric(metric) {
		d.Summary = fmt.Sprintf("过去 %d 分钟共 %d 个请求，失败 %d 个，%s %s（告警线 %s）。",
			windowMinutes, sample.Requests, sample.Errors, label, formatOpsAlertNumber(metric, metricValue), threshold)

		if reader, ok := s.opsRepo.(opsAlertErrorBreakdownReader); ok && reader != nil {
			rows, err := reader.GetAlertErrorBreakdown(ctx, &OpsDashboardFilter{
				StartTime: windowStart,
				EndTime:   windowEnd,
				Platform:  platform,
				GroupID:   groupID,
			}, metric == "upstream_error_rate", opsAlertBreakdownLimit)
			if err == nil {
				// Only rows the metric counts (not business-limited) feed the client share.
				var clientErrors int64
				for _, row := range rows {
					if row == nil {
						continue
					}
					cause := opsAlertCauseFromBreakdown(row)
					if cause.Client && !cause.Blocked {
						clientErrors += cause.Count
					}
					d.Causes = append(d.Causes, cause)
				}
				if clientErrors > 0 && clientErrors >= sample.Errors {
					d.Summary += "计入错误率的失败全是客户端自身问题，不代表站点故障。"
				} else if clientErrors > 0 {
					d.Summary += fmt.Sprintf("其中 %d 个是客户端自身问题。", clientErrors)
				}
			}
		}
		// Prefer a site-side cause for the headline; client-side noise is rarely the story.
		for _, c := range d.Causes {
			if !c.Client {
				d.Headline = opsAlertCauseHeadline(c)
				break
			}
		}
		if d.Headline == "" && len(d.Causes) > 0 {
			d.Headline = opsAlertCauseHeadline(d.Causes[0])
		}
		return d
	}

	switch metric {
	case "group_available_accounts":
		d.Summary = fmt.Sprintf("这个分组当前可用账号 %s 个（告警线 %s），走这个分组的请求会直接失败。",
			formatOpsAlertNumber(metric, metricValue), threshold)
	case "account_error_count":
		d.Summary = fmt.Sprintf("有 %s 个账号处于错误状态，系统已停止调度它们。", formatOpsAlertNumber(metric, metricValue))
	case "cpu_usage_percent", "memory_usage_percent", "concurrency_queue_depth":
		d.Summary = fmt.Sprintf("服务器%s当前 %s（告警线 %s）。", label, formatOpsAlertNumber(metric, metricValue), threshold)
	default:
		d.Summary = fmt.Sprintf("%s当前 %s（告警线 %s）。", label, formatOpsAlertNumber(metric, metricValue), threshold)
	}

	switch metric {
	case "group_available_accounts", "group_available_ratio", "group_rate_limit_ratio",
		"account_rate_limited_count", "account_error_count", "account_error_ratio",
		"account_temp_unscheduled_count", "overload_account_count":
		s.diagnoseAccounts(ctx, d, rule, platform, groupID, now)
	}
	return d
}

func opsAlertCauseHeadline(c opsAlertCause) string {
	where := c.Where
	// Keep the subject short: account (or model) + label.
	if i := strings.Index(where, " · "); i > 0 && strings.HasPrefix(where, "账号 ") {
		where = where[:i]
	} else if parts := strings.Split(where, " · "); len(parts) > 1 {
		where = strings.Join(parts[:len(parts)-1], " · ")
	}
	if where == "" {
		return c.Short
	}
	return where + " " + c.Short
}

func opsAlertSeverityColor(severity string) string {
	switch strings.ToUpper(strings.TrimSpace(severity)) {
	case "P0":
		return "#dc2626"
	case "P1":
		return "#ea580c"
	default:
		return "#ca8a04"
	}
}

// buildOpsAlertDetailsHTML renders the per-alert diagnosis blocks. Every dynamic
// value is escaped here because the result is injected as raw HTML.
func buildOpsAlertDetailsHTML(items []opsAlertFired, dashboardURL string) string {
	esc := html.EscapeString
	var b strings.Builder
	for _, it := range items {
		if it.rule == nil || it.event == nil {
			continue
		}
		d := it.diagnosis
		if d == nil {
			d = &opsAlertDiagnosis{}
		}
		fmt.Fprintf(&b, `<div style="border-left:4px solid %s;background:#fafafa;padding:10px 14px;margin:14px 0;border-radius:4px;">`,
			opsAlertSeverityColor(it.rule.Severity))
		title := strings.TrimSpace(it.rule.Name)
		if d.Headline != "" {
			title += "：" + d.Headline
		}
		fmt.Fprintf(&b, `<div style="font-weight:600;">[%s] %s</div>`, esc(strings.TrimSpace(it.rule.Severity)), esc(title))
		if d.Summary != "" {
			fmt.Fprintf(&b, `<div style="margin-top:4px;">%s</div>`, esc(d.Summary))
		}
		if len(d.Causes) > 0 {
			b.WriteString(`<div style="margin-top:10px;font-weight:600;">失败明细（按次数，含余额不足、无可用账号等被拦截的请求）</div>`)
			for _, c := range d.Causes {
				b.WriteString(`<div style="margin-top:8px;padding-top:6px;border-top:1px solid #e4e4e7;">`)
				fmt.Fprintf(&b, `<div><b>%d 次</b> · %s</div>`, c.Count, esc(c.Where))
				fmt.Fprintf(&b, `<div>原因：%s</div>`, esc(c.Meaning))
				fmt.Fprintf(&b, `<div>建议：%s</div>`, esc(c.Action))
				extra := make([]string, 0, 2)
				if c.Message != "" {
					extra = append(extra, "原文："+c.Message)
				}
				if c.Users != "" {
					extra = append(extra, c.Users)
				}
				if len(extra) > 0 {
					fmt.Fprintf(&b, `<div style="color:#71717a;font-size:13px;overflow-wrap:anywhere;word-break:break-word;">%s</div>`, esc(strings.Join(extra, "｜")))
				}
				b.WriteString(`</div>`)
			}
		}
		if d.AccountsTitle != "" {
			fmt.Fprintf(&b, `<div style="margin-top:10px;font-weight:600;">%s</div>`, esc(d.AccountsTitle))
			if len(d.Accounts) == 0 {
				b.WriteString(`<div style="color:#71717a;">（没有不可用的账号）</div>`)
			}
			b.WriteString(`<ul style="margin:4px 0 0 0;padding-left:20px;">`)
			for _, a := range d.Accounts {
				fmt.Fprintf(&b, `<li>%s：%s`, esc(a.Name), esc(a.State))
				if a.Detail != "" {
					fmt.Fprintf(&b, `<div style="color:#71717a;font-size:13px;overflow-wrap:anywhere;word-break:break-word;">%s</div>`, esc(a.Detail))
				}
				b.WriteString(`</li>`)
			}
			b.WriteString(`</ul>`)
		}
		b.WriteString(`</div>`)
	}
	if dashboardURL != "" && isSafeNotificationEmailURL(dashboardURL) {
		fmt.Fprintf(&b, `<p><a href="%s" style="display:inline-block;padding:10px 16px;border-radius:8px;background:#ea580c;color:#ffffff;text-decoration:none;font-weight:600;">打开运维看板</a></p>`,
			esc(dashboardURL))
	}
	return b.String()
}
