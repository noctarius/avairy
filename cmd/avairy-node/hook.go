package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// runHook is the `avairy-node hook` subcommand. Claude Code's PreToolUse hook executes it
// once per tool call, passing the tool call as JSON on stdin (see ADAPTERS.md). It relays
// that payload to the node's local gate endpoint and writes the gate's permissionDecision
// JSON to stdout. The node configures Claude to invoke this via --settings on spawn.
//
// It fails CLOSED: any transport error (gate down, timeout, non-200) yields a deny, because a
// security gate that cannot be reached must never silently allow. The 55s client timeout sits
// just under the hook's 60s timeout so we return a deny rather than letting the turn time out.
func runHook(argv []string) {
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	gate := fs.String("gate", "", "node gate endpoint URL")
	_ = fs.Parse(argv)

	body, _ := io.ReadAll(os.Stdin)
	if *gate == "" {
		denyClosed("no gate endpoint configured")
		return
	}
	client := &http.Client{Timeout: 55 * time.Second}
	resp, err := client.Post(*gate, "application/json", bytes.NewReader(body))
	if err != nil {
		denyClosed(fmt.Sprintf("gate unreachable: %v", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		denyClosed(fmt.Sprintf("gate error: %s", resp.Status))
		return
	}
	_, _ = io.Copy(os.Stdout, resp.Body)
}

// denyClosed emits a PreToolUse deny decision (exit 0 so Claude reads the JSON).
func denyClosed(reason string) {
	fmt.Printf(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":%q}}`+"\n", reason)
}
