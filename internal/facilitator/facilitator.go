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
	"slices"
	"strings"
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
	// FreshLook, if set, runs a clean-context one-shot session (DESIGN.md §8) — the facilitator's
	// primary tool against loops. On a detected loop it asks for an independent take and delivers
	// the answer to the stuck agent. Spawns a real session, so it's gated by the nudge cooldown.
	FreshLook func(ctx context.Context, question string) (string, error)

	mu        sync.Mutex
	recent    map[string][]string  // agent -> recent activity signatures
	stale     map[string]int       // agent -> consecutive actions with nothing never-seen (circling, #14a)
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
		stale:     make(map[string]int),
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

	// A loop is exactly what ephemeral fresh-look is for: get an unanchored take and hand it to
	// the stuck agent. Async — it spawns a session; the cooldown above already rate-limits it.
	if t.Kind == TriggerLoop && f.FreshLook != nil {
		go f.runFreshLook(t)
	}
}

func (f *Facilitator) runFreshLook(t Trigger) {
	q := fmt.Sprintf("Agent %q is stuck repeating the same step (%s). With no prior context or "+
		"assumptions, what is it most likely missing, and what concretely different approach should "+
		"it try? Be brief.", t.Agent, t.Detail)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ans, err := f.FreshLook(ctx, q)
	if err != nil || strings.TrimSpace(ans) == "" {
		return
	}
	f.bus.Publish("facilitator", bus.Agent(t.Agent), "fresh look (no prior context) on your loop — "+ans, agent.DeliverySteer)
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
			return f.trackLoop(rec.Actor, sig, mutatesFiles(ev.Tool))
		}
	}
	return Trigger{}, false
}

// loopMaxPeriod is the largest repeating cycle length trackLoop looks for.
const loopMaxPeriod = 4

// trackLoop records an agent's action signatures and fires a loop trigger when the recent
// history ends in a repeating cycle — the same block of 1..loopMaxPeriod actions repeated loopN
// times. Period 1 is the classic "same step over and over"; period ≥2 catches A↔B oscillation
// (ping-ponging between two fixes). Interleaved reasoning is already filtered out (signature
// only tracks tool actions), so retries with thinking in between still register. Resets on a
// hit to avoid re-firing on the same loop.
func (f *Facilitator) trackLoop(agentID, sig string, mutating bool) (Trigger, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := f.recent[agentID]
	// A file-mutating action repeated back-to-back (e.g. several Edits to the same file) is building
	// up a change, not looping — and the signature can't see the differing content, so the edits look
	// identical. Treat it as progress: don't grow the window with the duplicate, and clear the
	// circling counter. Interleaved repeats (edit↔test oscillation) are untouched and still detected.
	if mutating && len(old) > 0 && old[len(old)-1] == sig {
		f.stale[agentID] = 0
		return Trigger{}, false
	}
	novel := !slices.Contains(old, sig) // a never-seen-this-window action = progress
	w := append(old, sig)
	if maxLen := loopMaxPeriod * f.loopN; len(w) > maxLen {
		w = w[len(w)-maxLen:]
	}
	f.recent[agentID] = w

	// Periodic cycle: a block of 1..loopMaxPeriod actions repeated loopN times (period-1 repeats,
	// A↔B oscillation, interleaved retries).
	for p := 1; p <= loopMaxPeriod && p*f.loopN <= len(w); p++ {
		if isCycle(w[len(w)-p*f.loopN:], p) {
			f.resetLoop(agentID)
			return Trigger{Kind: TriggerLoop, Agent: agentID, Detail: cycleDetail(w[len(w)-p:])}, true
		}
	}

	// Aperiodic circling (#14a): the agent churns the same few actions in no fixed order, so there's
	// no period — but it introduces no NEW action for circleN steps. A novel action means progress
	// (resets); circleN consecutive non-novel steps means it's stuck recycling old moves.
	if novel {
		f.stale[agentID] = 0
	} else if f.stale[agentID]++; f.stale[agentID] >= circleN {
		detail := circlingDetail(w)
		f.resetLoop(agentID)
		return Trigger{Kind: TriggerLoop, Agent: agentID, Detail: detail}, true
	}
	return Trigger{}, false
}

// circleN is how many consecutive actions introducing nothing never-seen-this-window count as
// circling. Kept below the window (loopMaxPeriod*loopN) so a still-in-window action stays non-novel.
const circleN = 6

func (f *Facilitator) resetLoop(agentID string) {
	f.recent[agentID] = nil
	delete(f.stale, agentID)
}

// circlingDetail summarizes the churn for the handoff to a fresh-look agent: the distinct actions
// being recycled, in first-seen order. Enough context to analyze — without dumping the transcript.
func circlingDetail(w []string) string {
	seen := make(map[string]bool, len(w))
	var distinct []string
	for _, s := range w {
		a := strings.TrimPrefix(s, "tool:")
		if !seen[a] {
			seen[a] = true
			distinct = append(distinct, a)
		}
	}
	return "circling these actions without progress: " + strings.Join(distinct, ", ")
}

// isCycle reports whether tail is a single block of length p repeated (tail[i] == tail[i-p]).
func isCycle(tail []string, p int) bool {
	for i := p; i < len(tail); i++ {
		if tail[i] != tail[i-p] {
			return false
		}
	}
	return true
}

// cycleDetail renders the repeating block for the trigger ("Bash: make → Bash: test").
func cycleDetail(block []string) string {
	parts := make([]string, len(block))
	for i, s := range block {
		parts[i] = strings.TrimPrefix(s, "tool:")
	}
	return strings.Join(parts, " → ")
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

// signature is the per-step key for loop detection: only tool actions count (interleaved reasoning
// is ignored), keyed on the full action via agent.ActionKey — tool + identifying arg + a digest of
// the edit content + the read region — so reading 100 different files (or editing one file 100
// different ways) is 100 distinct steps, not a loop. Returns "" for events that don't count.
func signature(ev agent.Event) string {
	if ev.Type == agent.EventToolUse && ev.Tool != nil {
		return "tool:" + agent.ActionKey(ev.Tool)
	}
	return ""
}

// fileMutators are tools that change the workspace: repeating one is progress (a new change), not
// the stuck-repetition the loop detector hunts for. Covers the common families' edit/write tools.
var fileMutators = map[string]bool{
	"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true, "apply_patch": true,
	"fileChange": true, // codex app-server edit item
}

func mutatesFiles(tc *agent.ToolCall) bool {
	return tc != nil && fileMutators[tc.Name]
}
