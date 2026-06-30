// Package gating decouples gating *policy* (what is gated — DESIGN.md §7) from the
// *mechanism* that enforces it (how). Backends are pluggable so stronger enforcement can be
// added without touching policy or callers:
//
//   - hooked (v1): Claude Code PreToolUse hook calls back here; Codex app-server
//     item/*/requestApproval JSON-RPC requests are answered here.
//   - sandboxed (future): OS-layer confinement + brokers, dropped in as another Backend.
//   - advisory: allow + log + stream only, where no real interception exists.
package gating

import (
	"context"

	"avairy/internal/agent"
)

// ActionKind classifies a gated action (DESIGN.md §7).
type ActionKind string

const (
	ActionCommand   ActionKind = "command"    // shell/command execution
	ActionFileWrite ActionKind = "file_write" // edit outside the free set
	ActionRead      ActionKind = "read"       // read-only inspection (never gated)
	ActionGitMutate ActionKind = "git_mutate" // commit/tag/push — core-only & signed (§9)
	ActionCrossNode ActionKind = "cross_node" // action affecting another node
	ActionInstall   ActionKind = "install"    // package install / sudo
)

// Request is a normalized, family-agnostic approval request routed to the coordinator.
type Request struct {
	AgentID string
	Kind    ActionKind
	Summary string         // human-readable, e.g. the command line
	Details map[string]any // raw family-specific action fields
	Reason  string         // agent-provided rationale, if any
	Diff    string         // for a file edit: a unified diff to show the operator (empty otherwise)
}

// Decision is the coordinator's ruling on a gated action.
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
	// AllowForSession allows this and similar actions for the rest of the session.
	AllowForSession Decision = "allow_for_session"
)

// Decider renders a verdict for a gated action. It is implemented by the coordinator,
// which may consult policy, the facilitator, or the human (TUI approvals view, DESIGN.md §7).
//
// It MUST always return: an unanswered Codex approval request hangs the turn forever
// (see ADAPTERS.md). Backends are expected to apply a timeout/default if the Decider stalls.
type Decider func(ctx context.Context, req Request) (Decision, error)

// Backend wires a family's native interception to the Decider.
type Backend interface {
	Level() agent.EnforcementLevel
	// Attach begins intercepting gated actions for sess, routing each to decide. It returns
	// once interception is wired and stops when sess closes or ctx is cancelled.
	Attach(ctx context.Context, sess agent.Session, decide Decider) error
}
