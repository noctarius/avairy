package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

func newTestModel() (*Model, *bus.Bus, journal.Log) {
	j := journal.NewMemory()
	b := bus.New(j)
	bd := board.New(j)
	return NewModel(Deps{Bus: b, Board: bd, Journal: j}), b, j
}

// Records fold into the conversation view and the fleet, and View renders them.
func TestApplyAndView(t *testing.T) {
	m, _, _ := newTestModel()
	m.apply(journal.Record{Seq: 1, Kind: journal.KindMessage, Actor: "human",
		Data: bus.Message{ID: "m1", From: "human", To: bus.Agent("alice"), Body: "repro it"}})
	m.apply(journal.Record{Seq: 2, Kind: journal.KindAgentEvent, Actor: "alice",
		Data: agent.Event{Type: agent.EventText, Text: "on it"}})

	v := m.render()
	for _, want := range []string{"repro it", "on it", "alice"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view missing %q:\n%s", want, v)
		}
	}
}

// The Approvals tab renders pending gated actions, and y/n on a selected row resolves it.
func TestApprovalsViewAndResolve(t *testing.T) {
	pending := []ApprovalItem{
		{ID: "ap1", AgentID: "linbot", Kind: "command", Summary: "git push origin main", Reason: "ship it"},
		{ID: "ap2", AgentID: "macbot", Kind: "install", Summary: "brew install jq"},
	}
	var resolved [2]string // id, decision
	j := journal.NewMemory()
	b := bus.New(j)
	bd := board.New(j)
	m := NewModel(Deps{
		Bus: b, Board: bd, Journal: j,
		PendingApprovals: func() []ApprovalItem { return pending },
		ResolveApproval: func(id, decision string) {
			resolved = [2]string{id, decision}
			// drop the resolved item so the list shrinks like the real broker
			out := pending[:0]
			for _, p := range pending {
				if p.ID != id {
					out = append(out, p)
				}
			}
			pending = out
		},
	})
	// The pending count badges in the tab bar from any tab (inactive tab → unstyled text).
	if v := m.render(); !strings.Contains(v, "Approvals (2)") {
		t.Fatalf("tab bar missing pending badge:\n%s", v)
	}

	m.tab = tabApprovals
	v := m.render()
	for _, want := range []string{"git push origin main", "brew install jq", "linbot"} {
		if !strings.Contains(v, want) {
			t.Fatalf("approvals view missing %q:\n%s", want, v)
		}
	}

	// Move to the second row and deny it.
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if resolved != [2]string{"ap2", "deny"} {
		t.Fatalf("expected ap2 denied, got %v", resolved)
	}
	// Allow the remaining one with 'y'.
	m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if resolved != [2]string{"ap1", "allow"} {
		t.Fatalf("expected ap1 allowed, got %v", resolved)
	}
}

// Dedup: applying the same Seq twice doesn't double-render.
func TestApplyDedup(t *testing.T) {
	m, _, _ := newTestModel()
	rec := journal.Record{Seq: 7, Kind: journal.KindAgentEvent, Actor: "alice",
		Data: agent.Event{Type: agent.EventText, Text: "hi"}}
	m.apply(rec)
	m.apply(rec)
	if got := strings.Count(strings.Join(m.conv, "\n"), "hi"); got != 1 {
		t.Fatalf("expected 1 conv line, got %d", got)
	}
}

// Submitting "@alice ..." publishes a directed bus message (human injection).
func TestSubmitPublishesDirected(t *testing.T) {
	m, _, j := newTestModel()
	m.input.SetValue("@alice please reproduce")
	m.submit()

	found := false
	for _, rec := range j.Records() {
		if rec.Kind != journal.KindMessage {
			continue
		}
		if msg, ok := rec.Data.(bus.Message); ok &&
			msg.From == "human" && msg.To.Kind == bus.ToAgent && msg.To.Value == "alice" && msg.Body == "please reproduce" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a directed human message to agent:alice on the bus")
	}
}

// A bare line broadcasts.
func TestSubmitBroadcast(t *testing.T) {
	m, _, j := newTestModel()
	m.input.SetValue("standup in 5")
	m.submit()

	found := false
	for _, rec := range j.Records() {
		if msg, ok := rec.Data.(bus.Message); ok && msg.To.Kind == bus.ToBroadcast && msg.Body == "standup in 5" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a broadcast human message on the bus")
	}
}

