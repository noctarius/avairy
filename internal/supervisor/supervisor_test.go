package supervisor

import (
	"context"
	"errors"
	"strings"
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

// A Stop (broadcast interrupt) against a family that can't be interrupted mid-turn (e.g. claude,
// whose Interrupt returns an error) must still actually stop it: the supervisor hard-stops by
// closing the subprocess (agent_sleeping), then respawns on the next directed message.
func TestSupervisor_InterruptHardStopsNonInterruptible(t *testing.T) {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	var spawns int32

	ad := mock.New()
	ad.InterruptErr = errors.New("mock: interrupt not supported") // mimic claude
	s := New("alice", []string{"backend"}, spawnerFor(ad, &spawns), b, jrnl, 0)
	go s.Run(t.Context())

	if !waitFor(t, func() bool { return atomic.LoadInt32(&spawns) == 1 }) {
		t.Fatal("supervisor did not spawn at startup")
	}

	b.Interrupt("human", bus.Broadcast()) // the Stop button
	if !waitFor(t, func() bool { return hasSystem(jrnl, "agent_sleeping") }) {
		t.Fatalf("Stop did not stop a non-interruptible agent; journal %+v", jrnl.Records())
	}

	// It's still usable: a directed message respawns it.
	b.Publish("human", bus.Agent("alice"), "back to work", agent.DeliverySteer)
	if !waitFor(t, func() bool { return atomic.LoadInt32(&spawns) == 2 && hasText(jrnl, "back to work") }) {
		t.Fatalf("agent did not respawn after a hard-stop; spawns=%d", atomic.LoadInt32(&spawns))
	}
}

// A Stop against an interruptible family (Interrupt returns nil) cancels the turn in-band without
// tearing the session down — it must NOT be hard-stopped.
func TestSupervisor_InterruptKeepsInterruptibleSession(t *testing.T) {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	var spawns int32

	s := New("alice", []string{"backend"}, spawnerFor(mock.New(), &spawns), b, jrnl, 0)
	go s.Run(t.Context())

	if !waitFor(t, func() bool { return atomic.LoadInt32(&spawns) == 1 }) {
		t.Fatal("no startup spawn")
	}
	b.Interrupt("human", bus.Broadcast())

	// Give the interrupt time to be (wrongly) treated as a hard-stop.
	time.Sleep(150 * time.Millisecond)
	if hasSystem(jrnl, "agent_sleeping") {
		t.Fatal("interruptible agent should not be torn down on interrupt")
	}
}

// A @team request that wakes an agent must carry the claim protocol in the delivered prompt. The
// agent is woken by Send (which otherwise passes only the bare body), so without an injected
// instruction it never learns this is a team request and just starts working — which is exactly how
// two agents ended up investigating the same leak in parallel. (The mock echoes the delivered text.)
func TestSupervisor_TeamWakeCarriesClaimInstruction(t *testing.T) {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	var spawns int32

	s := New("alice", []string{"backend"}, spawnerFor(mock.New(), &spawns), b, jrnl, 0)
	go s.Run(t.Context())

	if !waitFor(t, func() bool { return atomic.LoadInt32(&spawns) == 1 }) {
		t.Fatal("no startup spawn")
	}
	b.Publish(bus.SenderHuman, bus.Team(), "fix the leak", agent.DeliverySteer)

	if !waitFor(t, func() bool { return hasTextContaining(jrnl, "claim_response") }) {
		t.Fatalf("team wake did not carry the claim instruction; journal %+v", jrnl.Records())
	}
}

// --- helpers ---

func hasTextContaining(j journal.Log, sub string) bool {
	for _, r := range j.Records() {
		if r.Kind != journal.KindAgentEvent {
			continue
		}
		if ev, ok := r.Data.(agent.Event); ok && ev.Type == agent.EventText && strings.Contains(ev.Text, sub) {
			return true
		}
	}
	return false
}

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
