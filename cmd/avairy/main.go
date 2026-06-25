// Command avairy runs the single-machine collaboration loop.
//
//	go run ./cmd/avairy                 # interactive TUI, two mock agents (zero credits)
//	go run ./cmd/avairy -live           # alice is a real Claude Code agent on the MCP bus
//	go run ./cmd/avairy -live -headless "create a task titled ping"
//	                                    # one real turn, print the journal, exit (for verification)
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"time"

	"avairy/internal/adapter/claudecode"
	"avairy/internal/adapter/codex"
	"avairy/internal/adapter/mock"
	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/control"
	"avairy/internal/journal"
	"avairy/internal/mcp"
	"avairy/internal/runner"
	"avairy/internal/tui"
	"avairy/internal/workspace"
)

func main() {
	live := flag.Bool("live", false, "run 'alice' as a real agent on the MCP bus")
	family := flag.String("family", "claude", "live agent family: claude | codex")
	headless := flag.String("headless", "", "send this message to alice, print the journal, and exit (no TUI)")
	model := flag.String("model", "haiku", "model for the live agent (kept cheap by default; ignored for codex unless set)")
	controlAddr := flag.String("control-addr", "", "if set, serve the node control API here (enrollment/sync) and print an enroll token")
	flag.Parse()

	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	bd := board.New(jrnl)
	mcpSrv := mcp.NewServer(b, bd, jrnl)

	// Serve the MCP bus on a loopback port; agents connect here (the daemon will tunnel
	// this for remote nodes — DESIGN.md §4).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail("listen", err)
	}
	go http.Serve(ln, mcpSrv.HTTPHandler())
	busURL := "http://" + ln.Addr().String() + mcp.EndpointPath

	// Optionally serve the node control API so remote avairy-node daemons can enroll and sync.
	if *controlAddr != "" {
		core := control.NewCore(workspace.NewHub(), jrnl)
		go func() {
			if err := http.ListenAndServe(*controlAddr, core.Handler()); err != nil {
				fmt.Fprintln(os.Stderr, "control server:", err)
			}
		}()
		fmt.Printf("control API on %s\nMCP bus base: http://%s\nenroll token: %s\n\n",
			*controlAddr, ln.Addr().String(), core.IssueEnrollToken())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	caps := map[string]string{"os": runtime.GOOS}

	// alice: real agent (claude|codex) when -live, else a mock.
	mcpSrv.RegisterAgent("alice", []string{"backend"}, caps)
	startAlice(ctx, *live, *family, *model, busURL, b, jrnl)

	// bob: always a mock peer, so cross-agent messaging works without extra credits.
	mcpSrv.RegisterAgent("bob", []string{"backend"}, caps)
	startMock(ctx, "bob", b, jrnl)

	if *headless != "" {
		runHeadless(b, jrnl, *headless)
		return
	}
	if err := tui.Run(tui.Deps{Bus: b, Board: bd, Journal: jrnl}); err != nil {
		fail("tui", err)
	}
}

const aliceRole = "You are 'alice', a backend engineer agent in the avairy multi-agent system. " +
	"Collaborate ONLY through the avairy MCP tools: post_task, claim_task, list_tasks, " +
	"send_message, read_inbox, report_status. Be terse and do exactly what you are asked, then stop."

func startAlice(ctx context.Context, live bool, family, model, busURL string, b *bus.Bus, jrnl journal.Log) {
	if !live {
		startMock(ctx, "alice", b, jrnl)
		return
	}
	ws, err := os.MkdirTemp("", "avairy-alice-")
	if err != nil {
		fail("workspace", err)
	}
	cfg := agent.SessionConfig{
		AgentID:   "alice",
		Role:      aliceRole,
		Workspace: ws,
		Model:     model,
		MCP: []agent.MCPServer{{
			Name:    "avairy",
			Type:    "http",
			URL:     busURL,
			Headers: map[string]string{"X-Avairy-Agent": "alice"},
		}},
	}

	var ad agent.Adapter
	switch family {
	case "codex":
		if cfg.Model == "haiku" { // the claude-flavored default isn't a codex model
			cfg.Model = ""
		}
		ad = codex.New() // approvalPolicy=never auto-runs the bus tools (gating is a later milestone)
	default: // claude
		ca := claudecode.New()
		// Pre-approve the avairy bus tools so headless turns don't stall on permission prompts.
		ca.ExtraArgs = []string{
			"--allowedTools", "mcp__avairy__post_task,mcp__avairy__claim_task,mcp__avairy__list_tasks,mcp__avairy__send_message,mcp__avairy__read_inbox,mcp__avairy__report_status",
		}
		ad = ca
	}

	sess, err := ad.Start(ctx, cfg)
	if err != nil {
		fail("start alice", err)
	}
	go runner.New(runner.Agent{ID: "alice", Roles: []string{"backend"}}, sess, b, jrnl).Run(ctx)
}

func startMock(ctx context.Context, id string, b *bus.Bus, jrnl journal.Log) {
	sess, err := mock.New().Start(ctx, agent.SessionConfig{AgentID: id, Role: "backend dev"})
	if err != nil {
		fail("start "+id, err)
	}
	go runner.New(runner.Agent{ID: id, Roles: []string{"backend"}}, sess, b, jrnl).Run(ctx)
}

// runHeadless sends one message to alice, waits for her turn to complete, and prints the journal.
func runHeadless(b *bus.Bus, jrnl journal.Log, msg string) {
	sub, cancelSub := jrnl.Subscribe()
	defer cancelSub()

	b.Publish("human", bus.Agent("alice"), msg, agent.DeliverySteer)

	deadline := time.After(180 * time.Second)
	for {
		select {
		case rec := <-sub:
			if rec.Kind == journal.KindAgentEvent && rec.Actor == "alice" {
				if ev, ok := rec.Data.(agent.Event); ok && ev.Type == agent.EventTurnDone {
					goto done
				}
			}
		case <-deadline:
			fmt.Fprintln(os.Stderr, "timeout waiting for alice's turn")
			goto done
		}
	}
done:
	fmt.Println("=== journal ===")
	for _, rec := range jrnl.Records() {
		fmt.Printf("  #%d %-12s actor=%-8s %s\n", rec.Seq, rec.Kind, rec.Actor, summarize(rec.Data))
	}
}

func summarize(data any) string {
	switch v := data.(type) {
	case bus.Message:
		return fmt.Sprintf("%s -> %s:%s %q", v.From, v.To.Kind, v.To.Value, v.Body)
	case agent.Event:
		if v.Tool != nil {
			return fmt.Sprintf("%s tool=%s %v", v.Type, v.Tool.Name, v.Tool.Input)
		}
		if v.Usage != nil {
			return fmt.Sprintf("%s ($%.4f)", v.Type, v.Usage.CostUSD)
		}
		return fmt.Sprintf("%s %q", v.Type, v.Text)
	case board.Task:
		return fmt.Sprintf("task %s [%s] %q requires=%v claimant=%s", v.ID, v.State, v.Title, v.Requires, v.Claimant)
	default:
		return fmt.Sprintf("%v", data)
	}
}

func fail(what string, err error) {
	fmt.Fprintln(os.Stderr, "avairy:", what, ":", err)
	os.Exit(1)
}
