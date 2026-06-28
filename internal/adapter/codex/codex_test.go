package codex

import (
	"encoding/json"
	"testing"

	"avairy/internal/adapter/jsonrpc"
	"avairy/internal/agent"
)

// nopCloser is a no-op io.WriteCloser for tests that build a session/peer without a real subprocess.
type nopCloser struct{}

func (nopCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopCloser) Close() error                { return nil }

// testSession builds a session with a peer whose Done channel is open (never fires), so emit()
// delivers to events.
func testSession(buf int) *session {
	return &session{events: make(chan agent.Event, buf), peer: jsonrpc.NewPeer("codex", nopCloser{})}
}

func TestItemToEvent(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want agent.EventType
		text string
		tool string
	}{
		{"agentMessage", `{"type":"agentMessage","id":"i1","text":"done"}`, agent.EventText, "done", ""},
		{"reasoning", `{"type":"reasoning","id":"i2","text":"thinking"}`, agent.EventReasoning, "thinking", ""},
		{"command", `{"type":"commandExecution","id":"i3","command":["go","test"]}`, agent.EventToolUse, "", "commandExecution"},
		{"mcp", `{"type":"mcpToolCall","id":"i4","tool":"post_task"}`, agent.EventToolUse, "", "mcpToolCall"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev, ok := itemToEvent(json.RawMessage(c.raw))
			if !ok || ev.Type != c.want {
				t.Fatalf("got type=%v ok=%v", ev.Type, ok)
			}
			if c.text != "" && ev.Text != c.text {
				t.Fatalf("text=%q want %q", ev.Text, c.text)
			}
			if c.tool != "" && (ev.Tool == nil || ev.Tool.Name != c.tool) {
				t.Fatalf("tool=%+v want name %q", ev.Tool, c.tool)
			}
		})
	}
}

func TestItemToEventSkipsUnknown(t *testing.T) {
	if _, ok := itemToEvent(json.RawMessage(`{"type":"sleep","id":"x"}`)); ok {
		t.Fatal("sleep item should be skipped")
	}
}

func TestHandleNotificationTurnCompleted(t *testing.T) {
	s := testSession(4)
	s.activeTurn = "turn_1"
	s.OnNotification("turn/completed", json.RawMessage(`{"threadId":"t","turn":{"id":"turn_1","status":"completed"}}`))

	if s.activeTurn != "" {
		t.Fatalf("expected activeTurn cleared, got %q", s.activeTurn)
	}
	select {
	case ev := <-s.events:
		if ev.Type != agent.EventTurnDone {
			t.Fatalf("got %v", ev.Type)
		}
	default:
		t.Fatal("expected a turn_done event")
	}
}

func TestHandleNotificationItemCompleted(t *testing.T) {
	s := testSession(4)
	s.OnNotification("item/completed", json.RawMessage(`{"turnId":"x","item":{"type":"agentMessage","id":"i","text":"hello"}}`))
	select {
	case ev := <-s.events:
		if ev.Type != agent.EventText || ev.Text != "hello" {
			t.Fatalf("got %+v", ev)
		}
	default:
		t.Fatal("expected a text event")
	}
}

func TestDefaultApprove(t *testing.T) {
	if got := defaultApprove("item/commandExecution/requestApproval", nil); got != "accept" {
		t.Fatalf("v2 decision = %q", got)
	}
	if got := defaultApprove("execCommandApproval", nil); got != "approved" {
		t.Fatalf("v1 decision = %q", got)
	}
}
