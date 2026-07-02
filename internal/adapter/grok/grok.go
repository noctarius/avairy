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
		Args:    args,
	})
	a.Decide = decide
	return a
}

// args builds grok's launch args. model and reasoning effort are options of the `agent` subcommand
// (`grok agent [OPTIONS] [COMMAND]`), so they must sit between `agent` and the `stdio` command —
// `stdio` itself doesn't accept them.
func args(cfg agent.SessionConfig) []string {
	a := []string{"agent"}
	if cfg.Model != "" {
		a = append(a, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		a = append(a, "--reasoning-effort", cfg.Effort)
	}
	return append(a, "stdio")
}
