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
