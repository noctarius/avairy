package agent

import (
	"strings"
	"testing"
)

func TestToolSummary(t *testing.T) {
	cases := []struct {
		tc   *ToolCall
		want string
	}{
		{&ToolCall{Name: "Bash", Input: map[string]any{"command": "go test ./..."}}, "Bash: go test ./..."},
		{&ToolCall{Name: "Read", Input: map[string]any{"file_path": "src/main.go"}}, "Read: src/main.go"},
		{&ToolCall{Name: "Grep", Input: map[string]any{"pattern": "TODO"}}, "Grep: TODO"},
		{&ToolCall{Name: "Bash", Input: map[string]any{"command": "echo hi\nmore"}}, "Bash: echo hi …"}, // first line only
		{&ToolCall{Name: "Mystery"}, "Mystery"},                                                         // no identifying arg
		{nil, ""},
	}
	for _, c := range cases {
		if got := ToolSummary(c.tc); got != c.want {
			t.Errorf("ToolSummary(%+v) = %q, want %q", c.tc, got, c.want)
		}
	}
}

func TestActionKey(t *testing.T) {
	edit := func(oldS, newS string) *ToolCall {
		return &ToolCall{Name: "Edit", Input: map[string]any{"file_path": "f.go", "old_string": oldS, "new_string": newS}}
	}
	// Same file, different content → different keys; identical content → identical key.
	if a, b := ActionKey(edit("x", "y")), ActionKey(edit("p", "q")); a == b {
		t.Fatalf("different edits should key differently, both = %q", a)
	}
	if a, b := ActionKey(edit("x", "y")), ActionKey(edit("x", "y")); a != b {
		t.Fatalf("identical edits should key identically, %q vs %q", a, b)
	}
	// The digest survives TrimInput (the node path) and yields the same key as the raw input.
	raw := edit("x", "y")
	trimmed := &ToolCall{Name: "Edit", Input: TrimInput(raw.Input)}
	if ActionKey(raw) != ActionKey(trimmed) {
		t.Fatalf("trimmed edit should key like the raw one: %q vs %q", ActionKey(trimmed), ActionKey(raw))
	}
	// Reads at different offsets differ; same offset matches.
	read := func(off int) *ToolCall {
		return &ToolCall{Name: "Read", Input: map[string]any{"file_path": "f.go", "offset": off}}
	}
	if ActionKey(read(0)) == ActionKey(read(200)) {
		t.Fatal("reads at different offsets should key differently")
	}
	if ActionKey(read(0)) != ActionKey(read(0)) {
		t.Fatal("reads at the same offset should key identically")
	}
	// codex fileChange: distinct edits (different paths/diffs) must key differently, so three
	// separate fileChanges aren't mistaken for a stuck loop by the facilitator.
	fc := func(path, diff string) *ToolCall {
		return &ToolCall{Name: "fileChange", Input: map[string]any{"type": "fileChange", "changes": []any{
			map[string]any{"path": path, "kind": "update", "diff": diff},
		}}}
	}
	if a, b := ActionKey(fc("a.c", "-x\n+y")), ActionKey(fc("b.c", "-p\n+q")); a == b {
		t.Fatalf("distinct fileChanges should key differently, both = %q", a)
	}
	if a, b := ActionKey(fc("a.c", "-x\n+y")), ActionKey(fc("a.c", "-x\n+y")); a != b {
		t.Fatalf("identical fileChanges should key identically, %q vs %q", a, b)
	}
	// And the key survives TrimInput (the node-shipped path drops the bodies, keeps a digest).
	rawFC := fc("a.c", "-x\n+y")
	trimFC := &ToolCall{Name: "fileChange", Input: TrimInput(rawFC.Input)}
	if ActionKey(rawFC) != ActionKey(trimFC) {
		t.Fatalf("trimmed fileChange should key like the raw one: %q vs %q", ActionKey(trimFC), ActionKey(rawFC))
	}
}

func TestPatchPreviewAndToolDiff(t *testing.T) {
	// Claude Edit: old→new becomes a unified diff mentioning both sides.
	edit := map[string]any{"file_path": "f.go", "old_string": "a := 1", "new_string": "a := 2"}
	d := PatchPreview("Edit", edit)
	if !strings.Contains(d, "-a := 1") || !strings.Contains(d, "+a := 2") {
		t.Fatalf("edit diff should show both sides:\n%s", d)
	}
	// codex apply_patch: an explicit unified patch is passed through.
	if got := PatchPreview("apply_patch", map[string]any{"patch": "@@ -1 +1 @@\n-x\n+y"}); !strings.Contains(got, "+y") {
		t.Fatalf("explicit patch should pass through, got %q", got)
	}
	// codex apply_patch: a per-path changes map, each with a unified_diff.
	changes := map[string]any{"changes": map[string]any{"a.go": map[string]any{"unified_diff": "@@ -1 +1 @@\n-p\n+q"}}}
	if got := PatchPreview("apply_patch", changes); !strings.Contains(got, "+q") {
		t.Fatalf("changes map should render each file's diff, got %q", got)
	}
	// codex app-server fileChange: `changes` is an ARRAY of {path, kind, diff} — each diff is
	// already a unified diff and must render (otherwise the console shows no diff link).
	fc := map[string]any{"type": "fileChange", "changes": []any{
		map[string]any{"path": "src/foo.c", "kind": "update", "diff": "@@ -1 +1 @@\n-old\n+new"},
	}}
	if got := PatchPreview("fileChange", fc); !strings.Contains(got, "+new") || !strings.Contains(got, "src/foo.c") {
		t.Fatalf("fileChange array should render each change's diff with its path, got %q", got)
	}
	// ACP edit: oldText/newText (and an empty old side = pure insertion) render.
	if got := PatchPreview("edit", map[string]any{"file_path": "f.go", "oldText": "", "newText": "hi"}); !strings.Contains(got, "+hi") {
		t.Fatalf("oldText/newText should render, got %q", got)
	}
	// A shell command has no diff.
	if got := PatchPreview("Bash", map[string]any{"command": "go test ./..."}); got != "" {
		t.Fatalf("a command has no diff, got %q", got)
	}
	// ToolDiff prefers the precomputed "_diff" (the node/shipped path) over re-rendering.
	tc := &ToolCall{Name: "Edit", Input: map[string]any{"_diff": "precomputed", "old_string": "a", "new_string": "b"}}
	if got := ToolDiff(tc); got != "precomputed" {
		t.Fatalf("ToolDiff should use _diff, got %q", got)
	}
	// TrimInput leaves a _diff behind for an edit (and still drops the bodies).
	out := TrimInput(edit)
	if _, ok := out["_diff"].(string); !ok {
		t.Fatalf("TrimInput should leave a _diff for an edit, got %v", out)
	}
}

func TestTrimInput(t *testing.T) {
	in := map[string]any{
		"file_path":  "a.go",
		"content":    "a very large file body that should not be shipped",
		"old_string": "x",
		"line":       42,
	}
	out := TrimInput(in)
	if _, ok := out["content"]; ok {
		t.Error("content (a body) should be dropped")
	}
	if _, ok := out["old_string"]; ok {
		t.Error("old_string (a diff side) should be dropped")
	}
	if out["file_path"] != "a.go" {
		t.Errorf("file_path should be kept, got %v", out["file_path"])
	}
	if out["line"] != 42 {
		t.Errorf("non-string values should be kept, got %v", out["line"])
	}
	if TrimInput(nil) != nil {
		t.Error("nil input should return nil")
	}
}
