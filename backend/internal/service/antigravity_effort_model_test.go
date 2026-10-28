package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResolveAntigravityEffortModelProtocols(t *testing.T) {
	families := map[string]string{
		"claude-opus-5-5-antigravity":   "claude-opus-5-5",
		"claude-sonnet-5-5-antigravity": "claude-sonnet-5-5",
		"gemini-4-argon":                "gemini-4-argon",
		"ag-claude-opus-5-5":            "claude-opus-5-5",
		"ag-claude-sonnet-5-5":          "claude-sonnet-5-5",
	}
	protocols := map[string]string{
		CompositeRouteEndpointMessages:        `{"output_config":{"effort":%q}}`,
		CompositeRouteEndpointCountTokens:     `{"output_config":{"effort":%q}}`,
		CompositeRouteEndpointResponses:       `{"reasoning":{"effort":%q}}`,
		CompositeRouteEndpointChatCompletions: `{"reasoning_effort":%q}`,
		CompositeRouteEndpointGemini:          `{"generationConfig":{"thinkingConfig":{"thinkingLevel":%q}}}`,
	}
	for alias, family := range families {
		for protocol, template := range protocols {
			for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
				t.Run(alias+"/"+protocol+"/"+effort, func(t *testing.T) {
					decision := CompositeRouteDecision{Matched: true, TargetPlatform: PlatformAntigravity,
						PublicModel: alias, UpstreamModel: family, Endpoint: protocol, Source: CompositeRouteSourceExplicit}
					body := []byte(fmt.Sprintf(template, effort))
					if effort == "" {
						body = []byte(`{}`)
					}
					got, err := ResolveAntigravityEffortModel(decision, body)
					require.NoError(t, err)
					level := effort
					if level == "" || level == "xhigh" || level == "max" {
						level = "high"
					}
					require.Equal(t, family+"-"+level, got.UpstreamModel)
					require.Equal(t, alias, got.PublicModel)
				})
			}
		}
	}
}

func TestResolveAntigravityEffortModelFallbackAndPrecedence(t *testing.T) {
	base := CompositeRouteDecision{Matched: true, TargetPlatform: PlatformAntigravity,
		PublicModel: "claude-opus-5-5-antigravity", UpstreamModel: "claude-opus-5-5", Endpoint: CompositeRouteEndpointMessages}
	for _, tc := range []struct {
		name, body, level string
	}{
		{"default", `{}`, "high"},
		{"budget low", `{"thinking":{"type":"enabled","budget_tokens":1024}}`, "low"},
		{"budget medium", `{"thinking":{"type":"enabled","budget_tokens":8192}}`, "medium"},
		{"budget high", `{"thinking":{"type":"enabled","budget_tokens":9000}}`, "high"},
		{"disabled", `{"thinking":{"type":"disabled"}}`, "low"},
		{"effort first", `{"output_config":{"effort":"medium"},"thinking":{"type":"enabled","budget_tokens":1024}}`, "medium"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveAntigravityEffortModel(base, []byte(tc.body))
			require.NoError(t, err)
			require.Equal(t, "claude-opus-5-5-"+tc.level, got.UpstreamModel)
		})
	}
	gemini := base
	gemini.Endpoint = CompositeRouteEndpointGemini
	gemini.PublicModel, gemini.UpstreamModel = "gemini-4-argon", "gemini-4-argon"
	for _, tc := range []struct {
		budget int
		level  string
	}{{-1, "high"}, {0, "low"}, {1024, "low"}, {1025, "medium"}, {8192, "medium"}, {8193, "high"}} {
		got, err := ResolveAntigravityEffortModel(gemini, []byte(fmt.Sprintf(`{"generationConfig":{"thinkingConfig":{"thinkingBudget":%d}}}`, tc.budget)))
		require.NoError(t, err)
		require.Equal(t, "gemini-4-argon-"+tc.level, got.UpstreamModel)
	}
	for _, value := range []string{`"minimal"`, `"none"`, `"urgent"`, `""`, `42`, `null`, `true`} {
		_, err := ResolveAntigravityEffortModel(base, []byte(`{"output_config":{"effort":`+value+`}}`))
		require.Error(t, err, value)
	}

	for _, tc := range []struct {
		name   string
		change func(*CompositeRouteDecision)
	}{
		{"native Claude", func(d *CompositeRouteDecision) { d.PublicModel = "claude-opus-5-5" }},
		{"other native Claude", func(d *CompositeRouteDecision) { d.PublicModel = "claude-sonnet-5-5" }},
		{"concrete suffix", func(d *CompositeRouteDecision) {
			d.PublicModel = "claude-opus-5-5-low"
			d.UpstreamModel = d.PublicModel
		}},
		{"pinned suffix", func(d *CompositeRouteDecision) { d.UpstreamModel = "claude-opus-5-5-low" }},
		{"pinned other family", func(d *CompositeRouteDecision) { d.UpstreamModel = "claude-sonnet-5-5" }},
		{"registered custom alias", func(d *CompositeRouteDecision) { d.PublicModel = "newclaudealias" }},
		{"non AG provider", func(d *CompositeRouteDecision) { d.TargetPlatform = PlatformAnthropic }},
		{"account alias", func(d *CompositeRouteDecision) { d.Source = CompositeRouteSourceAccount }},
		{"unmatched", func(d *CompositeRouteDecision) { d.Matched = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := base
			tc.change(&decision)
			got, err := ResolveAntigravityEffortModel(decision, []byte(`{"output_config":{"effort":"invalid"}}`))
			require.NoError(t, err)
			require.Equal(t, decision, got)
		})
	}
}

func TestAntigravityEffortModelSchedulerMappingAndCooldown(t *testing.T) {
	decision, err := ResolveAntigravityEffortModel(CompositeRouteDecision{
		Matched: true, TargetPlatform: PlatformAntigravity, PublicModel: "claude-opus-5-5-antigravity",
		UpstreamModel: "claude-opus-5-5", Endpoint: CompositeRouteEndpointMessages,
	}, []byte(`{"output_config":{"effort":"medium"}}`))
	require.NoError(t, err)
	ctx := WithCompositeRouteDecision(context.Background(), decision)
	svc := &GatewayService{}
	account := &Account{Platform: PlatformAntigravity}
	require.True(t, svc.isModelSupportedByAccountWithContext(ctx, account, decision.UpstreamModel))
	require.Equal(t, "claude-opus-5-5-medium", (&AntigravityGatewayService{}).getMappedModel(account, decision.UpstreamModel))
	require.Equal(t, []string{"claude-opus-5-5-medium"}, account.modelRateLimitKeysForRequest(ctx, decision.UpstreamModel))
	account.Extra = map[string]any{"model_rate_limits": map[string]any{
		"claude-opus-5-5-high": map[string]any{"rate_limit_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)},
	}}
	require.False(t, account.isModelRateLimitedWithContext(ctx, decision.UpstreamModel))
	for _, target := range []string{"claude-sonnet-5-5-low", "claude-opus-5-5-high", "gemini-4-argon-high"} {
		account := newAntigravityAccountWithMapping(map[string]string{decision.PublicModel: target, decision.UpstreamModel: decision.UpstreamModel})
		require.False(t, svc.isModelSupportedByAccountWithContext(ctx, account, decision.UpstreamModel), target)
	}
	account = newAntigravityAccountWithMapping(map[string]string{decision.PublicModel: "claude-opus-5-5", decision.UpstreamModel: decision.UpstreamModel})
	require.True(t, svc.isModelSupportedByAccountWithContext(ctx, account, decision.UpstreamModel))
}
