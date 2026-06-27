package operator_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/control"
	"avairy/internal/journal"
	"avairy/internal/operator"
)

// End-to-end: an in-process Services served over the operator API, attached by a Client, must
// replay journal history, stream new records, reflect state (tasks/approvals/conflicts/roster), and
// route an operator action (inject) back to the live bus.
func TestOperatorServerClientRoundTrip(t *testing.T) {
	j := journal.NewMemory()
	b := bus.New(j)
	bd := board.New(j)
	approvals := control.NewApprovals(time.Minute)
	conflicts := control.NewConflicts()

	// Pre-existing history (must arrive as backfill) + state the client should see.
	b.Publish("human", bus.Broadcast(), "hello before connect", agent.DeliverySteer)
	bd.Post("human", "repro the panic", map[string]string{"os": "linux"}, nil)
	approvals.OnRequest = func(control.Approval) {}
	go approvals.Ask(context.Background(), control.Approval{AgentID: "linbot", Kind: "command", Summary: "git push"})
	conflicts.Raise(control.OperatorConflict{Path: "f.go", HubVersion: 3, Source: "seed"})
	time.Sleep(20 * time.Millisecond) // let the async Ask register

	svc := &operator.Services{
		Journal: j, Bus: b, Approvals: approvals, Conflicts: conflicts,
		Tasks:  bd.List,
		Roster: func() []string { return []string{"alice", "linbot"} },
	}
	ts := httptest.NewServer(operator.NewServer(svc, "sekret", true).Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := operator.Connect(ctx, ts.URL, "sekret", ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	deps := client.Deps()

	// Backfill replayed: the pre-connect message is in the client's journal.
	if !hasMessage(deps.Journal.Records(), "hello before connect") {
		t.Fatal("backfill did not replay the pre-connect message")
	}
	// State snapshot reflects tasks/approvals/conflicts/roster.
	if got := deps.Tasks(); len(got) != 1 || got[0].Title != "repro the panic" {
		t.Fatalf("tasks = %+v", got)
	}
	if got := deps.PendingApprovals(); len(got) != 1 || got[0].AgentID != "linbot" {
		t.Fatalf("approvals = %+v", got)
	}
	if got := deps.PendingConflicts(); len(got) != 1 || got[0].Path != "f.go" {
		t.Fatalf("conflicts = %+v", got)
	}
	if got := deps.Roster(); len(got) != 2 {
		t.Fatalf("roster = %+v", got)
	}

	// A live record streams through after connect.
	b.Publish("alice", bus.Broadcast(), "live update", agent.DeliverySteer)
	if !eventually(t, func() bool { return hasMessage(deps.Journal.Records(), "live update") }) {
		t.Fatal("live record did not stream to the client")
	}

	// An operator action (inject) routes back to the live bus.
	deps.Inject("alice", "do the thing")
	if !eventually(t, func() bool { return hasMessage(j.Records(), "do the thing") }) {
		t.Fatal("inject did not reach the core bus")
	}

	// Resolving an approval through the client unblocks the waiting Ask (it's gone from pending).
	deps.ResolveApproval(deps.PendingApprovals()[0].ID, control.DecisionAllow)
	if !eventually(t, func() bool { return len(approvals.Pending()) == 0 }) {
		t.Fatal("approval was not resolved via the operator API")
	}
}

// Unauthorized requests are rejected when a token is set.
func TestOperatorAuth(t *testing.T) {
	j := journal.NewMemory()
	svc := &operator.Services{Journal: j, Bus: bus.New(j), Approvals: control.NewApprovals(0), Conflicts: control.NewConflicts()}
	ts := httptest.NewServer(operator.NewServer(svc, "sekret", true).Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := operator.Connect(ctx, ts.URL, "wrong-token", ts.Client()); err == nil {
		t.Fatal("expected connect with a bad token to fail")
	}
}

func hasMessage(recs []journal.Record, body string) bool {
	for _, r := range recs {
		if m, ok := r.Data.(bus.Message); ok && m.Body == body {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, cond func() bool) bool {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
