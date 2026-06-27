package claudecode

import (
	"testing"

	"avairy/internal/agent"
)

// Captured verbatim from a live `claude -p --output-format stream-json` run (2.1.176).
const (
	lineInit       = `{"type":"system","subtype":"init","cwd":"/x","session_id":"febeeb5a-95ef-494f-a929-20689a7a60ce","tools":["Task"],"mcp_servers":[],"model":"claude-opus-4-8[1m]","permissionMode":"default"}`
	lineRateLimit  = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"},"session_id":"febeeb5a"}`
	lineAssistant  = `{"type":"assistant","message":{"model":"claude-opus-4-8","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":null,"usage":{"input_tokens":4423,"output_tokens":1}},"session_id":"febeeb5a"}`
	lineToolUse    = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"go test ./..."}}]}}`
	lineThinking   = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Let me check the failing test first.","signature":"abc"}]}}`
	lineRedacted   = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"redacted_thinking","data":"eyJ..."}]}}`
	lineToolResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok\n"}]}}`
	lineResultOK   = `{"type":"result","subtype":"success","is_error":false,"result":"OK","stop_reason":"end_turn","total_cost_usd":0.1995,"usage":{"input_tokens":4423,"output_tokens":4}}`
	lineResultErr  = `{"type":"result","subtype":"error","is_error":true,"result":"boom","total_cost_usd":0.01}`
)

func TestParseLine_Init(t *testing.T) {
	evs, sid := parseLine([]byte(lineInit))
	if len(evs) != 0 {
		t.Fatalf("init: want 0 events, got %d", len(evs))
	}
	if sid != "febeeb5a-95ef-494f-a929-20689a7a60ce" {
		t.Fatalf("init: session id = %q", sid)
	}
}

func TestParseLine_RateLimitIgnored(t *testing.T) {
	evs, sid := parseLine([]byte(lineRateLimit))
	if len(evs) != 0 || sid != "" {
		t.Fatalf("rate_limit: want no events/sid, got %d events sid=%q", len(evs), sid)
	}
}

func TestParseLine_AssistantText(t *testing.T) {
	evs, _ := parseLine([]byte(lineAssistant))
	if len(evs) != 1 || evs[0].Type != agent.EventText || evs[0].Text != "OK" {
		t.Fatalf("assistant text: got %+v", evs)
	}
	if len(evs[0].Raw) == 0 {
		t.Fatal("assistant text: Raw not populated")
	}
}

func TestParseLine_Thinking(t *testing.T) {
	evs, _ := parseLine([]byte(lineThinking))
	if len(evs) != 1 || evs[0].Type != agent.EventReasoning || evs[0].Text != "Let me check the failing test first." {
		t.Fatalf("thinking: got %+v", evs)
	}
	evs, _ = parseLine([]byte(lineRedacted))
	if len(evs) != 1 || evs[0].Type != agent.EventReasoning || evs[0].Text != "[redacted thinking]" {
		t.Fatalf("redacted_thinking: got %+v", evs)
	}
}

func TestParseLine_ToolUse(t *testing.T) {
	evs, _ := parseLine([]byte(lineToolUse))
	if len(evs) != 1 || evs[0].Type != agent.EventToolUse {
		t.Fatalf("tool_use: got %+v", evs)
	}
	tc := evs[0].Tool
	if tc == nil || tc.ID != "tu_1" || tc.Name != "Bash" {
		t.Fatalf("tool_use: tool = %+v", tc)
	}
	if got, _ := tc.Input["command"].(string); got != "go test ./..." {
		t.Fatalf("tool_use: input.command = %q", got)
	}
}

func TestParseLine_ToolResult(t *testing.T) {
	evs, _ := parseLine([]byte(lineToolResult))
	if len(evs) != 1 || evs[0].Type != agent.EventToolResult {
		t.Fatalf("tool_result: got %+v", evs)
	}
	if evs[0].Tool == nil || evs[0].Tool.ID != "tu_1" {
		t.Fatalf("tool_result: tool = %+v", evs[0].Tool)
	}
}

func TestParseLine_ResultDone(t *testing.T) {
	evs, _ := parseLine([]byte(lineResultOK))
	if len(evs) != 1 || evs[0].Type != agent.EventTurnDone {
		t.Fatalf("result: got %+v", evs)
	}
	u := evs[0].Usage
	if u == nil || u.InputTokens != 4423 || u.OutputTokens != 4 || u.CostUSD != 0.1995 {
		t.Fatalf("result: usage = %+v", u)
	}
}

func TestParseLine_ResultError(t *testing.T) {
	evs, _ := parseLine([]byte(lineResultErr))
	if len(evs) != 2 || evs[0].Type != agent.EventError || evs[1].Type != agent.EventTurnDone {
		t.Fatalf("result error: got %+v", evs)
	}
	if evs[0].Text != "boom" {
		t.Fatalf("result error: text = %q", evs[0].Text)
	}
}

func TestParseLine_Garbage(t *testing.T) {
	evs, _ := parseLine([]byte(`not json`))
	if len(evs) != 1 || evs[0].Type != agent.EventError {
		t.Fatalf("garbage: got %+v", evs)
	}
}
