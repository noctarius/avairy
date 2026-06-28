// Package bus is avairy's message router (DESIGN.md §4): agents, the facilitator, and the
// human exchange addressed messages over it. Every message is recorded to the journal and
// routed to matching subscribers. The sender is stamped by the caller (the MCP layer
// enforces no-spoofing; here the API trusts its caller).
package bus

import (
	"strconv"
	"sync"
	"time"

	"avairy/internal/agent"
	"avairy/internal/journal"
)

// ToKind selects how a message is addressed.
type ToKind string

const (
	ToAgent     ToKind = "agent"     // a specific agent id
	ToRole      ToKind = "role"      // fan-out to everyone holding a role
	ToBroadcast ToKind = "broadcast" // everyone but the sender
	ToTeam      ToKind = "team"      // everyone, but exactly one should claim it and answer (claim_response)
)

// Senders whose messages always wake the recipient (#25): the operator and the facilitator are
// low-frequency and intentional, so their broadcasts/role messages aren't throttled.
const (
	SenderHuman       = "human"
	SenderFacilitator = "facilitator"
)

// dedupWindow drops an identical (from,to,body) message repeated within this window (anti-storm).
const dedupWindow = 2 * time.Second

// Addr is a message destination.
type Addr struct {
	Kind  ToKind
	Value string
}

func Agent(id string) Addr { return Addr{ToAgent, id} }
func Role(r string) Addr   { return Addr{ToRole, r} }
func Broadcast() Addr      { return Addr{ToBroadcast, ""} }
func Team() Addr           { return Addr{ToTeam, ""} }

// Message is one routed message.
type Message struct {
	ID        string
	From      string // agent id, "human", or "facilitator"
	To        Addr
	Body      string
	Delivery  agent.Delivery
	Interrupt bool // a control signal: cancel the recipient's current turn (not a text message)
	Time      time.Time
}

// Bus routes messages and records them to the journal.
type Bus struct {
	jrnl journal.Log

	mu      sync.RWMutex
	seq     uint64
	subs    map[int]*subscriber
	nextSub int
	recent  map[string]time.Time // (from,to,body) -> last publish, for dedup (#25)
}

type subscriber struct {
	agentID string
	roles   map[string]bool
	ch      chan Message
}

// New returns a Bus that records to jrnl.
func New(jrnl journal.Log) *Bus {
	return &Bus{jrnl: jrnl, subs: make(map[int]*subscriber), recent: make(map[string]time.Time)}
}

// Waker decides whether a delivered message WAKES its recipient (triggers a turn) or is delivered
// context-only (#25). One per agent at its activation point (runner / node pull-loop), so the reply
// budget is per-agent. NOT concurrency-safe — each activation point drives it from one goroutine.
type Waker struct {
	budget int
	window time.Duration
	recent []time.Time
}

// NewWaker allows up to 6 autonomous (agent-originated, direct) wakes per 30s before further agent
// messages fall to context-only until the recipient goes quiet.
func NewWaker() *Waker { return &Waker{budget: 6, window: 30 * time.Second} }

// Wake reports whether a message from `from`, addressed by `kind`, should wake the recipient now.
// Interrupts and human/facilitator messages always pass; agent broadcast/role is context-only;
// agent direct messages wake within the per-agent rate budget (over budget → context-only).
func (w *Waker) Wake(from string, kind ToKind, interrupt bool, now time.Time) bool {
	if interrupt || from == SenderHuman || from == SenderFacilitator {
		return true
	}
	if kind != ToAgent && kind != ToTeam {
		return false // agent → broadcast/role: deliver as context, don't trigger a turn
	}
	// agent → team: a coordination request; wake within the budget so a peer can claim it.
	cut := now.Add(-w.window)
	kept := w.recent[:0]
	for _, t := range w.recent {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	w.recent = kept
	if len(w.recent) >= w.budget {
		return false // autonomous-wake budget exhausted → context-only until it cools off
	}
	w.recent = append(w.recent, now)
	return true
}

// Subscribe registers a participant by agent id and roles; returns its inbox and a cancel.
func (b *Bus) Subscribe(agentID string, roles ...string) (<-chan Message, func()) {
	rs := make(map[string]bool, len(roles))
	for _, r := range roles {
		rs[r] = true
	}
	b.mu.Lock()
	id := b.nextSub
	b.nextSub++
	s := &subscriber{agentID: agentID, roles: rs, ch: make(chan Message, 64)}
	b.subs[id] = s
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		if cur, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(cur.ch)
		}
		b.mu.Unlock()
	}
	return s.ch, cancel
}

// Publish stamps, journals, and routes a text message, returning the stamped message.
func (b *Bus) Publish(from string, to Addr, body string, d agent.Delivery) Message {
	return b.publish(Message{From: from, To: to, Body: body, Delivery: d})
}

// Interrupt sends a control signal telling the recipient(s) to cancel their current turn.
func (b *Bus) Interrupt(from string, to Addr) Message {
	return b.publish(Message{From: from, To: to, Body: "⎋ stop", Delivery: agent.DeliveryInterrupt, Interrupt: true})
}

func (b *Bus) publish(msg Message) Message {
	b.mu.Lock()
	// Dedup: an identical (from,to,body) repeated within the window is dropped — not journaled, not
	// routed (#25). Control signals are never deduped.
	if !msg.Interrupt {
		key := msg.From + "\x00" + string(msg.To.Kind) + ":" + msg.To.Value + "\x00" + msg.Body
		now := time.Now()
		if last, ok := b.recent[key]; ok && now.Sub(last) < dedupWindow {
			b.mu.Unlock()
			return msg // duplicate within the window → drop
		}
		if len(b.recent) > 1024 {
			b.recent = make(map[string]time.Time, 64) // bounded; losing stale history is harmless
		}
		b.recent[key] = now
	}
	b.seq++
	msg.ID = "m" + strconv.FormatUint(b.seq, 10)
	msg.Time = time.Now()
	targets := make([]*subscriber, 0, len(b.subs))
	for _, s := range b.subs {
		if b.matches(s, msg) {
			targets = append(targets, s)
		}
	}
	b.mu.Unlock()

	b.jrnl.Append(journal.KindMessage, msg.From, msg)

	for _, s := range targets {
		select {
		case s.ch <- msg:
		default: // drop to a full inbox rather than block the publisher
		}
	}
	return msg
}

func (b *Bus) matches(s *subscriber, msg Message) bool {
	if s.agentID == msg.From {
		return false // never echo a message back to its sender
	}
	switch msg.To.Kind {
	case ToBroadcast, ToTeam:
		return true // everyone sees it; for a team request, one will claim it and the rest stand down
	case ToAgent:
		return s.agentID == msg.To.Value
	case ToRole:
		return s.roles[msg.To.Value]
	default:
		return false
	}
}
