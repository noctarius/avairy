package bus

import (
	"testing"
	"time"

	"avairy/internal/agent"
	"avairy/internal/journal"
)

// The wake policy (#25): interrupts and human/facilitator always wake; agent broadcast/role is
// context-only; agent direct wakes within a per-agent budget, refilling after the window.
func TestWakerPolicyAndBudget(t *testing.T) {
	now := time.Now()
	w := NewWaker()
	if !w.Wake("alice", ToBroadcast, true, now) {
		t.Fatal("interrupt must always wake")
	}
	if !w.Wake(SenderHuman, ToBroadcast, false, now) || !w.Wake(SenderFacilitator, ToRole, false, now) {
		t.Fatal("human/facilitator broadcast+role must wake")
	}
	if w.Wake("alice", ToBroadcast, false, now) || w.Wake("alice", ToRole, false, now) {
		t.Fatal("agent broadcast/role must be context-only (no wake)")
	}

	w2 := NewWaker()
	for i := 0; i < 6; i++ {
		if !w2.Wake("alice", ToAgent, false, now) {
			t.Fatalf("autonomous direct wake %d should pass (within budget)", i)
		}
	}
	if w2.Wake("alice", ToAgent, false, now) {
		t.Fatal("over-budget autonomous direct should be context-only")
	}
	if !w2.Wake("alice", ToAgent, false, now.Add(31*time.Second)) {
		t.Fatal("budget should refill after the window")
	}
}

// Dedup drops an identical (from,to,body) repeated within the window; distinct bodies pass.
func TestPublishDedup(t *testing.T) {
	b := New(journal.NewMemory())
	ch, _ := b.Subscribe("bob")
	b.Publish("alice", Agent("bob"), "hi", agent.DeliverySteer)
	b.Publish("alice", Agent("bob"), "hi", agent.DeliverySteer) // duplicate within window → dropped
	b.Publish("alice", Agent("bob"), "different", agent.DeliverySteer)

	got := drain(ch)
	if len(got) != 2 || got[0].Body != "hi" || got[1].Body != "different" {
		t.Fatalf("dedup: bob received %d messages %v, want [hi, different]", len(got), bodies(got))
	}
}

func drain(ch <-chan Message) []Message {
	var out []Message
	for {
		select {
		case m := <-ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

// The facilitator dispatch loop must receive ONLY @facilitator requests — never @team/@broadcast.
// Otherwise it re-dispatches a direct @team request as a duplicate team message (a second wake under
// a different id), so two agents end up working the same request in parallel.
func TestFacilitatorReceivesOnlyFacilitatorMessages(t *testing.T) {
	b := New(journal.NewMemory())
	fac, _ := b.Subscribe(SenderFacilitator)
	agt, _ := b.Subscribe("alice", "backend")

	// A human @team request: agents see it (to claim among themselves); the facilitator must not.
	b.Publish(SenderHuman, Team(), "fix the leak", agent.DeliverySteer)
	if got := drain(agt); len(got) != 1 {
		t.Fatalf("agent should receive the @team request, got %d", len(got))
	}
	if got := drain(fac); len(got) != 0 {
		t.Fatalf("facilitator must NOT receive a @team request (it would re-dispatch it), got %v", bodies(got))
	}

	// A human @broadcast: everyone (including the agent), but not the facilitator's triage loop.
	b.Publish(SenderHuman, Broadcast(), "status?", agent.DeliverySteer)
	if got := drain(agt); len(got) != 1 {
		t.Fatalf("agent should receive the @broadcast, got %v", bodies(got))
	}
	if got := drain(fac); len(got) != 0 {
		t.Fatalf("facilitator must NOT receive a @broadcast, got %v", bodies(got))
	}

	// But an explicit @facilitator request reaches only the facilitator.
	b.Publish(SenderHuman, Facilitator(), "triage this", agent.DeliverySteer)
	if got := drain(fac); len(got) != 1 {
		t.Fatalf("facilitator should receive a @facilitator request, got %d", len(got))
	}
	if got := drain(agt); len(got) != 0 {
		t.Fatalf("agents must not receive @facilitator messages, got %v", bodies(got))
	}
}

func bodies(ms []Message) []string {
	var b []string
	for _, m := range ms {
		b = append(b, m.Body)
	}
	return b
}
