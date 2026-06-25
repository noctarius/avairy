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
