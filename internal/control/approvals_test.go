package control

import (
	"context"
	"testing"
	"time"
)

func TestApprovalsResolveAllow(t *testing.T) {
	a := NewApprovals(2 * time.Second)
	done := make(chan string, 1)
	go func() {
		done <- a.Ask(context.Background(), Approval{AgentID: "alice", Kind: "command", Summary: "git push"})
	}()

	// Wait for the request to register, then resolve it.
	var id string
	for i := 0; i < 100 && id == ""; i++ {
		if p := a.Pending(); len(p) == 1 {
			id = p[0].ID
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if id == "" {
		t.Fatal("request never became pending")
	}
	if !a.Resolve(id, DecisionAllow) {
		t.Fatal("Resolve reported not-pending")
	}
	if got := <-done; got != DecisionAllow {
		t.Fatalf("Ask returned %q, want allow", got)
	}
	if len(a.Pending()) != 0 {
		t.Fatal("resolved request should be cleared")
	}
}

func TestApprovalsTimeoutDenies(t *testing.T) {
	a := NewApprovals(40 * time.Millisecond)
	if got := a.Ask(context.Background(), Approval{Summary: "rm -rf /"}); got != DecisionDeny {
		t.Fatalf("unanswered request should deny, got %q", got)
	}
}

func TestApprovalsContextCancelDenies(t *testing.T) {
	a := NewApprovals(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := a.Ask(ctx, Approval{Summary: "sudo reboot"}); got != DecisionDeny {
		t.Fatalf("cancelled request should deny, got %q", got)
	}
}

func TestApprovalsLifecycleHooks(t *testing.T) {
	a := NewApprovals(30 * time.Millisecond)
	var reqd, resolved string
	a.OnRequest = func(ap Approval) { reqd = ap.ID }
	a.OnResolve = func(ap Approval, d string) { resolved = d }
	a.Ask(context.Background(), Approval{Summary: "npm install x"}) // times out → deny
	if reqd == "" {
		t.Fatal("OnRequest not fired")
	}
	if resolved != DecisionDeny {
		t.Fatalf("OnResolve decision = %q, want deny", resolved)
	}
}
