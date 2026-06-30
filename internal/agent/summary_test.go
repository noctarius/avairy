package agent

import "testing"

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
