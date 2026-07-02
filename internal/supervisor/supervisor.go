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

// Spawn starts a fresh agent session bound to the given context, using the given model/effort. The
// supervisor calls it at startup and on each respawn; cancelling the passed context must close the
// session. model/effort let a respawn pick up a reconfigure that couldn't be applied live.
type Spawn func(ctx context.Context, model, effort string) (agent.Session, error)

type reconfigReq struct{ model, effort string } // "" = leave that field unchanged

// Supervisor drives one agent's session against the bus, sleeping/respawning it on idle.
type Supervisor struct {
	id       string
	roles    []string
	spawn    Spawn
	b        *bus.Bus
	jrnl     journal.Log
	idle     time.Duration // 0 = never sleep
	waker    *bus.Waker
	reconfig chan reconfigReq
	turnDone chan struct{} // event goroutine pokes this when a turn ends, so pending respawns fire

	mu         sync.Mutex
	lastActive time.Time
	working    bool   // mid-turn (last event wasn't turn_done) — never sleep while true
	model      string // desired model/effort; a respawn uses these
	effort     string
}

// New builds a Supervisor for an agent. idle is the quiet period before teardown (0 disables it).
// model/effort are the initial values (also what respawns use until a reconfigure changes them).
func New(id string, roles []string, spawn Spawn, b *bus.Bus, jrnl journal.Log, idle time.Duration, model, effort string) *Supervisor {
	return &Supervisor{
		id: id, roles: roles, spawn: spawn, b: b, jrnl: jrnl, idle: idle, waker: bus.NewWaker(),
		model: model, effort: effort,
		reconfig: make(chan reconfigReq, 8),
		turnDone: make(chan struct{}, 1),
	}
}

// Reconfigure requests a model/effort change on the running agent (from the operator). Applied live
// where the family supports it, else deferred to the next idle boundary as a respawn. "" leaves a
// field unchanged. Best-effort: dropped only if a burst overflows the small queue.
func (s *Supervisor) Reconfigure(model, effort string) {
	select {
	case s.reconfig <- reconfigReq{model: model, effort: effort}:
	default:
	}
}

// Run subscribes to the bus and drives the agent until ctx is cancelled or its inbox closes. It
// blocks; run it in its own goroutine. The session is spawned immediately (awake); idle teardown
// and lazy respawn happen inside the loop.
func (s *Supervisor) Run(ctx context.Context) {
	inbox, cancel := s.b.Subscribe(s.id, s.roles...)
	defer cancel()

	var (
		sess            agent.Session
		sessCancel      context.CancelFunc
		dead            chan struct{} // closed when the current session's event stream ends
		pendingReconfig bool          // a reconfigure awaits the next idle boundary to respawn
	)

	// wakeUp spawns a session (if asleep) and starts draining its events into the journal. wasAsleep
	// distinguishes the startup spawn (no event) from a lazy respawn (journals agent_awake).
	wakeUp := func(wasAsleep bool) bool {
		if sess != nil {
			return true
		}
		sctx, sc := context.WithCancel(ctx)
		s.mu.Lock()
		model, effort := s.model, s.effort
		s.mu.Unlock()
		ns, err := s.spawn(sctx, model, effort)
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
				if ev.Type == agent.EventTurnDone { // idle boundary: let a pending respawn fire
					select {
					case s.turnDone <- struct{}{}:
					default:
					}
				}
			}
		}(ns.Events())
		pendingReconfig = false // the fresh session already carries the current model/effort
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

	// respawn tears the current session down and starts a fresh one with the current model/effort —
	// used to apply a reconfigure a family can't do live. Not marked sleeping (it's continuous).
	respawn := func() {
		if sessCancel != nil {
			sessCancel()
		}
		if sess != nil {
			_ = sess.Close()
		}
		sess, sessCancel, dead = nil, nil, nil
		s.mu.Lock()
		s.working = false
		s.mu.Unlock()
		wakeUp(false)
	}
	// maybeRespawn applies a pending reconfigure once the agent is idle (never mid-turn). If it's
	// asleep, nothing to do — the stored model/effort is used on the next wake.
	maybeRespawn := func() {
		if !pendingReconfig {
			return
		}
		if sess == nil {
			pendingReconfig = false
			return
		}
		s.mu.Lock()
		working := s.working
		s.mu.Unlock()
		if !working {
			respawn()
		}
	}
	// applyReconfig records the desired model/effort and applies it: live via the session's
	// Reconfigurer where the family supports it, else a respawn deferred to the next idle boundary.
	applyReconfig := func(req reconfigReq) {
		s.mu.Lock()
		if req.model != "" {
			s.model = req.model
		}
		if req.effort != "" {
			s.effort = req.effort
		}
		model, effort := s.model, s.effort
		s.mu.Unlock()
		if rc, ok := sess.(agent.Reconfigurer); ok && sess != nil && rc.Reconfigure(ctx, req.model, req.effort) == nil {
			pendingReconfig = false
			s.jrnl.Append(journal.KindSystem, s.id, map[string]any{"event": "reconfigured", "model": model, "effort": effort, "applied": "live"})
			return
		}
		pendingReconfig = true
		s.jrnl.Append(journal.KindSystem, s.id, map[string]any{"event": "reconfigured", "model": model, "effort": effort, "applied": "pending"})
		maybeRespawn()
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
		case req := <-s.reconfig:
			applyReconfig(req)
		case <-s.turnDone:
			maybeRespawn() // a turn just ended — apply a pending reconfigure now
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
			if msg.NoWake {
				continue // context-only (e.g. a 👍/👎 reaction): already in read_inbox; don't trigger a turn
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
			// Signal the turn is starting the instant we dispatch, so the consoles show "working"
			// during the think gap before the first token — not only once the first event lands.
			s.mu.Lock()
			s.working = true
			s.mu.Unlock()
			s.jrnl.Append(journal.KindAgentEvent, s.id, agent.Event{Type: agent.EventTurnStart})
			text := bus.AnnotateDelivery(msg.ID, msg.To.Kind, msg.Body)
			if err := sess.Send(ctx, text, msg.Delivery); err != nil && msg.Delivery == agent.DeliveryInterrupt {
				_ = sess.Send(ctx, text, agent.DeliverySteer)
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
