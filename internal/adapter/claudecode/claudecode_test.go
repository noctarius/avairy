package claudecode

import (
	"slices"
	"testing"

	"avairy/internal/agent"
)

// buildArgs maps the reasoning-effort level onto claude's --effort flag, and omits it when unset.
func TestBuildArgs_Effort(t *testing.T) {
	a := &Adapter{}
	args := a.buildArgs(agent.SessionConfig{Effort: "high"})
	i := slices.Index(args, "--effort")
	if i < 0 || i+1 >= len(args) || args[i+1] != "high" {
		t.Fatalf("expected --effort high in args, got %v", args)
	}
	if bare := a.buildArgs(agent.SessionConfig{}); slices.Contains(bare, "--effort") {
		t.Fatalf("no effort set should omit --effort, got %v", bare)
	}
}
