package service

import (
	"context"
	"strings"
)

// CompositePlatformAvailability reports whether some account of platform in
// the group can take model right now (status, rate limits and model-level
// limits included).
type CompositePlatformAvailability func(ctx context.Context, groupID int64, platform, model string) (bool, error)

// SetPlatformAvailability wires the live availability check used by the
// Argon fallback.
func (r *CompositeRouteResolver) SetPlatformAvailability(fn CompositePlatformAvailability) {
	if r != nil {
		r.platformAvailability = fn
	}
}

// ApplyArgonGeminiFallback moves a Gemini 4 Argon request from Antigravity to
// the Gemini platform (AI Studio / web accounts) when no Antigravity account
// in the group can serve it right now, e.g. its Argon quota is rate limited.
// A composite group routes one request to one platform, so without this the
// request fails with "no available accounts" although Gemini accounts expose
// the same model. Any lookup error keeps the original decision.
func (r *CompositeRouteResolver) ApplyArgonGeminiFallback(ctx context.Context, decision CompositeRouteDecision) CompositeRouteDecision {
	if r == nil || r.platformAvailability == nil || !decision.Matched ||
		decision.TargetPlatform != PlatformAntigravity || decision.GroupID <= 0 {
		return decision
	}
	upstream := strings.TrimSpace(decision.UpstreamModel)
	if upstream == "" {
		upstream = strings.TrimSpace(decision.PublicModel)
	}
	if !isAntigravityGeminiArgonModel(upstream) || strings.Contains(upstream, "cyber") {
		return decision
	}
	agOK, err := r.platformAvailability(ctx, decision.GroupID, PlatformAntigravity, upstream)
	if err != nil || agOK {
		return decision
	}
	gemOK, err := r.platformAvailability(ctx, decision.GroupID, PlatformGemini, upstream)
	if err != nil || !gemOK {
		return decision
	}
	decision.TargetPlatform = PlatformGemini
	decision.UpstreamModel = upstream
	decision.Route = nil
	return decision
}

func (s *GatewayService) compositePlatformAvailableNow(ctx context.Context, groupID int64, platform, model string) (bool, error) {
	if s == nil || s.accountRepo == nil || groupID <= 0 {
		return true, nil
	}
	accounts, err := s.accountRepo.ListModelAvailabilityCandidates(ctx, &groupID, []string{platform}, false)
	if err != nil {
		return false, err
	}
	for i := range accounts {
		acc := &accounts[i]
		if acc.Platform != platform || !acc.IsSchedulable() {
			continue
		}
		if !s.isModelSupportedByAccountWithContext(ctx, acc, model) {
			continue
		}
		if acc.isModelRateLimitedWithContext(ctx, model) {
			continue
		}
		return true, nil
	}
	return false, nil
}
