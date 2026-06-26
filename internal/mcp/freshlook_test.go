package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestFreshLookTool(t *testing.T) {
	s, _ := newTestServer(t)
	s.RegisterAgent("alice", nil, nil)

	// Not enabled → clear error.
	res, _ := s.handleFreshLook(asAgent("alice"), call(map[string]any{"question": "x"}))
	if !res.IsError {
		t.Fatal("fresh_look should error when not enabled")
	}

	var gotQ string
	s.EnableFreshLook(func(_ context.Context, q string) (string, error) {
		gotQ = q
		return "an independent take", nil
	})
	out, err := s.handleFreshLook(asAgent("alice"), call(map[string]any{"question": "is this a loop?"}))
	if err != nil {
		t.Fatal(err)
	}
	if gotQ != "is this a loop?" {
		t.Fatalf("question not forwarded: %q", gotQ)
	}
	if got := mustText(t, out); !strings.Contains(got, "independent take") {
		t.Fatalf("answer not returned: %q", got)
	}
}
