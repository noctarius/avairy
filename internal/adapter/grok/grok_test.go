package grok

import (
	"slices"
	"testing"

	"avairy/internal/agent"
)

// args places model/reasoning-effort as options of the `agent` subcommand — before the `stdio`
// command (which doesn't accept them) — so grok honors them.
func TestArgs(t *testing.T) {
	base := args(agent.SessionConfig{})
	if !slices.Equal(base, []string{"agent", "stdio"}) {
		t.Fatalf("bare args = %v", base)
	}
	got := args(agent.SessionConfig{Model: "grok-4", Effort: "high"})
	if got[0] != "agent" || got[len(got)-1] != "stdio" {
		t.Fatalf("flags must sit between `agent` and `stdio`, got %v", got)
	}
	for _, want := range [][2]string{{"--model", "grok-4"}, {"--reasoning-effort", "high"}} {
		i := slices.Index(got, want[0])
		if i <= 0 || i+1 >= len(got) || got[i+1] != want[1] { // i>0 = after `agent`; value follows the flag
			t.Fatalf("expected %s %s between agent/stdio in %v", want[0], want[1], got)
		}
	}
}
