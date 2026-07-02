package agent

import (
	"strings"
	"testing"
)

func TestValidateConfig_Effort(t *testing.T) {
	caps := Capabilities{ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}}

	// A valid effort — and the empty (unset) effort — pass.
	for _, e := range []string{"", "low", "max"} {
		if err := ValidateConfig(caps, SessionConfig{Effort: e}); err != nil {
			t.Errorf("effort %q should be valid: %v", e, err)
		}
	}
	// An unsupported effort fails, and the message lists the valid levels.
	err := ValidateConfig(caps, SessionConfig{Effort: "bogus"})
	if err == nil {
		t.Fatal("expected an error for an unsupported effort")
	}
	if !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "high") {
		t.Fatalf("error should name the bad value and the valid ones, got %q", err)
	}
	// A family with no known effort set (e.g. codex, validated per-model by the agent) skips
	// validation — any value passes through.
	if err := ValidateConfig(Capabilities{}, SessionConfig{Effort: "whatever"}); err != nil {
		t.Fatalf("no known efforts should skip validation, got %v", err)
	}
}
