package copilot

import (
	"slices"
	"testing"

	"avairy/internal/agent"
)

// args maps model/effort onto copilot's global flags, appended after the ACP transport flags.
func TestArgs(t *testing.T) {
	base := args(agent.SessionConfig{})
	if !slices.Equal(base, []string{"--acp", "--stdio"}) {
		t.Fatalf("bare args = %v", base)
	}
	got := args(agent.SessionConfig{Model: "gpt-5.4", Effort: "high"})
	for _, want := range [][2]string{{"--model", "gpt-5.4"}, {"--effort", "high"}} {
		i := slices.Index(got, want[0])
		if i < 0 || i+1 >= len(got) || got[i+1] != want[1] {
			t.Fatalf("expected %s %s in %v", want[0], want[1], got)
		}
	}
}