// Node-lifecycle system records must not appear as agents in the fleet; only agent
// report_status does.
func TestSystemRecordsFleet(t *testing.T) {
	m, _, _ := newTestModel()
	m.apply(journal.Record{Seq: 1, Kind: journal.KindSystem, Actor: "claude-macos",
		Data: map[string]any{"event": "node_enrolled", "os": "darwin"}})
	if len(m.agentOrder) != 0 {
		t.Fatalf("node_enrolled must not create a fleet agent, got %v", m.agentOrder)
	}
	m.apply(journal.Record{Seq: 2, Kind: journal.KindSystem, Actor: "claude",
		Data: map[string]any{"event": "report_status", "status": "blocked"}})
	if a := m.agents["claude"]; a == nil || a.status != "blocked" {
		t.Fatalf("report_status should mark claude blocked, got %+v", a)
	}
}

// Esc never quits; quitting takes two ctrl+c in succession (any other key disarms).
func TestQuitRequiresDoubleCtrlC(t *testing.T) {
	m, _, _ := newTestModel()

	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}); cmd != nil || m.quitArmed {
		t.Fatal("esc does not quit")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd != nil || !m.quitArmed {
		t.Fatalf("first ctrl+c should arm without quitting (cmd=%v)", cmd)
	}
	if m.Update(tea.KeyPressMsg{Code: tea.KeyTab}); m.quitArmed {
		t.Fatal("a different key should disarm the pending quit")
	}
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("two ctrl+c in succession should return a quit command")
	}
}

// Esc publishes an interrupt control signal on the bus (stop running agents).
func TestEscSendsInterrupt(t *testing.T) {
	m, _, j := newTestModel()
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	found := false
	for _, rec := range j.Records() {
		if msg, ok := rec.Data.(bus.Message); ok && msg.Interrupt {
			found = true
		}
	}
	if !found {
		t.Fatal("esc should publish an interrupt on the bus")
	}
}

// The recipient selector and the input @mention stay in sync both ways.
func TestRecipientSelector(t *testing.T) {
	m, _, _ := newTestModel()
	m.touchAgent("alice")
	m.touchAgent("bob")

	// input → selector
	m.input.SetValue("@bob hi")
	if got := m.selectedTarget(); got != "bob" {
		t.Fatalf("typing @bob → selectedTarget=%q", got)
	}
	m.input.SetValue("hello")
	if got := m.selectedTarget(); got != "broadcast" {
		t.Fatalf("plain text → selectedTarget=%q", got)
	}

	// selector → input (cycle: broadcast → alice → bob), preserving the body
	m.cycleTarget(1)
	if got := m.input.Value(); got != "@alice hello" {
		t.Fatalf("cycle 1 → %q", got)
	}
	m.cycleTarget(1)
	if got := m.input.Value(); got != "@bob hello" {
		t.Fatalf("cycle 2 → %q", got)
	}
	m.setTarget("broadcast")
	if got := m.input.Value(); got != "hello" {
		t.Fatalf("broadcast strips mention → %q", got)
	}
}

// Roster agents appear in the fleet at startup, before any message.
func TestRosterPopulatesFleet(t *testing.T) {
	j := journal.NewMemory()
	m := NewModel(Deps{Bus: bus.New(j), Board: board.New(j), Journal: j,
		Roster: func() []string { return []string{"alice", "bob"} }})
	if m.agents["alice"] == nil || m.agents["bob"] == nil {
		t.Fatalf("roster agents should populate the fleet at startup: %v", m.agentOrder)
	}
	if m.agents["alice"].status != "idle" {
		t.Fatalf("roster agent should start idle, got %q", m.agents["alice"].status)
	}
}

// A multi-line agent message renders all its lines (not clipped to the first).
func TestMultilineAgentMessage(t *testing.T) {
	m, _, _ := newTestModel()
	m.height = 40 // plenty of room
	m.apply(journal.Record{Seq: 1, Kind: journal.KindAgentEvent, Actor: "claude",
		Data: agent.Event{Type: agent.EventText, Text: "Once approved I'll:\n- step one\n- step two"}})
	v := m.render()
	for _, want := range []string{"Once approved I'll:", "step one", "step two"} {
		if !strings.Contains(v, want) {
			t.Fatalf("render missing %q:\n%s", want, v)
		}
	}
}
