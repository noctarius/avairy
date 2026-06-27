// Package e2e is a black-box integration test of the whole distributed channel (#29): a real core
// (bus + MCP bus HTTP server + control HTTP server + canonical hub) and a real node (control.Node)
// talking over actual HTTP, driving a mock agent — zero credits. It asserts the three things the
// distributed surface must keep working end to end: a message round-trips to a node's agent and its
// reply lands in core's journal; a file syncs up and back down through the hub; and a startup
// conflict is raised to the operator and resolved by a resync directive.
package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"avairy/internal/adapter/mock"
	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/control"
	"avairy/internal/journal"
	"avairy/internal/mcp"
	"avairy/internal/workspace"

	"net/http/httptest"
)

// coreEnv is a running core: the in-process services plus the two HTTP endpoints a node dials.
type coreEnv struct {
	jrnl       *journal.Memory
	b          *bus.Bus
	hub        *workspace.Hub
	core       *control.Core
	ctrlURL    string               // node control API (enroll/sync/inbox/events/heartbeat)
	mcpURL     string               // MCP bus (the node proxies agents here)
	conflictCh chan startupConflict // OnNodeConflict notifications
}

type startupConflict struct {
	nodeID string
	paths  []string
}

// newCore wires the real components exactly as cmd/avairy does (OnEnroll → RegisterAgent,
// InboxDrainer → MCP DrainInbox, OnNodeConflict → operator), serving both HTTP surfaces.
func newCore(t *testing.T) *coreEnv {
	t.Helper()
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	bd := board.New(jrnl)
	hub := workspace.NewHub()
	msrv := mcp.NewServer(b, bd, jrnl)
	core := control.NewCore(hub, jrnl)
	core.LivenessTimeout = 10 * time.Second

	core.OnEnroll = func(id string, caps map[string]string) {
		msrv.RegisterAgent(id, mcp.AgentRoles(id, caps), caps) // exactly as cmd/avairy registers a node
	}
	core.InboxDrainer = func(agentID string) []control.InboxMessage {
		var out []control.InboxMessage
		for _, m := range msrv.DrainInbox(agentID) {
			out = append(out, control.InboxMessage{
				ID: m.ID, From: m.From, Body: m.Body,
				Delivery: string(m.Delivery), Interrupt: m.Interrupt, ToKind: string(m.To.Kind),
			})
		}
		return out
	}
	conflictCh := make(chan startupConflict, 8)
	core.OnNodeConflict = func(nodeID, _ string, paths []string) {
		conflictCh <- startupConflict{nodeID: nodeID, paths: paths}
	}

	ctrl := httptest.NewServer(core.Handler())
	mcpHTTP := httptest.NewServer(msrv.HTTPHandler())
	t.Cleanup(func() {
		ctrl.Close()
		mcpHTTP.Close()
	})
	return &coreEnv{jrnl: jrnl, b: b, hub: hub, core: core, ctrlURL: ctrl.URL, mcpURL: mcpHTTP.URL, conflictCh: conflictCh}
}

// runNodeLoop replicates the two goroutines cmd/avairy-node's spawnAgent runs: ship the agent's
// events to core's journal, and pull inbound bus messages from core into the agent (the node-side
// runner, with the #25 wake gate). A short tick keeps the test snappy.
func runNodeLoop(ctx context.Context, n *control.Node, sess agent.Session, agentID string) {
	go func() {
		for ev := range sess.Events() {
			r := control.AgentEventReport{AgentID: agentID, Type: string(ev.Type), Text: ev.Text}
			if ev.Tool != nil {
				r.Tool = ev.Tool.Name
				r.ToolInput = agent.TrimInput(ev.Tool.Input)
			}
			if ev.Usage != nil {
				r.CostUSD = ev.Usage.CostUSD
			}
			_ = n.PostEvents([]control.AgentEventReport{r})
		}
	}()
	waker := bus.NewWaker()
	go func() {
		tk := time.NewTicker(40 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = sess.Close()
				return
			case <-tk.C:
				msgs, err := n.PullInbox(agentID)
				if err != nil {
					continue
				}
				for _, m := range msgs {
					if m.Interrupt {
						_ = sess.Interrupt(ctx)
						continue
					}
					if !waker.Wake(m.From, bus.ToKind(m.ToKind), false, time.Now()) {
						continue
					}
					_ = sess.Send(ctx, m.Body, agent.DeliverySteer)
				}
			}
		}
	}()
}

