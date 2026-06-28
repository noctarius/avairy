package dispatch

import (
	"testing"

	"avairy/internal/bus"
)

func TestDecideCascade(t *testing.T) {
	// No workers → nothing to route to.
	if _, ok := Decide(nil, nil); ok {
		t.Fatal("no workers should yield no target")
	}

	// Exactly one worker → assign it, no LLM consulted.
	called := false
	d, ok := Decide([]string{"linux"}, func() string { called = true; return "macos" })
	if !ok || d.To != bus.Agent("linux") || d.Rule != "sole-candidate" {
		t.Fatalf("sole candidate = %+v ok=%v", d, ok)
	}
	if called {
		t.Fatal("LLM picker must not be consulted when there's only one candidate")
	}

	// Several workers, LLM picks a known one → assign that one.
	d, ok = Decide([]string{"linux", "macos"}, func() string { return "macos" })
	if !ok || d.To != bus.Agent("macos") || d.Rule != "matched" {
		t.Fatalf("matched = %+v", d)
	}

	// Several workers, LLM picks junk/unknown → fall back to a team claim.
	d, _ = Decide([]string{"linux", "macos"}, func() string { return "nobody" })
	if d.To != bus.Team() || d.Rule != "team" {
		t.Fatalf("unknown pick should fall back to team, got %+v", d)
	}

	// Several workers, no LLM available → team.
	d, _ = Decide([]string{"linux", "macos"}, nil)
	if d.To != bus.Team() || d.Rule != "team" {
		t.Fatalf("no picker should fall back to team, got %+v", d)
	}
}
