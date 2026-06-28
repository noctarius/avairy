package agent

import "strings"

// ToolSummary renders a concise, human-readable description of a tool call — "Bash: go test
// ./...", "Read src/main.go" — so the operator sees what an agent is actually doing instead of
// "Bash" or "Read" repeated. It also gives loop detection a meaningful per-action signature:
// reading 100 different files is 100 distinct actions, not one loop.
func ToolSummary(tc *ToolCall) string {
	if tc == nil {
		return ""
	}
	if d := actionDetail(tc.Input); d != "" {
		return tc.Name + ": " + d
	}
	return tc.Name
}

// actionDetail picks the identifying argument of a tool call (the command, the file, the
// pattern) — what distinguishes one invocation from another.
func actionDetail(in map[string]any) string {
	for _, k := range []string{"command", "cmd", "file_path", "filePath", "path", "pattern", "query", "url"} {
		if v, ok := in[k].(string); ok && v != "" {
			return trunc(firstLineEllipsis(v), 120)
		}
	}
	return ""
}

// TrimInput returns a copy of a tool input safe to ship over the wire and store in the journal:
// large or noisy values (file bodies, diffs) are dropped and long strings truncated, keeping
// the identifiers (command, file_path, …) that matter for display and loop detection.
func TrimInput(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch k {
		case "content", "new_string", "old_string", "file_text", "patch", "edits":
			continue // bodies/diffs: too big, and uninteresting for display/signature
		}
		if s, ok := v.(string); ok {
			out[k] = trunc(s, 256)
		} else {
			out[k] = v
		}
	}
	return out
}

func firstLineEllipsis(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
