package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
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

// Reconfigure sends a set_model control request for a model change (live), and rejects an effort
// change (claude has no live effort control — the driver respawns for that).
func TestReconfigure(t *testing.T) {
	var _ agent.Reconfigurer = (*session)(nil)
	var buf bytes.Buffer
	s := &session{enc: json.NewEncoder(&buf)}

	if err := s.Reconfigure(t.Context(), "haiku", ""); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"control_request", "set_model", "haiku"} {
		if !strings.Contains(out, want) {
			t.Fatalf("set_model control should contain %q, got %s", want, out)
		}
	}
	if err := s.Reconfigure(t.Context(), "", "high"); err == nil {
		t.Fatal("a live effort change should be rejected (respawn required)")
	}
}

// ListModels returns claude's static aliases with the fixed effort set.
func TestListModels(t *testing.T) {
	var _ agent.ModelLister = (*session)(nil)
	got, err := (&session{}).ListModels(context.Background())
	if err != nil || len(got) < 3 {
		t.Fatalf("models=%v err=%v", got, err)
	}
	if got[0].ID != "opus" || len(got[0].Efforts) != 5 {
		t.Fatalf("first model = %+v", got[0])
	}
}
