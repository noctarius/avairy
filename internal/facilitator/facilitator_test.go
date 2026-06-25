package facilitator

import (
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
