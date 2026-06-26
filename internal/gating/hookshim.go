package gating

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// This file is the client side of the Claude PreToolUse hook, shared by the avairy and
// avairy-node binaries (both spawn Claude and both expose a local /gate endpoint backed by
// HookHandler). RunHookShim is the `hook` subcommand Claude executes; ClaudeHookSettings
// builds the --settings JSON that registers it.

// RunHookShim is the `<binary> hook -gate <url>` subcommand. Claude Code's PreToolUse hook
// executes it once per tool call, passing the tool call as JSON on stdin (see ADAPTERS.md).
// It relays that to the local gate endpoint and writes the gate's permissionDecision JSON to
// stdout.
//
// It fails CLOSED: any transport error (gate down, timeout, non-200) yields a deny, because a
// security gate that cannot be reached must never silently allow. The 290s client timeout sits
// just under the hook's 300s timeout (gated actions may wait on a human) so we return a deny
// rather than letting the turn time out.
func RunHookShim(argv []string) {
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	gate := fs.String("gate", "", "local gate endpoint URL")
	_ = fs.Parse(argv)

	body, _ := io.ReadAll(os.Stdin)
	if *gate == "" {
		hookDeny("no gate endpoint configured")
		return
	}
	client := &http.Client{Timeout: 290 * time.Second}
	resp, err := client.Post(*gate, "application/json", bytes.NewReader(body))
	if err != nil {
		hookDeny(fmt.Sprintf("gate unreachable: %v", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		hookDeny(fmt.Sprintf("gate error: %s", resp.Status))
		return
	}
	_, _ = io.Copy(os.Stdout, resp.Body)
}

// hookDeny emits a PreToolUse deny decision (exit 0 so Claude reads the JSON).
func hookDeny(reason string) {
	fmt.Printf(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":%q}}`+"\n", reason)
}

// ClaudeHookSettings builds the Claude `--settings` JSON registering a PreToolUse hook for
// every tool call. The hook command is this same binary's `hook` subcommand pointed at
// gateURL, so the hook *is* the permission system (allow free actions, deny/route gated ones)
// — no need to bypass permissions. 300s timeout so a human has time to rule in the TUI.
func ClaudeHookSettings(gateURL string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate self for hook: %w", err)
	}
	command := fmt.Sprintf("%q hook -gate %q", exe, gateURL)
	settings := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "*",
					"hooks": []any{
						map[string]any{"type": "command", "command": command, "timeout": 300},
					},
				},
			},
		},
	}
	b, err := json.Marshal(settings)
	return string(b), err
}
