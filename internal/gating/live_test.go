package gating

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestLiveClaudeHook validates the real wiring (#7): a live `claude` parses the injected
// --settings, fires the PreToolUse hook on a tool call, the hook shim relays it to our /gate, and
// HookHandler's decision governs. It spawns real claude (≈1 cheap haiku turn), so it's gated behind
// an env var and never runs in normal/CI runs.
//
//	AVAIRY_CLAUDE_HOOK_LIVE=1 go test ./internal/gating -run TestLiveClaudeHook -v
func TestLiveClaudeHook(t *testing.T) {
	if os.Getenv("AVAIRY_CLAUDE_HOOK_LIVE") == "" {
		t.Skip("set AVAIRY_CLAUDE_HOOK_LIVE=1 to run the live claude hook test (spends a little credit)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not found on PATH")
	}

	// Build the avairy binary so the hook command (`<avairy> hook -gate <url>`) is real.
	bin := filepath.Join(t.TempDir(), "avairy")
	build := exec.Command("go", "build", "-o", bin, "avairy/cmd/avairy")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build avairy: %v\n%s", err, out)
	}

	// Local /gate that records each hook call and allows it (so claude proceeds — cheap + fast).
	var fired atomic.Int32
	var lastSummary atomic.Value
	lastSummary.Store("")
	decide := func(_ context.Context, req Request) (Decision, error) {
		fired.Add(1)
		lastSummary.Store(string(req.Kind) + " " + req.Summary)
		return Allow, nil
	}
	srv := httptest.NewServer(HookHandler(decide))
	defer srv.Close()

	// The same --settings JSON the adapter injects, but pointed at the freshly built binary.
	command := fmt.Sprintf("%q hook -gate %q", bin, srv.URL)
	settings, err := json.Marshal(map[string]any{"hooks": map[string]any{"PreToolUse": []any{
		map[string]any{"matcher": "*", "hooks": []any{
			map[string]any{"type": "command", "command": command, "timeout": 300},
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p",
		"Use the Bash tool to run exactly this command and nothing else: echo avairy-hook-ok",
		"--settings", string(settings), "--model", "haiku")
	cmd.Dir = t.TempDir() // a scratch workspace for the echo
	out, err := cmd.CombinedOutput()
	t.Logf("claude output:\n%s", out)
	if err != nil {
		t.Fatalf("run claude: %v (is it logged in? `claude` auth)", err)
	}

	if fired.Load() == 0 {
		t.Fatal("PreToolUse hook never reached /gate — claude didn't parse --settings or didn't use a tool")
	}
	t.Logf("hook fired %d time(s); first gated action: %s", fired.Load(), lastSummary.Load())
}
