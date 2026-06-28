package adapter

import (
	"context"
	"testing"

	"avairy/internal/agent"
	"avairy/internal/gating"
)

func denyAll(context.Context, gating.Request) (gating.Decision, error) { return gating.Deny, nil }

func TestNewGated(t *testing.T) {
	for _, tc := range []struct {
		family string
		want   agent.Family
	}{
		{"codex", agent.FamilyCodex},
		{"copilot", agent.FamilyCopilot},
		{"grok", agent.FamilyGrok},
	} {
		ad, ok := NewGated(tc.family, denyAll)
		if !ok || ad == nil {
			t.Fatalf("%s: ok=%v ad=%v", tc.family, ok, ad)
		}
		if ad.Family() != tc.want {
			t.Fatalf("%s: family = %q, want %q", tc.family, ad.Family(), tc.want)
		}
	}
	// claude and unknown families are the caller's responsibility.
	for _, f := range []string{"claude", "", "bogus"} {
		if ad, ok := NewGated(f, denyAll); ok || ad != nil {
			t.Fatalf("%q should not be built by NewGated (ok=%v)", f, ok)
		}
	}
}
