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
)

// Addr is a message destination.
type Addr struct {
	Kind  ToKind
	Value string
}

func Agent(id string) Addr { return Addr{ToAgent, id} }
func Role(r string) Addr   { return Addr{ToRole, r} }
func Broadcast() Addr      { return Addr{ToBroadcast, ""} }

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
}

type subscriber struct {
	agentID string
	roles   map[string]bool
	ch      chan Message
}

// New returns a Bus that records to jrnl.
func New(jrnl journal.Log) *Bus {
	return &Bus{jrnl: jrnl, subs: make(map[int]*subscriber)}
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
	case ToBroadcast:
		return true
	case ToAgent:
		return s.agentID == msg.To.Value
	case ToRole:
		return s.roles[msg.To.Value]
	default:
		return false
	}
}
