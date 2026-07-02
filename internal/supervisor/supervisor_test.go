package supervisor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"avairy/internal/adapter/mock"
	"avairy/internal/agent"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

func spawnerFor(ad *mock.Adapter, n *int32) Spawn {
	return func(ctx context.Context, model, effort string) (agent.Session, error) {
		atomic.AddInt32(n, 1)
		return ad.Start(ctx, agent.SessionConfig{AgentID: "alice", Model: model, Effort: effort})
	}
}

// The supervisor emits a turn_start event the moment it dispatches a message, so the consoles show
// "working" during the think gap before the first token — not only once the first event lands.
func TestSupervisor_EmitsTurnStartOnDispatch(t *testing.T) {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	var spawns int32

	s := New("alice", []string{"backend"}, spawnerFor(mock.New(), &spawns), b, jrnl, 0, "", "", agent.Capabilities{})
	go s.Run(t.Context())
	if !waitFor(t, func() bool { return atomic.LoadInt32(&spawns) == 1 }) {
		t.Fatal("supervisor did not spawn at startup")
	}
	b.Publish("human", bus.Agent("alice"), "hello alice", agent.DeliverySteer)

	if !waitFor(t, func() bool { return hasEvent(jrnl, "alice", agent.EventTurnStart) }) {
		t.Fatalf("expected a turn_start agent event on dispatch; got %+v", jrnl.Records())
	}
}

func hasEvent(j journal.Log, id string, typ agent.EventType) bool {
	for _, r := range j.Records() {
		if r.Kind != journal.KindAgentEvent || r.Actor != id {
			continue
		}
		if ev, ok := r.Data.(agent.Event); ok && ev.Type == typ {
			return true
		}
	}
	return false
}

// With idle == 0 the supervisor never sleeps and behaves like a runner: a human message is
// delivered to the session and the echoed events land in the journal.
func TestSupervisor_DeliversAndRecords(t *testing.T) {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	var spawns int32

	s := New("alice", []string{"backend"}, spawnerFor(mock.New(), &spawns), b, jrnl, 0, "", "", agent.Capabilities{})
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

	s := New("alice", []string{"backend"}, spawnerFor(mock.New(), &spawns), b, jrnl, 100*time.Millisecond, "", "", agent.Capabilities{})
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
	s := New("alice", []string{"backend"}, spawnerFor(ad, &spawns), b, jrnl, 0, "", "", agent.Capabilities{})
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

	s := New("alice", []string{"backend"}, spawnerFor(mock.New(), &spawns), b, jrnl, 0, "", "", agent.Capabilities{})
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

	s := New("alice", []string{"backend"}, spawnerFor(mock.New(), &spawns), b, jrnl, 0, "", "", agent.Capabilities{})
	go s.Run(t.Context())

	if !waitFor(t, func() bool { return atomic.LoadInt32(&spawns) == 1 }) {
		t.Fatal("no startup spawn")
	}
	b.Publish(bus.SenderHuman, bus.Team(), "fix the leak", agent.DeliverySteer)

	if !waitFor(t, func() bool { return hasTextContaining(jrnl, "claim_response") }) {
		t.Fatalf("team wake did not carry the claim instruction; journal %+v", jrnl.Records())
	}
}

