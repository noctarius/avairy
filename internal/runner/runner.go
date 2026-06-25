// Package runner connects a running agent.Session to the bus and journal (DESIGN.md §5
// "agent activation"): inbound bus messages are delivered to the agent (steer/interrupt),
// and the agent's stream events are recorded to the journal. This is the per-agent loop the
// node daemon runs.
package runner

import (
	"context"

	"avairy/internal/agent"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

// Agent describes a running agent's bus identity.
type Agent struct {
	ID    string
	Roles []string
}

// Runner drives one agent session against the bus.
type Runner struct {
	a      Agent
	sess   agent.Session
	b      *bus.Bus
	jrnl   journal.Log
	inbox  <-chan bus.Message
	cancel func()
}

// New builds a Runner for sess identified by a. It subscribes to the bus immediately so the
// agent's inbox exists before Run starts (no lost messages).
func New(a Agent, sess agent.Session, b *bus.Bus, jrnl journal.Log) *Runner {
	inbox, cancel := b.Subscribe(a.ID, a.Roles...)
	return &Runner{a: a, sess: sess, b: b, jrnl: jrnl, inbox: inbox, cancel: cancel}
}

// Run pumps messages in / events out until ctx is cancelled or the session's event stream
// closes. It blocks; run it in its own goroutine.
func (r *Runner) Run(ctx context.Context) {
	inbox := r.inbox
	defer r.cancel()

	// Drain agent events → journal in the background.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range r.sess.Events() {
			r.jrnl.Append(journal.KindAgentEvent, r.a.ID, ev)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case msg, ok := <-inbox:
			if !ok {
				return
			}
			// Deliver to the agent; if interrupt isn't supported, Send errors and we
			// fall back to steer (queue at the next turn boundary).
			if err := r.sess.Send(ctx, msg.Body, msg.Delivery); err != nil && msg.Delivery == agent.DeliveryInterrupt {
				_ = r.sess.Send(ctx, msg.Body, agent.DeliverySteer)
			}
		}
	}
}
