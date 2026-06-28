// Package mock provides a scriptable agent.Adapter for deterministic, zero-credit testing
// of the bus/runner/journal loop without invoking a real agent CLI.
package mock

import (
	"context"
	"strconv"
	"sync"

	"avairy/internal/agent"
)

// Reply maps an inbound message to the events the mock emits in response. The returned
// events are streamed in order; a terminal turn_done is appended automatically unless the
// script already ends with one.
type Reply func(input string) []agent.Event

// EchoReply emits the input back as assistant text, then completes the turn.
func EchoReply(input string) []agent.Event {
	return []agent.Event{{Type: agent.EventText, Text: input}}
}

// Adapter is a mock agent family.
type Adapter struct {
	Caps         agent.Capabilities
	Reply        Reply // defaults to EchoReply
	InterruptErr error // returned by session.Interrupt; non-nil mimics a family that can't be interrupted (e.g. claude)
}

// New returns a mock Adapter with steer/resume capabilities and an echo script.
func New() *Adapter {
	return &Adapter{
		Caps:  agent.Capabilities{SupportsSteer: true, SupportsResume: true, MCPClient: true, Enforcement: agent.EnforcementAdvisory},
		Reply: EchoReply,
	}
}

func (a *Adapter) Family() agent.Family             { return agent.Family("mock") }
func (a *Adapter) Capabilities() agent.Capabilities { return a.Caps }

func (a *Adapter) Start(ctx context.Context, cfg agent.SessionConfig) (agent.Session, error) {
	reply := a.Reply
	if reply == nil {
		reply = EchoReply
	}
	id := cfg.AgentID
	if id == "" {
		id = "mock"
	}
	return &session{id: id, reply: reply, interruptErr: a.InterruptErr, events: make(chan agent.Event, 64)}, nil
}

type session struct {
	id           string
	reply        Reply
	interruptErr error
	events       chan agent.Event

	mu     sync.Mutex
	closed bool
	turns  int
}

func (s *session) ID() string { return s.id }

func (s *session) Send(ctx context.Context, text string, d agent.Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return context.Canceled
	}
	s.turns++
	evs := s.reply(text)
	for _, e := range evs {
		s.events <- e
	}
	// Ensure a terminal turn_done so the runner sees a turn boundary.
	if len(evs) == 0 || evs[len(evs)-1].Type != agent.EventTurnDone {
		s.events <- agent.Event{
			Type:  agent.EventTurnDone,
			Usage: &agent.Usage{OutputTokens: len(text)},
			Raw:   []byte("turn:" + strconv.Itoa(s.turns)),
		}
	}
	return nil
}

func (s *session) Events() <-chan agent.Event { return s.events }

func (s *session) Interrupt(ctx context.Context) error { return s.interruptErr }

func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
	}
	return nil
}
