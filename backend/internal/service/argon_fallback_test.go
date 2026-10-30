package service

import (
	"context"
	"testing"
)

func TestApplyArgonGeminiFallback(t *testing.T) {
	avail := map[string]bool{}
	r := NewCompositeRouteResolver(nil)
	r.SetPlatformAvailability(func(_ context.Context, _ int64, p, _ string) (bool, error) { return avail[p], nil })
	base := CompositeRouteDecision{Matched: true, GroupID: 7, PublicModel: "gemini-4-argon", TargetPlatform: PlatformAntigravity, UpstreamModel: "gemini-4-argon-high"}

	avail[PlatformAntigravity], avail[PlatformGemini] = true, true
	if d := r.ApplyArgonGeminiFallback(context.Background(), base); d.TargetPlatform != PlatformAntigravity {
		t.Fatalf("antigravity available: got %s", d.TargetPlatform)
	}
	avail[PlatformAntigravity] = false
	d := r.ApplyArgonGeminiFallback(context.Background(), base)
	if d.TargetPlatform != PlatformGemini || d.UpstreamModel != "gemini-4-argon-high" {
		t.Fatalf("fallback: got %+v", d)
	}
	avail[PlatformGemini] = false
	if d := r.ApplyArgonGeminiFallback(context.Background(), base); d.TargetPlatform != PlatformAntigravity {
		t.Fatalf("no gemini: got %s", d.TargetPlatform)
	}
	other := base
	other.UpstreamModel, other.PublicModel = "gemini-3.8-flash-high", "gemini-3.8-flash-high"
	avail[PlatformGemini] = true
	if d := r.ApplyArgonGeminiFallback(context.Background(), other); d.TargetPlatform != PlatformAntigravity {
		t.Fatalf("non-argon moved: got %s", d.TargetPlatform)
	}
}
