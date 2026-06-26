// Package facilitator is avairy's minimal facilitator (DESIGN.md §5): a trigger-invoked
// observer that nudges stuck agents — it reminds, never commands. The coordinator half is
// deterministic, cheap stuck-detection over the event-sourced journal; the "what to say"
// half is a pluggable Nudger (rule-based by default; an LLM nudger can drop in later).
//
// Nudges it can emit (all just bus messages, reusing steer/interrupt delivery):
//   - "another agent is better positioned" (capability matchmaking — e.g. can't reproduce
//     locally → the agent on that OS can)
//   - "ask a peer for their opinion"
//   - "ask the human for a decision"
package facilitator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"avairy/internal/agent"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

// TriggerKind classifies a detected stuck condition.
type TriggerKind string

const (
	TriggerBlocked TriggerKind = "blocked" // agent self-declared blocked/low-confidence
	TriggerLoop    TriggerKind = "loop"    // agent repeating the same step
)

// Trigger is a detected stuck condition for one agent.
type Trigger struct {
	Kind   TriggerKind
	Agent  string
	Detail string
}

// Agent is a roster entry used for capability matchmaking.
type Agent struct {
	ID   string
	Caps map[string]string
}

// Roster supplies the current agents and their capabilities.
type Roster interface{ Agents() []Agent }

// RosterFunc adapts a func to Roster.
type RosterFunc func() []Agent

func (f RosterFunc) Agents() []Agent { return f() }

// NudgeKind selects how a nudge is delivered.
type NudgeKind string

const (
	NudgeRemind   NudgeKind = "nudge"    // to a specific agent
	NudgeConsult  NudgeKind = "consult"  // suggest a specific peer
	NudgeEscalate NudgeKind = "escalate" // surface a decision to the human
)

// Nudge is the facilitator's suggested message.
type Nudge struct {
	Kind NudgeKind
	To   string // target agent id (empty for escalate/broadcast)
	Body string
}

// Nudger decides what (if anything) to say for a trigger, given the roster. Stateless per
// trigger — the seam where an LLM facilitator plugs in.
type Nudger interface {
	Decide(t Trigger, roster []Agent) []Nudge
}

// Facilitator detects triggers from journal records and publishes nudges to the bus.
type Facilitator struct {
	bus      *bus.Bus
	roster   Roster
	nudger   Nudger
	loopN    int           // identical consecutive steps that count as a loop
	cooldown time.Duration // min gap between nudges for the same (agent, trigger)
	now      func() time.Time

	mu        sync.Mutex
	recent    map[string][]string  // agent -> recent activity signatures
	lastNudge map[string]time.Time // (agent, trigger) -> when we last nudged it
}

// New builds a Facilitator publishing as "facilitator" onto b.
func New(b *bus.Bus, roster Roster, nudger Nudger) *Facilitator {
	return &Facilitator{
		bus:       b,
		roster:    roster,
		nudger:    nudger,
		loopN:     3,
		cooldown:  45 * time.Second,
		now:       time.Now,
		recent:    make(map[string][]string),
		lastNudge: make(map[string]time.Time),
	}
}

// Run feeds journal records to Observe until ctx is cancelled or sub closes.
func (f *Facilitator) Run(ctx context.Context, sub <-chan journal.Record) {
	for {
		select {
		case <-ctx.Done():
			return
		case rec, ok := <-sub:
			if !ok {
				return
			}
			f.Observe(rec)
		}
	}
}

// Observe inspects one record; on a detected trigger it asks the nudger and publishes — unless
// that (agent, trigger) was nudged within the cooldown, which keeps a flapping agent from
// being nudged on every status report. An agent reporting progress clears its blocked
// cooldown, so a genuine later block nudges promptly instead of waiting out the window.
func (f *Facilitator) Observe(rec journal.Record) {
	if agentID, status, ok := reportStatus(rec); ok && status != "blocked" && status != "low_confidence" {
		f.clearCooldown(agentID, TriggerBlocked)
	}
	t, ok := f.detect(rec)
	if !ok || f.suppressed(t) {
		return
	}
	for _, n := range f.nudger.Decide(t, f.roster.Agents()) {
		f.publish(n)
	}
	f.markNudged(t)
}

// reportStatus extracts (agent, status) from a report_status system record.
func reportStatus(rec journal.Record) (agentID, status string, ok bool) {
	if rec.Kind != journal.KindSystem {
		return "", "", false
	}
	m, isMap := rec.Data.(map[string]any)
	if !isMap || m["event"] != "report_status" {
		return "", "", false
	}
	status, _ = m["status"].(string)
	return rec.Actor, status, true
}

func (f *Facilitator) detect(rec journal.Record) (Trigger, bool) {
	switch rec.Kind {
	case journal.KindSystem:
		_, status, ok := reportStatus(rec)
		if !ok {
			return Trigger{}, false
		}
		if status == "blocked" || status == "low_confidence" {
			detail, _ := rec.Data.(map[string]any)["detail"].(string)
			return Trigger{Kind: TriggerBlocked, Agent: rec.Actor, Detail: detail}, true
		}
	case journal.KindAgentEvent:
		ev, ok := rec.Data.(agent.Event)
		if !ok {
			return Trigger{}, false
		}
		if sig := signature(ev); sig != "" {
			return f.trackLoop(rec.Actor, sig)
		}
	}
	return Trigger{}, false
}

// trackLoop records an activity signature and fires a loop trigger after loopN identical
// consecutive steps (then resets, to avoid repeat-firing on the same loop).
func (f *Facilitator) trackLoop(agentID, sig string) (Trigger, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := append(f.recent[agentID], sig)
	if len(r) > f.loopN {
		r = r[len(r)-f.loopN:]
	}
	f.recent[agentID] = r
	if len(r) == f.loopN && allEqual(r) {
		f.recent[agentID] = nil
		return Trigger{Kind: TriggerLoop, Agent: agentID, Detail: sig}, true
	}
	return Trigger{}, false
}

func (f *Facilitator) publish(n Nudge) {
	switch n.Kind {
	case NudgeEscalate:
		f.bus.Publish("facilitator", bus.Broadcast(), "needs a human decision: "+n.Body, agent.DeliverySteer)
	default:
		to := bus.Broadcast()
		if n.To != "" {
			to = bus.Agent(n.To)
		}
		f.bus.Publish("facilitator", to, n.Body, agent.DeliverySteer)
	}
}

func nudgeKey(agentID string, k TriggerKind) string { return agentID + "\x00" + string(k) }

// suppressed reports whether this (agent, trigger) was nudged within the cooldown window.
func (f *Facilitator) suppressed(t Trigger) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	last, ok := f.lastNudge[nudgeKey(t.Agent, t.Kind)]
	return ok && f.now().Sub(last) < f.cooldown
}

func (f *Facilitator) markNudged(t Trigger) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastNudge[nudgeKey(t.Agent, t.Kind)] = f.now()
}

func (f *Facilitator) clearCooldown(agentID string, k TriggerKind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.lastNudge, nudgeKey(agentID, k))
}

func signature(ev agent.Event) string {
	switch ev.Type {
	case agent.EventToolUse:
		if ev.Tool != nil {
			return "tool:" + ev.Tool.Name + ":" + fmt.Sprint(ev.Tool.Input)
		}
	case agent.EventText:
		return "text:" + ev.Text
	}
	return ""
}

func allEqual(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return len(s) > 0
}
