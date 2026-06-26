package facilitator

import (
	"context"
	"strings"
	"testing"
	"time"

	"avairy/internal/agent"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

var roster = []Agent{
	{ID: "macbot", Caps: map[string]string{"os": "darwin"}},
	{ID: "linbot", Caps: map[string]string{"os": "linux"}},
}

func TestRuleNudger_CapabilityMatchmaking(t *testing.T) {
	got := RuleNudger{}.Decide(Trigger{Kind: TriggerBlocked, Agent: "macbot", Detail: "can't reproduce the panic on linux"}, roster)
	if len(got) != 1 || got[0].Kind != NudgeConsult || got[0].To != "macbot" {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got[0].Body, "linbot") {
		t.Fatalf("should point at linbot: %q", got[0].Body)
	}
}

func TestRuleNudger_BlockedNoCapablePeerSuggestsConsult(t *testing.T) {
	// blocker mentions linux but only a darwin peer exists → fall back to "ask a peer".
	got := RuleNudger{}.Decide(Trigger{Kind: TriggerBlocked, Agent: "linbot", Detail: "stuck on linux build"},
		[]Agent{{ID: "linbot", Caps: map[string]string{"os": "linux"}}, {ID: "macbot", Caps: map[string]string{"os": "darwin"}}})
	if len(got) != 1 || got[0].Kind != NudgeRemind || !strings.Contains(got[0].Body, "macbot") {
		t.Fatalf("got %+v", got)
	}
}

func TestRuleNudger_BlockedAloneEscalates(t *testing.T) {
	got := RuleNudger{}.Decide(Trigger{Kind: TriggerBlocked, Agent: "solo", Detail: "stuck"}, []Agent{{ID: "solo"}})
	if len(got) != 1 || got[0].Kind != NudgeEscalate {
		t.Fatalf("expected escalation, got %+v", got)
	}
}

func TestRuleNudger_Loop(t *testing.T) {
	got := RuleNudger{}.Decide(Trigger{Kind: TriggerLoop, Agent: "macbot"}, roster)
	if len(got) != 1 || got[0].To != "macbot" || !strings.Contains(got[0].Body, "ephemeral") {
		t.Fatalf("got %+v", got)
	}
}

// A blocked report observed from the journal results in a nudge published to the bus.
func TestObserve_BlockedPublishesNudge(t *testing.T) {
	j := journal.NewMemory()
	b := bus.New(j)
	f := New(b, RosterFunc(func() []Agent { return roster }), RuleNudger{})

	inbox, _ := b.Subscribe("macbot")
	f.Observe(journal.Record{Seq: 1, Kind: journal.KindSystem, Actor: "macbot",
		Data: map[string]any{"event": "report_status", "status": "blocked", "detail": "can't reproduce on linux"}})

	select {
	case m := <-inbox:
		if m.From != "facilitator" || !strings.Contains(m.Body, "linbot") {
			t.Fatalf("unexpected nudge: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("expected a facilitator nudge on the bus")
	}
}

// countNudger records how many times the facilitator decided to nudge.
type countNudger struct{ n *int }

func (c countNudger) Decide(t Trigger, _ []Agent) []Nudge {
	*c.n++
	return []Nudge{{Kind: NudgeRemind, To: t.Agent, Body: "x"}}
}

// A flapping agent that keeps reporting blocked is nudged once, not on every report — until the
// cooldown elapses (or it reports progress, which clears the cooldown).
func TestObserve_DebouncesRepeatedBlocked(t *testing.T) {
	var n int
	f := New(bus.New(journal.NewMemory()), RosterFunc(func() []Agent { return roster }), countNudger{&n})
	clock := time.Unix(1000, 0)
	f.now = func() time.Time {
		return clock
	}

	blocked := journal.Record{Kind: journal.KindSystem, Actor: "linbot",
		Data: map[string]any{"event": "report_status", "status": "blocked", "detail": "stuck"}}

	f.Observe(blocked)
	f.Observe(blocked)
	f.Observe(blocked)
	if n != 1 {
		t.Fatalf("repeated blocked should nudge once within cooldown, got %d", n)
	}

	// After the cooldown, a still-blocked agent is nudged again.
	clock = clock.Add(f.cooldown + time.Second)
	f.Observe(blocked)
	if n != 2 {
		t.Fatalf("expected a nudge after cooldown elapsed, got %d", n)
	}

	// Reporting progress clears the cooldown, so the next block nudges immediately.
	f.Observe(journal.Record{Kind: journal.KindSystem, Actor: "linbot",
		Data: map[string]any{"event": "report_status", "status": "working"}})
	f.Observe(blocked)
	if n != 3 {
		t.Fatalf("progress should clear the cooldown so a new block nudges, got %d", n)
	}
}

// Reading many *different* files is not a loop — only the same action repeated is. (Regression:
// remote agents used to lose tool args, making every Read look identical.)
func TestObserve_DifferentFilesAreNotALoop(t *testing.T) {
	var n int
	f := New(bus.New(journal.NewMemory()), RosterFunc(func() []Agent { return roster }), countNudger{&n})
	for _, path := range []string{"a.go", "b.go", "c.go", "d.go"} {
		f.Observe(journal.Record{Kind: journal.KindAgentEvent, Actor: "linbot",
			Data: agent.Event{Type: agent.EventToolUse, Tool: &agent.ToolCall{Name: "Read", Input: map[string]any{"file_path": path}}}})
	}
	if n != 0 {
		t.Fatalf("reading distinct files should not trip the loop detector, got %d nudges", n)
	}
}

// On a detected loop, the facilitator runs a fresh look and delivers its answer to the agent.
func TestObserve_LoopRunsFreshLook(t *testing.T) {
	j := journal.NewMemory()
	b := bus.New(j)
	f := New(b, RosterFunc(func() []Agent { return roster }), RuleNudger{})

	var gotQ string
	f.FreshLook = func(_ context.Context, q string) (string, error) {
		gotQ = q
		return "try a different command", nil
	}
	inbox, _ := b.Subscribe("linbot")

	rec := journal.Record{Kind: journal.KindAgentEvent, Actor: "linbot",
		Data: agent.Event{Type: agent.EventToolUse, Tool: &agent.ToolCall{Name: "Bash", Input: map[string]any{"command": "make"}}}}
	f.Observe(rec)
	f.Observe(rec)
	f.Observe(rec) // 3rd identical → loop

	deadline := time.After(2 * time.Second)
	for {
		select {
		case m := <-inbox:
			if strings.Contains(m.Body, "fresh look") && strings.Contains(m.Body, "try a different command") {
				if gotQ == "" {
					t.Fatal("FreshLook called with empty question")
				}
				return // success
			}
		case <-deadline:
			t.Fatal("expected a fresh-look message delivered to the looping agent")
		}
	}
}

// Loop detection fires only after loopN identical consecutive steps.
func TestObserve_LoopDetection(t *testing.T) {
	j := journal.NewMemory()
	b := bus.New(j)
	f := New(b, RosterFunc(func() []Agent { return roster }), RuleNudger{})
	inbox, _ := b.Subscribe("linbot")

	rec := journal.Record{Kind: journal.KindAgentEvent, Actor: "linbot",
		Data: agent.Event{Type: agent.EventToolUse, Tool: &agent.ToolCall{Name: "Bash", Input: map[string]any{"command": "make"}}}}
	f.Observe(rec) // 1
	f.Observe(rec) // 2 — no trigger yet
	select {
	case <-inbox:
		t.Fatal("loop fired too early")
	case <-time.After(50 * time.Millisecond):
	}
	f.Observe(rec) // 3 — loop!
	select {
	case m := <-inbox:
		if !strings.Contains(m.Body, "repeating") {
			t.Fatalf("unexpected: %q", m.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("expected a loop nudge")
	}
}
