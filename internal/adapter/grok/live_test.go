package grok

import (
	"context"
	"os"
	"testing"
	"time"

	"avairy/internal/agent"
)

// TestLiveTurn drives a real Grok ACP turn end-to-end (needs xAI auth). Gated by an env var.
func TestLiveTurn(t *testing.T) {
	if os.Getenv("AVAIRY_GROK_LIVE") == "" {
		t.Skip("set AVAIRY_GROK_LIVE=1 to run the live Grok turn")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sess, err := New(nil).Start(ctx, agent.SessionConfig{Workspace: t.TempDir(), Role: "You are a terse test agent."})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sess.Close()
	t.Logf("session id: %s", sess.ID())

	var sawDone bool
	done := make(chan struct{})
	go func() {
		for ev := range sess.Events() {
			switch ev.Type {
			case agent.EventText:
				t.Logf("text: %q", ev.Text)
			case agent.EventToolUse:
				t.Logf("tool: %s", ev.Tool.Name)
			case agent.EventTurnDone:
				sawDone = true
				close(done)
				return
			}
		}
	}()

	if err := sess.Send(ctx, "Reply with exactly the word: OK", agent.DeliverySteer); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case <-done:
	case <-time.After(110 * time.Second):
		t.Fatal("no turn_done within timeout")
	}
	if !sawDone {
		t.Fatal("expected a turn_done event")
	}
}
