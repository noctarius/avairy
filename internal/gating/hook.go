package gating

import (
	"encoding/json"
	"net/http"
)

// hookInput is the Claude Code PreToolUse hook payload (subset; see ADAPTERS.md).
type hookInput struct {
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	SessionID string         `json:"session_id"`
}

// HookHandler is the avairy endpoint a Claude Code PreToolUse hook calls: it maps the tool
// call to a gating Request, runs decide, and returns the hook's permissionDecision JSON
// (allow|deny|ask). This is the Claude side of the EnforcementBackend (the Codex side routes
// app-server approvals; see codex.ApproverFromDecider).
func HookHandler(decide Decider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in hookInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad hook payload", http.StatusBadRequest)
			return
		}
		d, err := decide(r.Context(), hookToRequest(in))
		decision, reason := "deny", "blocked by avairy gating policy (DESIGN.md §7)"
		switch {
		case err != nil:
			reason = "gating error: " + err.Error()
		case d == Allow || d == AllowForSession:
			decision = "allow"
		}
		out := map[string]any{"hookEventName": "PreToolUse", "permissionDecision": decision}
		if decision == "deny" {
			out["permissionDecisionReason"] = reason
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"hookSpecificOutput": out})
	}
}

func hookToRequest(in hookInput) Request {
	switch in.ToolName {
	case "Bash":
		cmd, _ := in.ToolInput["command"].(string)
		return Request{Kind: ActionCommand, Summary: cmd, Details: in.ToolInput}
	case "Edit", "Write", "NotebookEdit":
		path, _ := in.ToolInput["file_path"].(string)
		return Request{Kind: ActionFileWrite, Summary: path, Details: in.ToolInput}
	default:
		return Request{Kind: ActionCommand, Summary: in.ToolName, Details: in.ToolInput}
	}
}
