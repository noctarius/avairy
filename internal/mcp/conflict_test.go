package mcp

import (
	"strings"
	"testing"
)

func TestResolveConflictTool(t *testing.T) {
	s, _ := newTestServer(t)
	s.RegisterAgent("alice", nil, nil)

	// Not enabled → tool errors clearly.
	res, _ := s.handleResolveConflict(asAgent("alice"), call(map[string]any{"path": "f", "content": "x"}))
	if !res.IsError {
		t.Fatal("resolve_conflict should error when reconciliation isn't enabled")
	}

	var gotAgent, gotPath string
	var gotContent []byte
	s.EnableConflicts(func(agentID, path string, content []byte) (uint64, error) {
		gotAgent, gotPath, gotContent = agentID, path, content
		return 7, nil
	})

	ok, err := s.handleResolveConflict(asAgent("alice"), call(map[string]any{"path": "f.go", "content": "merged\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if out := mustText(t, ok); !strings.Contains(out, "resolved f.go") {
		t.Fatalf("result = %q", out)
	}
	if gotAgent != "alice" || gotPath != "f.go" || string(gotContent) != "merged\n" {
		t.Fatalf("resolver got agent=%q path=%q content=%q", gotAgent, gotPath, gotContent)
	}
}
