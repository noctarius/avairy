// Package cost tracks per-agent spend from the event journal and enforces optional budget caps
// (#26): when an agent (or the fleet total) crosses its cap, OnExceed fires once so core can warn
// the operator and interrupt the runaway. Spend is read off agent turn_done usage events.
package cost

import (
	"sync"

	"avairy/internal/agent"
	"avairy/internal/journal"
)

// Spend is accumulated usage.
type Spend struct {
	CostUSD      float64
	InputTokens  int
	OutputTokens int
}

// Monitor folds turn_done usage into per-agent + total spend and fires OnExceed once per scope that
// crosses its cap. Safe for concurrent Snapshot while Observe runs (one Run goroutine drives it).
type Monitor struct {
	agentCap, totalCap float64 // 0 = uncapped

	// OnExceed fires once when an agent (scope "agent") or the fleet (scope "total", agentID "")
	// first crosses its cap. Wire it to warn the operator + interrupt.
	OnExceed func(agentID, scope string, spent float64)

	mu       sync.Mutex
	perAgent map[string]Spend
	total    Spend
	flagged  map[string]bool
}

// New returns a Monitor with the given per-agent and total caps (0 = uncapped).
func New(agentCap, totalCap float64) *Monitor {
	return &Monitor{agentCap: agentCap, totalCap: totalCap, perAgent: map[string]Spend{}, flagged: map[string]bool{}}
}

// Run drains journal records into Observe until sub is closed (cancel by closing the subscription).
func (m *Monitor) Run(sub <-chan journal.Record) {
	for rec := range sub {
		m.Observe(rec)
	}
}

// Observe folds one record's usage and fires OnExceed for any cap newly crossed.
func (m *Monitor) Observe(rec journal.Record) {
	if rec.Kind != journal.KindAgentEvent {
		return
	}
	ev, ok := rec.Data.(agent.Event)
	if !ok || ev.Type != agent.EventTurnDone || ev.Usage == nil {
		return // usage rides turn_done; match the TUI's accounting (no double count)
	}
	type breach struct {
		id, scope string
		spent     float64
	}
	var breaches []breach

	m.mu.Lock()
	s := m.perAgent[rec.Actor]
	s.CostUSD += ev.Usage.CostUSD
	s.InputTokens += ev.Usage.InputTokens
	s.OutputTokens += ev.Usage.OutputTokens
	m.perAgent[rec.Actor] = s
	m.total.CostUSD += ev.Usage.CostUSD
	m.total.InputTokens += ev.Usage.InputTokens
	m.total.OutputTokens += ev.Usage.OutputTokens

	if m.agentCap > 0 && s.CostUSD >= m.agentCap && !m.flagged["a:"+rec.Actor] {
		m.flagged["a:"+rec.Actor] = true
		breaches = append(breaches, breach{rec.Actor, "agent", s.CostUSD})
	}
	if m.totalCap > 0 && m.total.CostUSD >= m.totalCap && !m.flagged["total"] {
		m.flagged["total"] = true
		breaches = append(breaches, breach{"", "total", m.total.CostUSD})
	}
	m.mu.Unlock()

	for _, b := range breaches {
		if m.OnExceed != nil {
			m.OnExceed(b.id, b.scope, b.spent)
		}
	}
}

// Snapshot returns a copy of per-agent spend and the fleet total.
func (m *Monitor) Snapshot() (map[string]Spend, Spend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Spend, len(m.perAgent))
	for k, v := range m.perAgent {
		out[k] = v
	}
	return out, m.total
}
