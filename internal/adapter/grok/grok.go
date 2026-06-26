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

// New returns a Grok adapter. Tool execution is gated via ACP's session/request_permission by
// decide: pass a human-routing decider to surface gated actions for approval, or nil to fail
// closed (§7 policy with no approver → destructive shell/file actions denied).
func New(decide gating.Decider) agent.Adapter {
	if decide == nil {
		decide = gating.Policy{}.Decide
	}
	a := acp.New(acp.Profile{
		Family:  agent.FamilyGrok,
		Command: "grok",
		Args:    []string{"agent", "stdio"},
	})
	a.Decide = decide
	return a
}
