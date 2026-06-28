// Package journal is avairy's event-sourced log (DESIGN.md §10): an append-only record of
// everything that happens — bus messages, agent events, task changes, handovers, approvals.
// It is the durable source of truth for resume and the audit/handover timeline; the
// blackboard and task board are materialized views over it.
//
// This file is the in-memory Log; the durable append-only JSONL variant is in file.go (it embeds
// Memory and persists each record). The Log interface is the seam between them.
package journal

import (
	"sync"
	"time"
)

// Kind classifies a journal record.
type Kind string

const (
	KindMessage    Kind = "message"     // a bus message
	KindAgentEvent Kind = "agent_event" // a normalized agent stream event
	KindTask       Kind = "task"        // task posted / claimed / state change
	KindHandover   Kind = "handover"    // work changed hands (TUI handover timeline)
	KindApproval   Kind = "approval"    // gated-action decision
	KindNote       Kind = "note"        // blackboard entry (durable shared memory)
	KindSystem     Kind = "system"      // lifecycle / diagnostic
)

// Record is one immutable entry in the log.
type Record struct {
	Seq   uint64
	Time  time.Time
	Kind  Kind
	Actor string // agent id, "human", "facilitator", or "" for system
	Data  any    // kind-specific payload (serialized at the persistence boundary)
}

// Log is an append-only event log with replay and live subscription.
type Log interface {
	// Append records an entry and returns it (with Seq/Time assigned).
	Append(kind Kind, actor string, data any) Record
	// Records returns a snapshot of all entries so far, in order.
	Records() []Record
	// Subscribe returns a channel of records appended after the call, plus a cancel func.
	Subscribe() (<-chan Record, func())
}

// Memory is an in-memory Log.
type Memory struct {
	mu      sync.Mutex
	seq     uint64
	records []Record
	subs    map[int]chan Record
	nextSub int
}

// NewMemory returns an empty in-memory log.
func NewMemory() *Memory {
	return &Memory{subs: make(map[int]chan Record)}
}

func (m *Memory) Append(kind Kind, actor string, data any) Record {
	m.mu.Lock()
	m.seq++
	rec := Record{Seq: m.seq, Time: time.Now(), Kind: kind, Actor: actor, Data: data}
	m.records = append(m.records, rec)
	subs := make([]chan Record, 0, len(m.subs))
	for _, ch := range m.subs {
		subs = append(subs, ch)
	}
	m.mu.Unlock()

	for _, ch := range subs {
		// Non-blocking: a slow subscriber must not stall the log.
		select {
		case ch <- rec:
		default:
		}
	}
	return rec
}

// Restore seeds the log with prior records (decoded from a persisted journal) so Records()
// returns history after a restart — the TUI replays them to rebuild its view. Call once at
// startup, before any Append or Subscribe; Seq continues past the highest restored entry.
func (m *Memory) Restore(records []Record) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, records...)
	for _, r := range records {
		if r.Seq > m.seq {
			m.seq = r.Seq
		}
	}
}

func (m *Memory) Records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Record, len(m.records))
	copy(out, m.records)
	return out
}

func (m *Memory) Subscribe() (<-chan Record, func()) {
	m.mu.Lock()
	id := m.nextSub
	m.nextSub++
	ch := make(chan Record, 256)
	m.subs[id] = ch
	m.mu.Unlock()

	cancel := func() {
		m.mu.Lock()
		if c, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(c)
		}
		m.mu.Unlock()
	}
	return ch, cancel
}
