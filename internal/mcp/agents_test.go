package mcp

import (
	"strings"
	"testing"
)

// list_agents returns the OTHER agents (with caps), excluding the caller, and reports cleanly when
// the caller is alone.
func TestListAgentsTool(t *testing.T) {
	s, _ := newTestServer(t)
	s.RegisterAgent("alice", []string{"backend"}, map[string]string{"os": "darwin"})
	s.RegisterAgent("linbot", []string{"backend"}, map[string]string{"os": "linux"})

	res, _ := s.handleListAgents(asAgent("alice"), call(nil))
	got := mustText(t, res)
	if !strings.Contains(got, "linbot") || !strings.Contains(got, "linux") {
		t.Fatalf("list_agents should show the linux peer: %q", got)
	}
	if strings.Contains(got, "alice") {
		t.Fatalf("list_agents should exclude the caller: %q", got)
	}

	// An agent alone sees none.
	s2, _ := newTestServer(t)
	s2.RegisterAgent("solo", nil, nil)
	res, _ = s2.handleListAgents(asAgent("solo"), call(nil))
	if got := mustText(t, res); !strings.Contains(got, "no other agents") {
		t.Fatalf("solo agent = %q, want \"no other agents\"", got)
	}
}
