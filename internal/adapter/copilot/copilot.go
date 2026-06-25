// Package copilot drives GitHub Copilot CLI as an avairy agent via its Agent Client Protocol
// server (`copilot --acp --stdio`). It composes the generic ACP engine (internal/adapter/acp);
// the constructor matches the other families (claudecode.New, codex.New).
//
// Note: Copilot must be logged in (`copilot login`) for session creation to succeed.
package copilot

import (
	"avairy/internal/adapter/acp"
	"avairy/internal/agent"
	"avairy/internal/gating"
)

// New returns a Copilot adapter. Tool execution is gated by the §7 policy via ACP's
// session/request_permission (fail-closed: destructive shell/file actions are denied when no
// interactive approver is wired).
func New() agent.Adapter {
	a := acp.New(acp.Profile{
		Family:  agent.FamilyCopilot,
		Command: "copilot",
		Args:    []string{"--acp", "--stdio"},
	})
	a.Decide = gating.Policy{}.Decide
	return a
}
