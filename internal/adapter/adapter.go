// Package adapter holds cross-family helpers for building agent adapters. The per-family adapters
// live in subpackages (claudecode, codex, copilot, grok, acp); this is the small shared seam that
// avoids re-deriving the family→constructor mapping at every call site.
package adapter

import (
	"avairy/internal/adapter/codex"
	"avairy/internal/adapter/copilot"
	"avairy/internal/adapter/grok"
	"avairy/internal/agent"
	"avairy/internal/gating"
)

// NewGated builds the adapter for an agent family whose gating is a plain gating.Decider — codex
// (app-server approvals), copilot and grok (ACP). It returns ok=false for "claude" and any unknown
// family, because claude's gating is wired environment-specifically by the caller (a local /gate
// server, a gate URL, or no tools at all) and the caller also owns its default-family policy.
func NewGated(family string, dec gating.Decider) (agent.Adapter, bool) {
	switch family {
	case "codex":
		cx := codex.New()
		cx.Approve = codex.ApproverFromDecider(dec)
		return cx, true
	case "copilot":
		return copilot.New(dec), true // ACP; needs `copilot login`
	case "grok":
		return grok.New(dec), true // ACP; needs xAI auth
	}
	return nil, false
}
