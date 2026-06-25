package control

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"avairy/internal/journal"
	"avairy/internal/workspace"
)

func newCoreServer(t *testing.T) (*Core, *httptest.Server) {
	t.Helper()
	c := NewCore(workspace.NewHub(), journal.NewMemory())
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	return c, srv
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Enroll two nodes; a file synced up from one appears when the other syncs down — over HTTP,
// through the canonical hub.
func TestEnrollAndSyncAcrossNodes(t *testing.T) {
	core, srv := newCoreServer(t)
	tok := core.CurrentToken()

	dirA, dirB := t.TempDir(), t.TempDir()
	writeFile(t, dirA, "src/app.go", "package app\n")

	nodeA := NewNode(srv.URL, "linux-box")
	if err := nodeA.Enroll(tok, "", "linux", map[string]string{"os": "linux"}); err != nil {
		t.Fatalf("enroll A: %v", err)
	}
	if conflicts, err := nodeA.SyncUp(dirA); err != nil || len(conflicts) != 0 {
		t.Fatalf("syncUp A: err=%v conflicts=%v", err, conflicts)
	}

	nodeB := NewNode(srv.URL, "mac-box")
	if err := nodeB.Enroll(core.CurrentToken(), "", "darwin", map[string]string{"os": "darwin"}); err != nil {
		t.Fatalf("enroll B: %v", err)
	}
	if err := nodeB.SyncDown(dirB); err != nil {
		t.Fatalf("syncDown B: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dirB, "src/app.go"))
	if err != nil || string(got) != "package app\n" {
		t.Fatalf("B did not receive A's file: %q err=%v", got, err)
	}

	// Both nodes are registered and live.
	if len(core.Nodes()) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(core.Nodes()))
	}
}

func TestInvalidEnrollTokenRejected(t *testing.T) {
	_, srv := newCoreServer(t)
	n := NewNode(srv.URL, "rogue")
	if err := n.Enroll("not-a-real-token", "", "linux", nil); err == nil {
		t.Fatal("expected enrollment to fail with a bad token")
	}
}

func TestEnrollTokenBindingAndRejoin(t *testing.T) {
	core, srv := newCoreServer(t)
	tok := core.CurrentToken()
	if err := NewNode(srv.URL, "first").Enroll(tok, "", "linux", nil); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	// The operator-facing token auto-regenerates once a node consumes it.
	if core.CurrentToken() == tok {
		t.Fatal("token should auto-regenerate after a node enrolls")
	}
	// A different node may not reuse a bound token.
	if err := NewNode(srv.URL, "second").Enroll(tok, "", "linux", nil); err == nil {
		t.Fatal("a bound token must be rejected for a different node")
	}
	// The same node may rejoin with its bound token (restart / crash recovery).
	if err := NewNode(srv.URL, "first").Enroll(tok, "", "linux", nil); err != nil {
		t.Fatalf("same node should rejoin with its bound token: %v", err)
	}
}

// A concurrent edit detected at the hub is reported back over the wire as a conflict.
func TestConflictOverWire(t *testing.T) {
	core, srv := newCoreServer(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	writeFile(t, dirA, "f.go", "A")
	writeFile(t, dirB, "f.go", "B")

	nodeA := NewNode(srv.URL, "a")
	nodeA.Enroll(core.CurrentToken(), "", "linux", nil)
	nodeA.SyncUp(dirA) // f.go -> v1

	nodeB := NewNode(srv.URL, "b") // fresh base, never pulled v1
	nodeB.Enroll(core.CurrentToken(), "", "linux", nil)
	conflicts, err := nodeB.SyncUp(dirB)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Path != "f.go" || conflicts[0].HubVersion != 1 {
		t.Fatalf("expected a conflict on f.go @v1, got %+v", conflicts)
	}
}

func TestMCPProxyInjectsIdentity(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Header.Get("X-Avairy-Agent"))
	}))
	t.Cleanup(backend.Close)

	h, err := NewNode("", "").MCPProxy(backend.URL, "alice")
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(h)
	t.Cleanup(proxy.Close)

	resp, err := http.Get(proxy.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "alice" {
		t.Fatalf("proxy did not inject identity, backend saw %q", body)
	}
}

// Enrolling with an agent id fires OnEnroll; inbound messages and event reports flow over
// the channel.
func TestEnrollHookInboxAndEvents(t *testing.T) {
	j := journal.NewMemory()
	c := NewCore(workspace.NewHub(), j)
	var enrolledAgent string
	c.OnEnroll = func(nodeID, agentID string, caps map[string]string) { enrolledAgent = agentID }
	c.InboxDrainer = func(agentID string) []InboxMessage {
		if agentID == "claude" {
			return []InboxMessage{{ID: "m1", From: "human", Body: "reproduce it", Delivery: "steer"}}
		}
		return nil
	}
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)

	n := NewNode(srv.URL, "macos")
	if err := n.Enroll(c.CurrentToken(), "claude", "darwin", map[string]string{"os": "darwin"}); err != nil {
		t.Fatal(err)
	}
	if enrolledAgent != "claude" {
		t.Fatalf("OnEnroll agent = %q", enrolledAgent)
	}

	msgs, err := n.PullInbox("claude")
	if err != nil || len(msgs) != 1 || msgs[0].Body != "reproduce it" {
		t.Fatalf("inbox pull: err=%v msgs=%+v", err, msgs)
	}

	if err := n.PostEvents([]AgentEventReport{{AgentID: "claude", Type: "text", Text: "on it"}}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range j.Records() {
		if rec.Kind == journal.KindAgentEvent && rec.Actor == "claude" {
			found = true
		}
	}
	if !found {
		t.Fatal("agent event was not journaled at core")
	}
}
