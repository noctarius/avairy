package runner

import (
	"context"
	"testing"
	"time"

	"avairy/internal/adapter/mock"
	"avairy/internal/agent"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

// End-to-end loop, zero credits: a human message published to the bus is delivered to a
// mock agent, whose echoed events land in the journal.
func TestRunner_DeliversAndRecords(t *testing.T) {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)

	ad := mock.New()
	sess, err := ad.Start(context.Background(), agent.SessionConfig{AgentID: "alice"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// New subscribes synchronously, so publishing before Run starts is safe (buffered inbox).
	r := New(Agent{ID: "alice", Roles: []string{"backend"}}, sess, b, jrnl)
	go r.Run(t.Context())
	b.Publish("human", bus.Agent("alice"), "hello alice", agent.DeliverySteer)

	// Expect: a text event echoing the body and a turn_done.
	if !waitFor(t, func() bool {
		return hasText(jrnl, "hello alice") && countKind(jrnl, journal.KindAgentEvent) >= 2
	}) {
		t.Fatalf("expected echoed text + turn_done in journal; got %+v", jrnl.Records())
	}
}

func TestBus_RoleFanoutAndNoEcho(t *testing.T) {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)

	aCh, _ := b.Subscribe("alice", "backend")
	bCh, _ := b.Subscribe("bob", "backend")

	b.Publish("alice", bus.Role("backend"), "standup", agent.DeliverySteer)

	// bob (same role) receives it; alice (sender) does not.
	select {
	case m := <-bCh:
		if m.Body != "standup" {
			t.Fatalf("bob got %q", m.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("bob did not receive role message")
	}
	select {
	case m := <-aCh:
		t.Fatalf("alice should not receive her own message, got %q", m.Body)
	case <-time.After(100 * time.Millisecond):
	}
}

// --- helpers ---

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func countKind(j journal.Log, k journal.Kind) int {
	n := 0
	for _, r := range j.Records() {
		if r.Kind == k {
			n++
		}
	}
	return n
}

func hasText(j journal.Log, text string) bool {
	for _, r := range j.Records() {
		if r.Kind != journal.KindAgentEvent {
			continue
		}
		if ev, ok := r.Data.(agent.Event); ok && ev.Type == agent.EventText && ev.Text == text {
			return true
		}
	}
	return false
}
