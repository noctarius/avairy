// Command avairy launches the single-machine collaboration TUI wired to two mock agents,
// so the loop is interactive with zero agent credits: type "@alice <msg>" to message an
// agent and watch the echo flow back through the bus and journal into the UI.
//
// Real agent adapters (Claude Code / Codex) and the node daemon land in later milestones.
package main

import (
	"context"
	"fmt"
	"os"

	"avairy/internal/adapter/mock"
	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/journal"
	"avairy/internal/runner"
	"avairy/internal/tui"
)

func main() {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	bd := board.New(jrnl)

	// Seed a capability-scoped task (DESIGN.md §4).
	bd.Post("human", "reproduce the linux-only panic", map[string]string{"os": "linux"}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two mock agents on the bus; each echoes what it's told.
	for _, id := range []string{"alice", "bob"} {
		ad := mock.New()
		sess, err := ad.Start(ctx, agent.SessionConfig{AgentID: id, Role: "backend dev"})
		if err != nil {
			fmt.Fprintln(os.Stderr, "start", id, ":", err)
			os.Exit(1)
		}
		go runner.New(runner.Agent{ID: id, Roles: []string{"backend"}}, sess, b, jrnl).Run(ctx)
	}

	if err := tui.Run(tui.Deps{Bus: b, Board: bd, Journal: jrnl}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
