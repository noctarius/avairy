package mcp

import (
	"strings"
	"testing"
)

// list_conflicts returns the caller's conflicted files from the wired lister (no grepping), and
// reports "no conflicts" for an agent with none.
func TestListConflictsTool(t *testing.T) {
	s, _ := newTestServer(t)
	s.EnableConflictList(func(agentID string) []string {
		if agentID == "macos" {
			return []string{"a.go", "pkg/b.go"}
		}
		return nil
	})

	res, _ := s.handleListConflicts(asAgent("macos"), call(nil))
	got := mustText(t, res)
	if !strings.Contains(got, "a.go") || !strings.Contains(got, "pkg/b.go") {
		t.Fatalf("list_conflicts = %q, want both paths", got)
	}
	res, _ = s.handleListConflicts(asAgent("other"), call(nil))
	if got := mustText(t, res); got != "no conflicts" {
		t.Fatalf("agent with no conflicts = %q, want \"no conflicts\"", got)
	}
}