// A context-only (NoWake) message — e.g. a 👍/👎 reaction — is delivered to the agent's inbox but
// must NOT trigger a turn; a normal directed message still does. (The mock echoes delivered text, so
// an echo == a turn happened.)
func TestSupervisor_NoWakeDoesNotTriggerTurn(t *testing.T) {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	var spawns int32

	s := New("alice", []string{"backend"}, spawnerFor(mock.New(), &spawns), b, jrnl, 0, "", "", agent.Capabilities{})
	go s.Run(t.Context())
	if !waitFor(t, func() bool { return atomic.LoadInt32(&spawns) == 1 }) {
		t.Fatal("no startup spawn")
	}

	b.PublishContext("human", bus.Agent("alice"), "👍 nice work")
	time.Sleep(150 * time.Millisecond)
	if hasText(jrnl, "👍 nice work") {
		t.Fatal("a context-only message must not trigger a turn")
	}

	b.Publish("human", bus.Agent("alice"), "do this", agent.DeliverySteer)
	if !waitFor(t, func() bool { return hasText(jrnl, "do this") }) {
		t.Fatal("a normal directed message should trigger a turn")
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

// --- reconfigure ---

// A fake session for reconfigure tests. fakeSession implements only agent.Session (so the driver
// must respawn to reconfigure it); liveFakeSession also implements agent.Reconfigurer (applied live).
type fakeSession struct {
	events chan agent.Event
	closed bool
}

func (f *fakeSession) ID() string                                         { return "alice" }
func (f *fakeSession) Send(context.Context, string, agent.Delivery) error { return nil }
func (f *fakeSession) Events() <-chan agent.Event                         { return f.events }
func (f *fakeSession) Interrupt(context.Context) error                    { return nil }
func (f *fakeSession) Close() error {
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	return nil
}

type liveFakeSession struct {
	*fakeSession
	mu            sync.Mutex
	model, effort string
	calls         int
}

func (l *liveFakeSession) Reconfigure(_ context.Context, model, effort string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if model != "" {
		l.model = model
	}
	if effort != "" {
		l.effort = effort
	}
	return nil
}

// A live-reconfigurable family applies the change in place — no respawn.
func TestSupervisor_ReconfigureLive(t *testing.T) {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	var spawns int32
	live := &liveFakeSession{fakeSession: &fakeSession{events: make(chan agent.Event, 4)}}
	spawn := func(context.Context, string, string) (agent.Session, error) {
		atomic.AddInt32(&spawns, 1)
		return live, nil
	}
	s := New("alice", []string{"backend"}, spawn, b, jrnl, 0, "m1", "e1", agent.Capabilities{})
	go s.Run(t.Context())
	if !waitFor(t, func() bool { return atomic.LoadInt32(&spawns) == 1 }) {
		t.Fatal("no startup spawn")
	}

	s.Reconfigure("m2", "e2")
	if !waitFor(t, func() bool {
		live.mu.Lock()
		defer live.mu.Unlock()
		return live.model == "m2" && live.effort == "e2"
	}) {
		t.Fatal("live reconfigure was not applied to the session")
	}
	if got := atomic.LoadInt32(&spawns); got != 1 {
		t.Fatalf("live reconfigure must not respawn, spawns=%d", got)
	}
	if !waitFor(t, func() bool { return hasSystemField(jrnl, "reconfigured", "applied", "live") }) {
		t.Fatalf("expected a live reconfigured event; journal %+v", jrnl.Records())
	}
}

// A family with no live path respawns — and the fresh session gets the new model.
func TestSupervisor_ReconfigureRespawnsWhenIdle(t *testing.T) {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	var spawns int32
	var mu sync.Mutex
	var lastModel string
	spawn := func(_ context.Context, model, _ string) (agent.Session, error) {
		atomic.AddInt32(&spawns, 1)
		mu.Lock()
		lastModel = model
		mu.Unlock()
		return &fakeSession{events: make(chan agent.Event, 4)}, nil
	}
	s := New("alice", []string{"backend"}, spawn, b, jrnl, 0, "m1", "", agent.Capabilities{})
	go s.Run(t.Context())
	if !waitFor(t, func() bool { return atomic.LoadInt32(&spawns) == 1 }) {
		t.Fatal("no startup spawn")
	}

	s.Reconfigure("m2", "") // not live-reconfigurable, agent idle → respawn with m2
	if !waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return atomic.LoadInt32(&spawns) == 2 && lastModel == "m2"
	}) {
		t.Fatalf("expected a respawn with model m2; spawns=%d journal %+v", atomic.LoadInt32(&spawns), jrnl.Records())
	}
}

func hasSystemField(j journal.Log, event, key, val string) bool {
	for _, r := range j.Records() {
		if r.Kind != journal.KindSystem {
			continue
		}
		if d, ok := r.Data.(map[string]any); ok && d["event"] == event && d[key] == val {
			return true
		}
	}
	return false
}