// A human message published on core's bus reaches a node's agent through the control channel, and
// the agent's reply is reported back into core's journal — the full request/response round-trip.
func TestE2E_MessageRoundTrips(t *testing.T) {
	env := newCore(t)
	node := control.NewNode(env.ctrlURL, "linux-box")
	if err := node.Enroll(env.core.CurrentToken(), "linux", map[string]string{"os": "linux"}); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	ad := mock.New() // echoes the inbound message back as assistant text
	sess, err := ad.Start(t.Context(), agent.SessionConfig{AgentID: "linux-box"})
	if err != nil {
		t.Fatalf("start agent: %v", err)
	}
	runNodeLoop(t.Context(), node, sess, "linux-box")

	env.b.Publish("human", bus.Agent("linux-box"), "ping", agent.DeliverySteer)

	if !waitFor(t, func() bool { return journalHasText(env.jrnl, "linux-box", "ping") }) {
		t.Fatalf("agent's echoed reply never reached core's journal; records=%+v", env.jrnl.Records())
	}
	// The node is enrolled and visible in the fleet.
	if len(env.core.Nodes()) != 1 {
		t.Fatalf("expected 1 enrolled node, got %d", len(env.core.Nodes()))
	}
}

// A file written on one node syncs up to the canonical hub and back down to a second node.
func TestE2E_FileSyncRoundTrips(t *testing.T) {
	env := newCore(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	writeFile(t, dirA, "src/app.go", "package app\n")

	a := control.NewNode(env.ctrlURL, "node-a")
	if err := a.Enroll(env.core.CurrentToken(), "linux", nil); err != nil {
		t.Fatalf("enroll a: %v", err)
	}
	if conflicts, err := a.SyncUp(dirA); err != nil || len(conflicts) != 0 {
		t.Fatalf("syncUp a: err=%v conflicts=%v", err, conflicts)
	}
	if f, ok := env.hub.Get("src/app.go"); !ok || string(f.Content) != "package app\n" {
		t.Fatalf("hub did not receive a's file: %q ok=%v", f.Content, ok)
	}

	b := control.NewNode(env.ctrlURL, "node-b")
	if err := b.Enroll(env.core.CurrentToken(), "darwin", nil); err != nil {
		t.Fatalf("enroll b: %v", err)
	}
	if err := b.SyncDown(dirB); err != nil {
		t.Fatalf("syncDown b: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dirB, "src/app.go"))
	if err != nil || string(got) != "package app\n" {
		t.Fatalf("b did not receive the file: %q err=%v", got, err)
	}
}

// A second node whose copy diverged while offline raises a startup conflict to the operator; the
// operator's resync verdict reconciles it to canonical — all over the wire.
func TestE2E_ConflictRaisesAndResolves(t *testing.T) {
	env := newCore(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	writeFile(t, dirA, "f.go", "A")
	writeFile(t, dirB, "f.go", "B")

	a := control.NewNode(env.ctrlURL, "a")
	if err := a.Enroll(env.core.CurrentToken(), "linux", nil); err != nil {
		t.Fatalf("enroll a: %v", err)
	}
	if _, err := a.SyncUp(dirA); err != nil { // f.go -> v1 (canonical = "A")
		t.Fatalf("syncUp a: %v", err)
	}

	b := control.NewNode(env.ctrlURL, "b") // fresh base, its f.go="B" diverges from v1
	if err := b.Enroll(env.core.CurrentToken(), "linux", nil); err != nil {
		t.Fatalf("enroll b: %v", err)
	}
	conflicts, err := b.SyncUp(dirB)
	if err != nil || len(conflicts) != 1 || conflicts[0].Path != "f.go" {
		t.Fatalf("expected one held conflict on f.go, got err=%v conflicts=%+v", err, conflicts)
	}

	// The operator is notified of the held startup conflict.
	select {
	case nc := <-env.conflictCh:
		if nc.nodeID != "b" || len(nc.paths) != 1 || nc.paths[0] != "f.go" {
			t.Fatalf("unexpected OnNodeConflict: %+v", nc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("operator was never notified of the startup conflict")
	}

	// The operator chooses resync; the verdict rides back on the next heartbeat and the node applies it.
	env.core.SetNodeDirective("b", control.ConflictResync)
	if err := b.Heartbeat(); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if d := b.TakeDirective(); d != control.ConflictResync {
		t.Fatalf("directive = %q, want %q", d, control.ConflictResync)
	}
	if err := b.ApplyDirective(dirB, control.ConflictResync); err != nil {
		t.Fatalf("apply directive: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dirB, "f.go"))
	if err != nil || string(got) != "A" {
		t.Fatalf("resync should have adopted canonical \"A\", got %q err=%v", got, err)
	}
	if paths := b.ConflictPaths(); len(paths) != 0 {
		t.Fatalf("conflict not cleared after resync: %v", paths)
	}
}

// Reproducer for the agent→agent delivery bug: an agent on one node messages an agent on another
// node addressing it by role:<id> and role:<os> (the natural choices when peers are OS-named, and
// what an agent actually did), plus agent:<id>. All three must land in the target's inbox, pulled
// over HTTP. Before AgentRoles registered the id/os as roles, role:<id> matched no subscriber and
// the message vanished — exactly the symptom observed (sender saw it sent; recipient's inbox empty).
func TestE2E_AgentToAgentAcrossNodesDelivers(t *testing.T) {
	env := newCore(t)
	linux := control.NewNode(env.ctrlURL, "linux")
	if err := linux.Enroll(env.core.CurrentToken(), "linux", map[string]string{"os": "linux"}); err != nil {
		t.Fatalf("enroll linux: %v", err)
	}
	macos := control.NewNode(env.ctrlURL, "macos")
	if err := macos.Enroll(env.core.CurrentToken(), "darwin", map[string]string{"os": "darwin"}); err != nil {
		t.Fatalf("enroll macos: %v", err)
	}

	// Each form is what the linux agent's send_message would publish (handleSendMessage does exactly
	// s.bus.Publish(from, addr, body, delivery)). macos must receive every one via PullInbox.
	cases := []struct {
		name string
		addr bus.Addr
		body string
	}{
		{"role:id", bus.Role("macos"), "via role:macos"},   // the form that used to vanish
		{"role:os", bus.Role("darwin"), "via role:darwin"}, // OS capability as a role
		{"agent:id", bus.Agent("macos"), "via agent:macos"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env.b.Publish("linux", tc.addr, tc.body, agent.DeliverySteer)
			msgs := pullInbox(t, macos, "macos")
			if len(msgs) != 1 || msgs[0].From != "linux" || msgs[0].Body != tc.body {
				t.Fatalf("%s: macos inbox = %+v, want one %q from linux", tc.name, msgs, tc.body)
			}
		})
	}
}

// pullInbox polls the node's inbox over HTTP until a message arrives (or it gives up).
func pullInbox(t *testing.T, n *control.Node, agentID string) []control.InboxMessage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := n.PullInbox(agentID)
		if err == nil && len(msgs) > 0 {
			return msgs
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// A node reporting its agent's idle-teardown lifecycle over the events channel lands as the
// agent_sleeping / agent_awake system events the operator consoles render (#28).
func TestE2E_NodeSleepLifecycleSurfaces(t *testing.T) {
	env := newCore(t)
	node := control.NewNode(env.ctrlURL, "edge")
	if err := node.Enroll(env.core.CurrentToken(), "linux", nil); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	if err := node.PostEvents([]control.AgentEventReport{{AgentID: "edge", Type: control.EventAgentSleeping}}); err != nil {
		t.Fatalf("post sleeping: %v", err)
	}
	if !journalHasSystem(env.jrnl, "edge", "agent_sleeping") {
		t.Fatalf("sleeping lifecycle did not surface; records=%+v", env.jrnl.Records())
	}

	if err := node.PostEvents([]control.AgentEventReport{{AgentID: "edge", Type: control.EventAgentAwake}}); err != nil {
		t.Fatalf("post awake: %v", err)
	}
	if !journalHasSystem(env.jrnl, "edge", "agent_awake") {
		t.Fatalf("awake lifecycle did not surface; records=%+v", env.jrnl.Records())
	}
	// And the pseudo-events must NOT have been journaled as agent stream events.
	for _, r := range env.jrnl.Records() {
		if r.Kind == journal.KindAgentEvent {
			if ev, ok := r.Data.(agent.Event); ok && (ev.Type == "sleeping" || ev.Type == "awake") {
				t.Fatalf("lifecycle leaked as an agent stream event: %+v", ev)
			}
		}
	}
}

// --- helpers ---

func journalHasSystem(j *journal.Memory, actor, event string) bool {
	for _, r := range j.Records() {
		if r.Kind != journal.KindSystem || r.Actor != actor {
			continue
		}
		if d, ok := r.Data.(map[string]any); ok && d["event"] == event {
			return true
		}
	}
	return false
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func journalHasText(j *journal.Memory, actor, text string) bool {
	for _, r := range j.Records() {
		if r.Kind != journal.KindAgentEvent || r.Actor != actor {
			continue
		}
		if ev, ok := r.Data.(agent.Event); ok && ev.Type == agent.EventText && ev.Text == text {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
