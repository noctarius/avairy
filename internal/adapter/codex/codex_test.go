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

// reasoningArgs pins codex's model_reasoning_effort via a -c override, and is empty when unset.
func TestReasoningArgs(t *testing.T) {
	if got := reasoningArgs(""); got != nil {
		t.Fatalf("no effort should yield no args, got %v", got)
	}
	got := reasoningArgs("high")
	if len(got) != 2 || got[0] != "-c" || got[1] != `model_reasoning_effort="high"` {
		t.Fatalf("reasoningArgs(high) = %v", got)
	}
}

// Reconfigure updates the model/effort that ride the next turn/start (codex applies both live). An
// empty field leaves the current value unchanged.
func TestReconfigureLive(t *testing.T) {
	var _ agent.Reconfigurer = (*session)(nil) // codex sessions are live-reconfigurable
	s := &session{model: "gpt-5", effort: "low"}
	if err := s.Reconfigure(t.Context(), "gpt-5.4", "high"); err != nil {
		t.Fatal(err)
	}
	if s.model != "gpt-5.4" || s.effort != "high" {
		t.Fatalf("model=%q effort=%q, want gpt-5.4/high", s.model, s.effort)
	}
	if err := s.Reconfigure(t.Context(), "", ""); err != nil { // no-op keeps values
		t.Fatal(err)
	}
	if s.model != "gpt-5.4" || s.effort != "high" {
		t.Fatalf("empty args should leave values, got %q/%q", s.model, s.effort)
	}
}

// parseModelList maps the model/list response into picker entries (id from `model`, per-model efforts).
func TestParseModelList(t *testing.T) {
	raw := []byte(`{"data":[
		{"id":"gpt-5-codex","model":"gpt-5-codex","displayName":"GPT-5 Codex","supportedReasoningEfforts":["low","medium","high"]},
		{"id":"o3","model":"o3","displayName":"o3","supportedReasoningEfforts":["medium","high"]}
	]}`)
	got := parseModelList(raw)
	if len(got) != 2 || got[0].ID != "gpt-5-codex" || got[0].Name != "GPT-5 Codex" {
		t.Fatalf("parsed = %+v", got)
	}
	if len(got[0].Efforts) != 3 || got[0].Efforts[2] != "high" {
		t.Fatalf("per-model efforts = %v", got[0].Efforts)
	}
	if parseModelList([]byte("nonsense")) != nil {
		t.Fatal("bad json should parse to nil")
	}
}
