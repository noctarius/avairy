package agent

import (
	"fmt"
	"strings"

	udiff "github.com/aymanbagabas/go-udiff"
)

// maxPatchLines caps a diff preview so a huge edit can't bloat the approval payload or the UI.
const maxPatchLines = 400

// PatchPreview renders a human-readable diff of a file-editing tool call, for the approval card's
// "show diff". It handles the families' different shapes: an explicit unified patch (codex
// apply_patch), an old→new string replacement (Claude Edit, ACP edit), a list of edits (Claude
// MultiEdit), or a full new body (Write/NotebookEdit). Returns "" when there's nothing diff-like to
// show (e.g. a shell command), so callers can decide whether to offer the control. Output is capped.
func PatchPreview(toolName string, input map[string]any) string {
	if input == nil {
		return ""
	}
	file, _ := firstString(input, "file_path", "filePath", "path")

	// 1. An explicit unified patch/diff (codex apply_patch, or any tool that hands us one).
	if p, ok := firstString(input, "patch", "diff", "unified_diff", "unifiedDiff"); ok {
		return capLines(p)
	}
	// 2. An old→new string replacement (Claude Edit, ACP edit).
	if old, ok := input["old_string"].(string); ok {
		if nw, ok := input["new_string"].(string); ok {
			return capLines(diffOf(file, old, nw))
		}
	}
	// 3. A list of edits (Claude MultiEdit): [{old_string, new_string}, …].
	if edits, ok := input["edits"].([]any); ok {
		var b strings.Builder
		for _, e := range edits {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			old, _ := m["old_string"].(string)
			nw, _ := m["new_string"].(string)
			b.WriteString(diffOf(file, old, nw))
			b.WriteByte('\n')
		}
		if b.Len() > 0 {
			return capLines(b.String())
		}
	}
	// 4. A full new body (Write / NotebookEdit / create): show it as an all-added diff.
	if body, ok := firstString(input, "content", "file_text", "new_str"); ok {
		return capLines(diffOf(file, "", body))
	}
	return ""
}

// ToolDiff returns the diff to show for a tool call: the capped "_diff" TrimInput left on a shipped
// event (node agents), else one rendered from the raw input (core-local agents journal untrimmed).
// "" when the call isn't a file edit. Lets the consoles offer a "diff" control on any edit.
func ToolDiff(tc *ToolCall) string {
	if tc == nil {
		return ""
	}
	if d, ok := tc.Input["_diff"].(string); ok && d != "" {
		return d
	}
	return PatchPreview(tc.Name, tc.Input)
}

func diffOf(file, old, nw string) string {
	name := file
	if name == "" {
		name = "file"
	}
	return udiff.Unified(name+" (before)", name+" (after)", old, nw)
}

func firstString(in map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := in[k].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

func capLines(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxPatchLines {
		return s
	}
	return strings.Join(lines[:maxPatchLines], "\n") + fmt.Sprintf("\n… (%d more lines truncated)", len(lines)-maxPatchLines)
}
