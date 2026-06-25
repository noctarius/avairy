package codex

import (
	"context"
	"os"
	"testing"
	"time"

	"avairy/internal/agent"
)

// TestLiveHandshake spawns the real `codex app-server` and verifies the
// initialize + thread/start handshake (no model turn → no credits). Gated behind an env var
// so it never runs in normal/CI test runs.
func TestLiveHandshake(t *testing.T) {
	if os.Getenv("AVAIRY_CODEX_LIVE") == "" {
		t.Skip("set AVAIRY_CODEX_LIVE=1 to run the live codex handshake test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := New().Start(ctx, agent.SessionConfig{
		AgentID:   "alice",
		Role:      "You are alice, a test agent.",
		Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer sess.Close()

	if sess.ID() == "" {
		t.Fatal("expected a thread id after thread/start")
	}
	t.Logf("codex thread id: %s", sess.ID())
}
