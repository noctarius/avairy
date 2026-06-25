package tui

import (
	"strings"
	"testing"

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

	v := m.View()
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
