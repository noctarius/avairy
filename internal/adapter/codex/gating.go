package codex

import (
	"context"
	"encoding/json"
	"strings"

	"avairy/internal/agent"
	"avairy/internal/gating"
)

// ApproverFromDecider turns a gating.Decider into a Codex ApprovalDecider, so app-server
// approval requests (command execution, file changes) are gated by avairy policy instead of
// auto-approved. This is the Codex side of the EnforcementBackend (the Claude side is a
// PreToolUse hook → gating.HookHandler).
func ApproverFromDecider(decide gating.Decider) ApprovalDecider {
	return func(method string, params json.RawMessage) string {
		req := approvalToRequest(method, params)
		d, err := decide(context.Background(), req)
		allow := err == nil && (d == gating.Allow || d == gating.AllowForSession)
		return codexDecision(method, allow, d == gating.AllowForSession)
	}
}

func approvalToRequest(method string, params json.RawMessage) gating.Request {
	var p struct {
		Command []string `json:"command"`
		Reason  string   `json:"reason"`
	}
	_ = json.Unmarshal(params, &p)
	switch {
	case strings.Contains(method, "fileChange"), method == "applyPatchApproval":
		// Best-effort: surface whatever patch/changes the app-server included so the operator can
		// review the edit (PatchPreview tolerates the various shapes; "" → no diff to show).
		var raw map[string]any
		_ = json.Unmarshal(params, &raw)
		return gating.Request{Kind: gating.ActionFileWrite, Summary: "file change", Reason: p.Reason, Diff: agent.PatchPreview("apply_patch", raw)}
	default:
		return gating.Request{Kind: gating.ActionCommand, Summary: strings.Join(p.Command, " "), Reason: p.Reason}
	}
}

// codexDecision maps a gating outcome to the right decision string for the method's protocol
// version (v1: approved/denied; v2: accept/decline).
func codexDecision(method string, allow, forSession bool) string {
	v1 := method == "execCommandApproval" || method == "applyPatchApproval"
	switch {
	case !allow && v1:
		return "denied"
	case !allow:
		return "decline"
	case forSession && v1:
		return "approved_for_session"
	case forSession:
		return "acceptForSession"
	case v1:
		return "approved"
	default:
		return "accept"
	}
}
