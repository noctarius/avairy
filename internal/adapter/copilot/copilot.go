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

// New returns a Copilot adapter. Tool execution is gated via ACP's session/request_permission
// by decide: pass a human-routing decider to surface gated actions for approval, or nil to
// fail closed (§7 policy with no approver → destructive shell/file actions denied).
func New(decide gating.Decider) agent.Adapter {
	if decide == nil {
		decide = gating.Policy{}.Decide
	}
	a := acp.New(acp.Profile{
		Family:  agent.FamilyCopilot,
		Command: "copilot",
		Args:    args,
		Efforts: []string{"none", "low", "medium", "high", "xhigh", "max"},
	})
	a.Decide = decide
	return a
}

// args builds copilot's launch flags: --acp --stdio is the ACP transport; model and reasoning
// effort are global copilot flags (--model / --effort), appended when pinned.
func args(cfg agent.SessionConfig) []string {
	a := []string{"--acp", "--stdio"}
	if cfg.Model != "" {
		a = append(a, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		a = append(a, "--effort", cfg.Effort)
	}
	return a
}
