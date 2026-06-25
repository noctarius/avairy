package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

func newTestServer(t *testing.T) (*Server, journal.Log) {
	t.Helper()
	j := journal.NewMemory()
	s := NewServer(bus.New(j), board.New(j), j)
	return s, j
}

func asAgent(id string) context.Context {
	return context.WithValue(context.Background(), agentKey, id)
}

func call(args map[string]any) mcpgo.CallToolRequest {
	var r mcpgo.CallToolRequest
	r.Params.Arguments = args
	return r
}

func mustText(t *testing.T, res *mcpgo.CallToolResult) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(res))
	}
	return resultText(res)
}

func resultText(res *mcpgo.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// Agent-to-agent messaging through the MCP tools: alice sends, bob reads.
func TestSendMessageAndReadInbox(t *testing.T) {
	s, _ := newTestServer(t)
	s.RegisterAgent("alice", nil, map[string]string{"os": "darwin"})
	s.RegisterAgent("bob", []string{"backend"}, map[string]string{"os": "linux"})

	res, err := s.handleSendMessage(asAgent("alice"), call(map[string]any{"to": "agent:bob", "body": "hi bob"}))
	if err != nil {
		t.Fatal(err)
	}
	mustText(t, res)

	res, _ = s.handleReadInbox(asAgent("bob"), call(nil))
	var msgs []inboxMessage
	if err := json.Unmarshal([]byte(mustText(t, res)), &msgs); err != nil {
		t.Fatalf("inbox json: %v", err)
	}
	if len(msgs) != 1 || msgs[0].From != "alice" || msgs[0].Body != "hi bob" {
		t.Fatalf("bob inbox = %+v", msgs)
	}

	// Second read is empty (inbox drained).
	res, _ = s.handleReadInbox(asAgent("bob"), call(nil))
	if got := mustText(t, res); got != "[]" {
		t.Fatalf("second read = %q, want []", got)
	}
}

// send_message must reject an unidentified caller (no spoofing / no anonymous sends).
func TestSendMessageRequiresIdentity(t *testing.T) {
	s, _ := newTestServer(t)
	res, _ := s.handleSendMessage(context.Background(), call(map[string]any{"to": "agent:bob", "body": "x"}))
	if !res.IsError {
		t.Fatal("expected error for missing caller identity")
	}
}

// Capability-gated claim: a darwin agent cannot claim an os=linux task; a linux agent can.
func TestPostAndClaimTaskCapabilityGate(t *testing.T) {
	s, _ := newTestServer(t)
	s.RegisterAgent("alice", nil, map[string]string{"os": "darwin"})
	s.RegisterAgent("bob", nil, map[string]string{"os": "linux"})

	res, _ := s.handlePostTask(asAgent("alice"), call(map[string]any{
		"title":    "repro linux panic",
		"requires": []any{"os=linux"},
	}))
	posted := mustText(t, res) // "posted t1"
	taskID := strings.TrimPrefix(posted, "posted ")

	// alice (darwin) is rejected.
	res, _ = s.handleClaimTask(asAgent("alice"), call(map[string]any{"task_id": taskID}))
	if !res.IsError {
		t.Fatal("darwin agent should not claim os=linux task")
	}

	// bob (linux) succeeds.
	res, _ = s.handleClaimTask(asAgent("bob"), call(map[string]any{"task_id": taskID}))
	if got := mustText(t, res); got != "claimed "+taskID {
		t.Fatalf("bob claim = %q", got)
	}
}
