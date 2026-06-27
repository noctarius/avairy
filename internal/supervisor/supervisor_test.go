package supervisor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"avairy/internal/adapter/mock"
	"avairy/internal/agent"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

func spawnerFor(ad *mock.Adapter, n *int32) Spawn {
	return func(ctx context.Context) (agent.Session, error) {
		atomic.AddInt32(n, 1)
		return ad.Start(ctx, agent.SessionConfig{AgentID: "alice"})
	}
}

// With idle == 0 the supervisor never sleeps and behaves like a runner: a human message is
// delivered to the session and the echoed events land in the journal.
func TestSupervisor_DeliversAndRecords(t *testing.T) {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	var spawns int32

	s := New("alice", []string{"backend"}, spawnerFor(mock.New(), &spawns), b, jrnl, 0)
	go s.Run(t.Context())

	// Give Run a moment to spawn + subscribe, then publish.
	if !waitFor(t, func() bool { return atomic.LoadInt32(&spawns) == 1 }) {
		t.Fatal("supervisor did not spawn at startup")
	}
	b.Publish("human", bus.Agent("alice"), "hello alice", agent.DeliverySteer)

	if !waitFor(t, func() bool {
		return hasText(jrnl, "hello alice") && countKind(jrnl, journal.KindAgentEvent) >= 2
	}) {
		t.Fatalf("expected echoed text + turn_done; got %+v", jrnl.Records())
	}
	if got := atomic.LoadInt32(&spawns); got != 1 {
		t.Fatalf("spawns = %d, want 1 (no respawn while awake)", got)
	}
}

// With a short idle the supervisor tears the session down (agent_sleeping) and lazily respawns it
// (agent_awake + a second spawn) when a directed message arrives.
func TestSupervisor_SleepsAndRespawns(t *testing.T) {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	var spawns int32

	s := New("alice", []string{"backend"}, spawnerFor(mock.New(), &spawns), b, jrnl, 100*time.Millisecond)
	go s.Run(t.Context())

	if !waitFor(t, func() bool { return atomic.LoadInt32(&spawns) == 1 }) {
		t.Fatal("no startup spawn")
	}
	// No activity → it should sleep.
	if !waitFor(t, func() bool { return hasSystem(jrnl, "agent_sleeping") }) {
		t.Fatalf("agent did not sleep; journal %+v", jrnl.Records())
	}

	// A directed message wakes it: respawn (#2), agent_awake, and the message is delivered.
	b.Publish("human", bus.Agent("alice"), "wake up", agent.DeliverySteer)
	if !waitFor(t, func() bool {
		return atomic.LoadInt32(&spawns) == 2 && hasSystem(jrnl, "agent_awake") && hasText(jrnl, "wake up")
	}) {
		t.Fatalf("agent did not respawn/deliver; spawns=%d journal %+v", atomic.LoadInt32(&spawns), jrnl.Records())
	}
}

// --- helpers ---

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
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

func hasSystem(j journal.Log, event string) bool {
	for _, r := range j.Records() {
		if r.Kind != journal.KindSystem {
			continue
		}
		if d, ok := r.Data.(map[string]any); ok && d["event"] == event {
			return true
		}
	}
	return false
}
