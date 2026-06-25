package acp

import (
	"encoding/json"
	"testing"

	"avairy/internal/agent"
	"avairy/internal/gating"
)

func newTestSession() *session {
	return &session{events: make(chan agent.Event, 16), done: make(chan struct{})}
}

// agent_message_chunks accumulate and flush as one text event when a tool_call arrives.
func TestSessionUpdate_TextThenTool(t *testing.T) {
	s := newTestSession()
	s.handleNotification("session/update", json.RawMessage(`{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello "}}}`))
	s.handleNotification("session/update", json.RawMessage(`{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"world"}}}`))
	// nothing emitted yet (buffered)
	select {
	case ev := <-s.events:
		t.Fatalf("unexpected early event %+v", ev)
	default:
	}
	s.handleNotification("session/update", json.RawMessage(`{"update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Bash","kind":"execute"}}`))

	if ev := <-s.events; ev.Type != agent.EventText || ev.Text != "hello world" {
		t.Fatalf("text event = %+v", ev)
	}
	if ev := <-s.events; ev.Type != agent.EventToolUse || ev.Tool.Name != "Bash" {
		t.Fatalf("tool event = %+v", ev)
	}
}

func TestSessionUpdate_ToolResult(t *testing.T) {
	s := newTestSession()
	s.handleNotification("session/update", json.RawMessage(`{"update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}}`))
	if ev := <-s.events; ev.Type != agent.EventToolResult || ev.Tool.ID != "t1" {
		t.Fatalf("tool result = %+v", ev)
	}
}

func TestPermToRequest(t *testing.T) {
	if r := permToRequest("execute", "run", map[string]any{"command": "rm -rf /"}); r.Kind != gating.ActionCommand || r.Summary != "rm -rf /" {
		t.Fatalf("execute: %+v", r)
	}
	if r := permToRequest("delete", "drop table", nil); r.Kind != gating.ActionGitMutate {
		t.Fatalf("delete should be force-gated: %+v", r)
	}
	if r := permToRequest("edit", "edit main.go", nil); r.Kind != gating.ActionFileWrite {
		t.Fatalf("edit should be free: %+v", r)
	}
}

func TestPickOption(t *testing.T) {
	opts := []permOption{
		{OptionID: "a", Kind: "allow_once"},
		{OptionID: "A", Kind: "allow_always"},
		{OptionID: "r", Kind: "reject_once"},
	}
	if got := pickOption(opts, gating.Allow); got != "a" {
		t.Fatalf("allow → %q", got)
	}
	if got := pickOption(opts, gating.AllowForSession); got != "A" {
		t.Fatalf("allow_for_session → %q", got)
	}
	if got := pickOption(opts, gating.Deny); got != "r" {
		t.Fatalf("deny → %q", got)
	}
	if got := pickOption(nil, gating.Allow); got != "" {
		t.Fatalf("no options → %q (want cancelled)", got)
	}
}

// The fail-closed policy denies a destructive execute and allows a benign one — verified
// end-to-end through the option selection an ACP agent would receive.
func TestPolicyThroughPickOption(t *testing.T) {
	opts := []permOption{{OptionID: "ok", Kind: "allow_once"}, {OptionID: "no", Kind: "reject_once"}}
	decide := gating.Policy{}.Decide
	d, _ := decide(t.Context(), permToRequest("execute", "", map[string]any{"command": "rm -rf /"}))
	if pickOption(opts, d) != "no" {
		t.Fatal("destructive execute should select reject")
	}
	d, _ = decide(t.Context(), permToRequest("execute", "", map[string]any{"command": "go test ./..."}))
	if pickOption(opts, d) != "ok" {
		t.Fatal("benign execute should select allow")
	}
}
