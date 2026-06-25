// Package grok drives the xAI Grok CLI as an avairy agent via its Agent Client Protocol
// server (`grok agent stdio`). Like copilot, it's just a Profile over the generic ACP engine
// (internal/adapter/acp); the constructor matches the other families.
//
// Note: Grok must be authenticated (xAI login) for session creation to succeed.
package grok

import (
	"avairy/internal/adapter/acp"
	"avairy/internal/agent"
	"avairy/internal/gating"
)

// New returns a Grok adapter. Tool execution is gated by the §7 policy via ACP's
// session/request_permission (fail-closed when no interactive approver is wired).
func New() agent.Adapter {
	a := acp.New(acp.Profile{
		Family:  agent.FamilyGrok,
		Command: "grok",
		Args:    []string{"agent", "stdio"},
	})
	a.Decide = gating.Policy{}.Decide
	return a
}
