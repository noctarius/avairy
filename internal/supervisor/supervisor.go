// Package supervisor owns a core-local agent's session lifecycle (DESIGN.md §5, item #28). Like the
// runner it bridges the bus and journal — inbound messages steer/interrupt the session, the session's
// events are journaled — but it adds idle teardown: after a quiet period it closes the agent's
// subprocess ("sleeping"), freeing context/credits, and lazily respawns it on the next wake-worthy
// directed message. With idle == 0 it never sleeps and behaves exactly like a runner.
package supervisor

import (
	"context"
	"sync"
	"time"

	"avairy/internal/agent"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

// Spawn starts a fresh agent session bound to the given context. The supervisor calls it once at
// startup and again on each lazy respawn; cancelling the passed context must close the session.
type Spawn func(ctx context.Context) (agent.Session, error)

// Supervisor drives one agent's session against the bus, sleeping/respawning it on idle.
type Supervisor struct {
	id    string
	roles []string
	spawn Spawn
	b     *bus.Bus
	jrnl  journal.Log
	idle  time.Duration // 0 = never sleep
	waker *bus.Waker

	mu         sync.Mutex
	lastActive time.Time
	working    bool // mid-turn (last event wasn't turn_done) — never sleep while true
}

// New builds a Supervisor for an agent. idle is the quiet period before teardown (0 disables it).
func New(id string, roles []string, spawn Spawn, b *bus.Bus, jrnl journal.Log, idle time.Duration) *Supervisor {
	return &Supervisor{id: id, roles: roles, spawn: spawn, b: b, jrnl: jrnl, idle: idle, waker: bus.NewWaker()}
}

// Run subscribes to the bus and drives the agent until ctx is cancelled or its inbox closes. It
// blocks; run it in its own goroutine. The session is spawned immediately (awake); idle teardown
// and lazy respawn happen inside the loop.
func (s *Supervisor) Run(ctx context.Context) {
	inbox, cancel := s.b.Subscribe(s.id, s.roles...)
	defer cancel()

	var (
		sess       agent.Session
		sessCancel context.CancelFunc
		dead       chan struct{} // closed when the current session's event stream ends
	)

	// wakeUp spawns a session (if asleep) and starts draining its events into the journal. wasAsleep
	// distinguishes the startup spawn (no event) from a lazy respawn (journals agent_awake).
	wakeUp := func(wasAsleep bool) bool {
		if sess != nil {
			return true
		}
		sctx, sc := context.WithCancel(ctx)
		ns, err := s.spawn(sctx)
		if err != nil {
			sc()
			s.jrnl.Append(journal.KindSystem, s.id, map[string]any{"event": "agent_error", "error": err.Error()})
			return false
		}
		sess, sessCancel = ns, sc
		d := make(chan struct{})
		dead = d
		s.touch()
		go func(events <-chan agent.Event) {
			defer close(d)
			for ev := range events {
				s.jrnl.Append(journal.KindAgentEvent, s.id, ev)
				s.mu.Lock()
				s.lastActive = time.Now()
				s.working = ev.Type != agent.EventTurnDone
				s.mu.Unlock()
			}
		}(ns.Events())
		if wasAsleep {
			s.jrnl.Append(journal.KindSystem, s.id, map[string]any{"event": "agent_awake"})
		}
		return true
	}

	// sleep tears the session down and marks the agent sleeping; the bus subscription stays so a
	// later directed message can wake it. Sets sess=nil first so the dead signal is recognized as
	// intentional (not a crash).
	sleep := func() {
		if sess == nil {
			return
		}
		c := sessCancel
		toClose := sess
		sess, sessCancel = nil, nil
		c()
		_ = toClose.Close()
		s.mu.Lock()
		s.working = false
		s.mu.Unlock()
		s.markSleeping()
	}

	wakeUp(false) // start awake (a spawn failure is retried on the next message)

	// Poll for idleness at half the idle period (bounded so a tiny idle still ticks sanely).
	var tickC <-chan time.Time
	if s.idle > 0 {
		every := s.idle / 2
		if every < 50*time.Millisecond {
			every = 50 * time.Millisecond
		}
		t := time.NewTicker(every)
		defer t.Stop()
		tickC = t.C
	}

	for {
		select {
		case <-ctx.Done():
			if sess != nil {
				if sessCancel != nil {
					sessCancel()
				}
				_ = sess.Close()
			}
			return
		case <-dead:
			// The session's stream ended. If we didn't intend it (sess still set), the subprocess
			// died — drop to sleeping so the next message respawns it.
			if sess != nil {
				sess, sessCancel, dead = nil, nil, nil
				s.markSleeping()
			} else {
				dead = nil
			}
		case <-tickC:
			if sess != nil && s.idleElapsed() {
				sleep()
			}
		case msg, ok := <-inbox:
			if !ok {
				if sessCancel != nil {
					sessCancel()
				}
				return
			}
			if msg.Interrupt {
				// Cancel the turn in-band; if the family can't (e.g. claude), hard-stop by closing
				// the subprocess so Stop actually stops it. It respawns on the next directed message.
				if sess != nil && sess.Interrupt(ctx) != nil {
					sleep()
				}
				continue
			}
			// Only wake-worthy messages (direct, or human/facilitator broadcast, within the
			// autonomous-wake budget — #25) trigger a turn or a respawn. Context-only chatter is
			// ignored here; a sleeping agent stays asleep.
			if !s.waker.Wake(msg.From, msg.To.Kind, false, time.Now()) {
				continue
			}
			if sess == nil {
				if !wakeUp(true) {
					continue
				}
			}
			s.touch()
			if err := sess.Send(ctx, msg.Body, msg.Delivery); err != nil && msg.Delivery == agent.DeliveryInterrupt {
				_ = sess.Send(ctx, msg.Body, agent.DeliverySteer)
			}
		}
	}
}

// markSleeping journals the agent's transition to the sleeping state (shown in the consoles).
func (s *Supervisor) markSleeping() {
	s.jrnl.Append(journal.KindSystem, s.id, map[string]any{"event": "agent_sleeping"})
}

func (s *Supervisor) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

// idleElapsed reports whether the agent has been quiet (and not mid-turn) for at least idle.
func (s *Supervisor) idleElapsed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.working && time.Since(s.lastActive) >= s.idle
}
