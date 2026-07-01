package agent

import (
	"fmt"
	"sort"
	"strings"

	udiff "github.com/aymanbagabas/go-udiff"
)

// maxPatchLines caps a diff preview so a huge edit can't bloat the approval payload or the UI.
const maxPatchLines = 400

// PatchPreview renders a human-readable diff of a file-editing tool call, for the approval card's
// "show diff". It handles the families' different shapes:
//   - an explicit unified patch — patch/diff/unified_diff (codex apply_patch, or any tool);
//   - an old→new string replacement — old_string/new_string (Claude Edit) or oldText/newText (ACP);
//   - a list of edits (Claude MultiEdit);
//   - a per-path changes map (codex apply_patch: path → {unified_diff | old/new | content});
//   - a full new body — content/file_text (Write/NotebookEdit).
//
// Returns "" when there's nothing diff-like to show (e.g. a shell command), so callers can decide
// whether to offer the control. Output is capped.
func PatchPreview(toolName string, input map[string]any) string {
	if input == nil {
		return ""
	}
	file, _ := firstString(input, "file_path", "filePath", "path", "abs_path", "absPath")

	// 1. An explicit unified patch/diff.
	if p, ok := firstString(input, "patch", "diff", "unified_diff", "unifiedDiff"); ok {
		return capLines(p)
	}
	// 2. An old→new replacement (Claude Edit; ACP and others via *Text; empty side = insert/delete).
	if d, ok := oldNewDiff(file, input); ok {
		return capLines(d)
	}
	// 3. A list of edits (Claude MultiEdit): [{old_string, new_string}, …].
	if edits, ok := input["edits"].([]any); ok {
		var b strings.Builder
		for _, e := range edits {
			if m, ok := e.(map[string]any); ok {
				if d, ok := oldNewDiff(file, m); ok {
					b.WriteString(d + "\n")
				}
			}
		}
		if b.Len() > 0 {
			return capLines(b.String())
		}
	}
	// 4. A per-path changes map (codex apply_patch): { "<path>": {unified_diff | old/new | content} }.
	if changes, ok := input["changes"].(map[string]any); ok {
		var b strings.Builder
		for _, path := range sortedKeys(changes) {
			m, ok := changes[path].(map[string]any)
			if !ok {
				continue
			}
			if p, ok := firstString(m, "unified_diff", "diff", "patch"); ok {
				b.WriteString(p + "\n")
			} else if d, ok := oldNewDiff(path, m); ok {
				b.WriteString(d + "\n")
			} else if body, ok := firstString(m, "content", "new_content", "contents"); ok {
				b.WriteString(diffOf(path, "", body) + "\n")
			}
		}
		if b.Len() > 0 {
			return capLines(b.String())
		}
	}
	// 4b. A changes array (codex app-server fileChange): [{path, kind, diff}, …]. Each diff is
	// already unified; prefix the path (unless the diff already names it) so multi-file changes
	// stay legible.
	if arr, ok := input["changes"].([]any); ok {
		var b strings.Builder
		for _, e := range arr {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			path, _ := firstString(m, "path", "file_path")
			if p, ok := firstString(m, "diff", "unified_diff", "patch"); ok {
				if path != "" && !strings.Contains(p, path) {
					fmt.Fprintf(&b, "--- %s\n", path)
				}
				b.WriteString(p + "\n")
			} else if d, ok := oldNewDiff(path, m); ok {
				b.WriteString(d + "\n")
			} else if body, ok := firstString(m, "content", "new_content", "contents"); ok {
				b.WriteString(diffOf(path, "", body) + "\n")
			}
		}
		if b.Len() > 0 {
			return capLines(b.String())
		}
	}
	// 5. A full new body (Write / NotebookEdit / create): show it as an all-added diff.
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

// oldNewDiff builds a diff from an old→new pair keyed under any of the families' names. It detects
// the *keys* (not non-empty values), so a pure insertion (empty old) or deletion (empty new) still
// renders. Returns false when neither side's key is present.
func oldNewDiff(file string, m map[string]any) (string, bool) {
	old, oOK := stringByKey(m, "old_string", "oldText", "old_text", "old")
	nw, nOK := stringByKey(m, "new_string", "newText", "new_text", "new")
	if !oOK && !nOK {
		return "", false
	}
	return diffOf(file, old, nw), true
}

func diffOf(file, old, nw string) string {
	name := file
	if name == "" {
		name = "file"
	}
	return udiff.Unified(name+" (before)", name+" (after)", old, nw)
}

// firstString returns the first key whose value is a non-empty string (for identifiers/patches).
func firstString(in map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := in[k].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// stringByKey returns the first key that is *present* and a string (empty allowed).
func stringByKey(in map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := in[k]; ok {
			if s, ok := v.(string); ok {
				return s, true
			}
		}
	}
	return "", false
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func capLines(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxPatchLines {
		return s
	}
	return strings.Join(lines[:maxPatchLines], "\n") + fmt.Sprintf("\n… (%d more lines truncated)", len(lines)-maxPatchLines)
}
