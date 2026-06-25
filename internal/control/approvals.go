package control

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Decision strings delivered to a waiting Ask. Kept as plain strings so this package stays
// independent of internal/gating (which maps them to gating.Decision at the edges).
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
)

// Approval is a pending request for the operator to allow or deny a gated action (DESIGN.md
// §7). It surfaces in the TUI approvals view; the operator's verdict is delivered back to the
// agent (local or via a node) that is blocked waiting on it.
type Approval struct {
	ID      string    `json:"id"`
	AgentID string    `json:"agentId"`
	Kind    string    `json:"kind"`
	Summary string    `json:"summary"`
	Reason  string    `json:"reason,omitempty"`
	At      time.Time `json:"-"`
}

// Approvals brokers human-in-the-loop approvals: Ask registers a request and blocks; the
// operator sees it (Pending) and rules on it (Resolve); the verdict is delivered to Ask.
// Unanswered requests fail CLOSED (deny) on timeout or caller cancellation — a gate the human
// never answers must not silently allow.
type Approvals struct {
	timeout time.Duration

	// OnRequest/OnResolve, if set, observe the lifecycle (used to journal so the TUI refreshes
	// and the decision is audited). OnResolve fires once with the final verdict, however reached.
	OnRequest func(Approval)
	OnResolve func(Approval, string)

	mu      sync.Mutex
	seq     int
	pending map[string]*waiter
}

type waiter struct {
	Approval
	ch chan string
}

// NewApprovals returns a broker; unanswered requests deny after timeout (default 5m).
func NewApprovals(timeout time.Duration) *Approvals {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &Approvals{timeout: timeout, pending: make(map[string]*waiter)}
}

// Ask registers req as pending and blocks until the operator resolves it, ctx is cancelled,
// or the timeout elapses. Returns DecisionAllow or DecisionDeny (deny on cancel/timeout).
func (a *Approvals) Ask(ctx context.Context, req Approval) string {
	a.mu.Lock()
	a.seq++
	req.ID = fmt.Sprintf("ap%d", a.seq)
	req.At = time.Now()
	w := &waiter{Approval: req, ch: make(chan string, 1)}
	a.pending[req.ID] = w
	a.mu.Unlock()

	if a.OnRequest != nil {
		a.OnRequest(req)
	}

	timer := time.NewTimer(a.timeout)
	defer timer.Stop()

	decision := DecisionDeny
	select {
	case d := <-w.ch:
		decision = d
	case <-ctx.Done():
	case <-timer.C:
	}
	a.remove(req.ID)
	if a.OnResolve != nil {
		a.OnResolve(req, decision)
	}
	return decision
}

// Pending returns a snapshot of unresolved approvals, oldest first (for the TUI).
func (a *Approvals) Pending() []Approval {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Approval, 0, len(a.pending))
	for _, w := range a.pending {
		out = append(out, w.Approval)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// Resolve delivers the operator's decision to the waiting Ask and clears the request. It
// reports whether id was still pending (false if it already timed out or was resolved).
func (a *Approvals) Resolve(id, decision string) bool {
	a.mu.Lock()
	w, ok := a.pending[id]
	if ok {
		delete(a.pending, id)
	}
	a.mu.Unlock()
	if !ok {
		return false
	}
	w.ch <- decision
	return true
}

func (a *Approvals) remove(id string) {
	a.mu.Lock()
	delete(a.pending, id)
	a.mu.Unlock()
}
