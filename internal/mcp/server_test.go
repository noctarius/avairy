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

// Reproducer (#bug): a node's PullInbox loop drains the agent's wake queue (DrainInbox) and discards
// context-only messages it won't wake on (#25 — agent→role/broadcast). That must NOT consume the
// agent's read_inbox: a role-addressed peer message has to remain readable. Before the wake queue
// was split from the read_inbox buffer, the daemon's drain stole the message and read_inbox went
// empty — exactly the "linux → role:macos never reaches macos" symptom.
func TestNodeWakeDrainDoesNotStealReadInbox(t *testing.T) {
	s, _ := newTestServer(t)
	s.RegisterAgent("macos", AgentRoles("macos", map[string]string{"os": "darwin"}), map[string]string{"os": "darwin"})
	s.RegisterAgent("linux", AgentRoles("linux", map[string]string{"os": "linux"}), map[string]string{"os": "linux"})

	// linux addresses macos by role (the form the node won't wake on).
	mustText(t, must(s.handleSendMessage(asAgent("linux"), call(map[string]any{"to": "role:macos", "body": "cross-check please"}))))

	// The node daemon's PullInbox loop drains macos's wake queue every tick (and discards the
	// context-only message because Wake()==false). This must not empty read_inbox.
	_ = s.DrainInbox("macos")

	res := must(s.handleReadInbox(asAgent("macos"), call(nil)))
	var msgs []inboxMessage
	if err := json.Unmarshal([]byte(mustText(t, res)), &msgs); err != nil {
		t.Fatalf("inbox json: %v", err)
	}
	if len(msgs) != 1 || msgs[0].From != "linux" || msgs[0].Body != "cross-check please" {
		t.Fatalf("read_inbox = %+v; the node's wake drain stole the role-addressed message", msgs)
	}
}

func must(res *mcpgo.CallToolResult, err error) *mcpgo.CallToolResult {
	if err != nil {
		panic(err)
	}
	return res
}

// A directed message that matches no recipient must be rejected so the sender knows (and can fix
// the address) instead of getting a false "sent" — the silent drop is what hid the role:macos bug.
func TestSendMessageRejectsUnaddressable(t *testing.T) {
	s, _ := newTestServer(t)
	s.RegisterAgent("alice", AgentRoles("alice", map[string]string{"os": "linux"}), map[string]string{"os": "linux"})

	if res := must(s.handleSendMessage(asAgent("alice"), call(map[string]any{"to": "agent:ghost", "body": "hi"}))); !res.IsError {
		t.Fatalf("agent:ghost should be rejected (no such agent); got %q", resultText(res))
	}
	if res := must(s.handleSendMessage(asAgent("alice"), call(map[string]any{"to": "role:macos", "body": "hi"}))); !res.IsError {
		t.Fatalf("role:macos should be rejected (no agent has that role); got %q", resultText(res))
	}
	// A role only the sender holds reaches nobody → reject too.
	if res := must(s.handleSendMessage(asAgent("alice"), call(map[string]any{"to": "role:linux", "body": "hi"}))); !res.IsError {
		t.Fatalf("role:linux (only the sender) should be rejected; got %q", resultText(res))
	}

	// A real recipient succeeds; broadcast is always allowed (fan-out, not a targeted address).
	s.RegisterAgent("bob", AgentRoles("bob", map[string]string{"os": "linux"}), map[string]string{"os": "linux"})
	if res := must(s.handleSendMessage(asAgent("alice"), call(map[string]any{"to": "agent:bob", "body": "hi"}))); res.IsError {
		t.Fatalf("agent:bob should succeed: %s", resultText(res))
	}
	if res := must(s.handleSendMessage(asAgent("alice"), call(map[string]any{"to": "role:linux", "body": "hi"}))); res.IsError {
		t.Fatalf("role:linux now reaches bob, should succeed: %s", resultText(res))
	}
	if res := must(s.handleSendMessage(asAgent("alice"), call(map[string]any{"to": "broadcast", "body": "hi"}))); res.IsError {
		t.Fatalf("broadcast should always be allowed: %s", resultText(res))
	}
}
