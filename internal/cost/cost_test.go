package cost

import (
	"math"
	"testing"

	"avairy/internal/agent"
	"avairy/internal/journal"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func turnDone(actor string, usd float64, in, out int) journal.Record {
	return journal.Record{
		Kind:  journal.KindAgentEvent,
		Actor: actor,
		Data: agent.Event{
			Type:  agent.EventTurnDone,
			Usage: &agent.Usage{CostUSD: usd, InputTokens: in, OutputTokens: out},
		},
	}
}

func TestMonitorAccumulatesPerAgentAndTotal(t *testing.T) {
	m := New(0, 0)
	m.Observe(turnDone("alice", 0.10, 100, 50))
	m.Observe(turnDone("alice", 0.05, 20, 10))
	m.Observe(turnDone("bob", 0.20, 200, 80))
	// Non-usage records and non-turn_done events are ignored.
	m.Observe(journal.Record{Kind: journal.KindMessage, Actor: "alice", Data: "hi"})
	m.Observe(journal.Record{Kind: journal.KindAgentEvent, Actor: "alice", Data: agent.Event{Type: agent.EventUsage, Usage: &agent.Usage{CostUSD: 99}}})

	per, total := m.Snapshot()
	if got := per["alice"]; !near(got.CostUSD, 0.15) || got.InputTokens != 120 || got.OutputTokens != 60 {
		t.Fatalf("alice = %+v", got)
	}
	if got := per["bob"]; !near(got.CostUSD, 0.20) {
		t.Fatalf("bob = %+v", got)
	}
	if !near(total.CostUSD, 0.35) || total.InputTokens != 320 || total.OutputTokens != 140 {
		t.Fatalf("total = %+v", total)
	}
}

func TestMonitorFiresAgentCapOnce(t *testing.T) {
	var hits []string
	m := New(0.10, 0)
	m.OnExceed = func(id, scope string, spent float64) {
		hits = append(hits, id+":"+scope)
	}
	m.Observe(turnDone("alice", 0.06, 0, 0)) // under cap
	if len(hits) != 0 {
		t.Fatalf("fired early: %v", hits)
	}
	m.Observe(turnDone("alice", 0.06, 0, 0)) // crosses 0.10
	m.Observe(turnDone("alice", 0.06, 0, 0)) // stays over — must not refire
	if len(hits) != 1 || hits[0] != "alice:agent" {
		t.Fatalf("agent cap hits = %v", hits)
	}
}

func TestMonitorFiresTotalCap(t *testing.T) {
	var hits []string
	m := New(0, 0.25)
	m.OnExceed = func(id, scope string, spent float64) {
		hits = append(hits, scope)
	}
	m.Observe(turnDone("alice", 0.20, 0, 0))
	m.Observe(turnDone("bob", 0.10, 0, 0)) // total 0.30 ≥ 0.25
	m.Observe(turnDone("bob", 0.10, 0, 0)) // no refire
	if len(hits) != 1 || hits[0] != "total" {
		t.Fatalf("total cap hits = %v", hits)
	}
}
