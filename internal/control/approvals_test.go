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

func TestApprovalsAllowForSession(t *testing.T) {
	a := NewApprovals(2 * time.Second)
	ask := func() string {
		done := make(chan string, 1)
		go func() { done <- a.Ask(context.Background(), Approval{AgentID: "alice", Kind: "command", Summary: "git push"}) }()
		return waitResolve(t, a, done, DecisionAllowForSession)
	}
	// First ask is resolved allow-for-session by the operator.
	if got := ask(); got != DecisionAllowForSession {
		t.Fatalf("first ask: got %q", got)
	}
	// A second identical-kind action from the same agent is auto-allowed with no pending entry.
	got := a.Ask(context.Background(), Approval{AgentID: "alice", Kind: "command", Summary: "git tag v2"})
	if got != DecisionAllow {
		t.Fatalf("second ask should auto-allow, got %q", got)
	}
	// A different kind, and a different agent, still prompt.
	if !wouldBlock(a, Approval{AgentID: "alice", Kind: "install", Summary: "npm i x"}) {
		t.Fatal("different kind should still prompt")
	}
	if !wouldBlock(a, Approval{AgentID: "bob", Kind: "command", Summary: "git push"}) {
		t.Fatal("different agent should still prompt")
	}
}

// waitResolve waits for the request to register, resolves it with decision, and returns Ask's result.
func waitResolve(t *testing.T, a *Approvals, done <-chan string, decision string) string {
	t.Helper()
	for i := 0; i < 100; i++ {
		if p := a.Pending(); len(p) == 1 {
			a.Resolve(p[0].ID, decision)
			return <-done
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("request never became pending")
	return ""
}

// wouldBlock reports whether Ask for req would register a pending request (i.e. isn't auto-allowed).
func wouldBlock(a *Approvals, req Approval) bool {
	go a.Ask(context.Background(), req)
	for i := 0; i < 100; i++ {
		for _, p := range a.Pending() {
			if p.AgentID == req.AgentID && p.Kind == req.Kind {
				a.Resolve(p.ID, DecisionDeny) // clean up the goroutine
				return true
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
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
