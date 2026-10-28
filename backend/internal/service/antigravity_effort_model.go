package service

import (
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/tidwall/gjson"
)

// ResolveAntigravityEffortModel runs after provider routing but before scheduling.
// Only the new public AG aliases are dynamic: native Claude IDs, concrete variants,
// account-owned aliases and explicitly pinned route targets keep their semantics.
func ResolveAntigravityEffortModel(decision CompositeRouteDecision, body []byte) (CompositeRouteDecision, error) {
	if !decision.Matched || decision.TargetPlatform != PlatformAntigravity || decision.Source == CompositeRouteSourceAccount {
		return decision, nil
	}
	family := antigravityEffortModelFamily(decision.PublicModel)
	if family == "" {
		return decision, nil
	}
	upstream := strings.TrimSpace(decision.UpstreamModel)
	if upstream != "" && upstream != decision.PublicModel && upstream != family {
		return decision, nil
	}

	var effort gjson.Result
	switch decision.Endpoint {
	case CompositeRouteEndpointMessages, CompositeRouteEndpointCountTokens:
		effort = gjson.GetBytes(body, "output_config.effort")
	case CompositeRouteEndpointResponses:
		effort = gjson.GetBytes(body, "reasoning.effort")
	case CompositeRouteEndpointChatCompletions:
		effort = gjson.GetBytes(body, "reasoning_effort")
	case CompositeRouteEndpointGemini:
		effort = gjson.GetBytes(body, "generationConfig.thinkingConfig.thinkingLevel")
	default:
		return decision, nil
	}
	level := "high"
	if effort.Exists() {
		if effort.Type != gjson.String {
			return decision, fmt.Errorf("Antigravity effort must be low, medium, high, xhigh or max")
		}
		switch strings.ToLower(strings.TrimSpace(effort.String())) {
		case "low":
			level = "low"
		case "medium":
			level = "medium"
		case "high", "xhigh", "max":
			level = "high"
		default:
			return decision, fmt.Errorf("unsupported Antigravity effort %q: use low, medium, high, xhigh or max", effort.String())
		}
	} else if decision.Endpoint == CompositeRouteEndpointGemini {
		level = geminiThinkingLevelFromBody(body)
	} else if thinking := gjson.GetBytes(body, "thinking"); thinking.IsObject() {
		level = geminiThinkingLevelFromClaudeThinking(&antigravity.ThinkingConfig{
			Type:         thinking.Get("type").String(),
			BudgetTokens: int(thinking.Get("budget_tokens").Int()),
		})
	}
	decision.UpstreamModel = family + "-" + level
	return decision, nil
}

func antigravityEffortModelFamily(model string) string {
	switch model {
	case "claude-opus-5-5-antigravity", "ag-claude-opus-5-5":
		return "claude-opus-5-5"
	case "claude-sonnet-5-5-antigravity", "ag-claude-sonnet-5-5":
		return "claude-sonnet-5-5"
	case "gemini-4-argon":
		return "gemini-4-argon"
	default:
		return ""
	}
}
