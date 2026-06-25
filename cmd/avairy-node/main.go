// Command avairy-node is the avairy node daemon: a single cross-platform binary that
// enrolls with core, serves a local MCP proxy for agents on this machine, continuously
// syncs a workspace directory to/from the canonical hub, and heartbeats. It dials core
// (node→core outbound, NAT-friendly); the channel is HTTP here and TLS in production.
//
//	avairy-node -core http://core:7700 -core-mcp http://core:7701 -token <T> \
//	            -id linux-box -agent alice -workspace ./repo -proxy 127.0.0.1:7800
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"avairy/internal/adapter/claudecode"
	"avairy/internal/adapter/codex"
	"avairy/internal/agent"
	"avairy/internal/control"
	"avairy/internal/gating"
)

func main() {
	core := flag.String("core", "", "core control API base URL (required)")
	coreMCP := flag.String("core-mcp", "", "core MCP bus base URL for the local proxy")
	token := flag.String("token", "", "one-time enrollment token (required)")
	id := flag.String("id", "", "node id (required)")
	agentID := flag.String("agent", "", "agent id this node hosts (for the MCP proxy identity)")
	osName := flag.String("os", runtime.GOOS, "node OS capability")
	ws := flag.String("workspace", "", "workspace directory to sync (optional)")
	proxy := flag.String("proxy", "127.0.0.1:7800", "local MCP proxy listen address")
	interval := flag.Duration("interval", 2*time.Second, "sync/heartbeat interval")
	family := flag.String("family", "", "spawn & drive the agent here: claude | codex (empty = proxy only, run the agent yourself)")
	model := flag.String("model", "", "model for the spawned agent (family default if empty)")
	role := flag.String("role", "", "system prompt / role for the spawned agent")
	flag.Parse()

	if *core == "" || *token == "" || *id == "" {
		fmt.Fprintln(os.Stderr, "avairy-node: -core, -token and -id are required")
		os.Exit(2)
	}

	// The local workspace is this node's synced copy; create it if absent (it gets populated
	// by SyncDown from the canonical hub).
	if *ws != "" {
		if err := os.MkdirAll(*ws, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node: workspace:", err)
			os.Exit(1)
		}
	}

	n := control.NewNode(*core, *id)
	if err := n.Enroll(*token, *agentID, *osName, map[string]string{"os": *osName}); err != nil {
		fmt.Fprintln(os.Stderr, "avairy-node: enroll:", err)
		os.Exit(1)
	}
	fmt.Printf("enrolled node %q (os=%s) with core %s\n", *id, *osName, *core)

	// Local MCP proxy → core bus, stamping this node's agent identity.
	if *coreMCP != "" && *agentID != "" {
		h, err := n.MCPProxy(*coreMCP, *agentID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node: proxy:", err)
			os.Exit(1)
		}
		go func() {
			fmt.Printf("MCP proxy for agent %q at http://%s/mcp → %s\n", *agentID, *proxy, *coreMCP)
			if err := http.ListenAndServe(*proxy, h); err != nil {
				fmt.Fprintln(os.Stderr, "avairy-node: proxy server:", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Optionally spawn & drive the agent on this node, wired to the local MCP proxy.
	if *family != "" {
		if *coreMCP == "" || *agentID == "" {
			fmt.Fprintln(os.Stderr, "avairy-node: -family requires -core-mcp and -agent")
			os.Exit(2)
		}
		if err := spawnAgent(ctx, n, *family, *agentID, *role, *model, *ws, *proxy); err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node: spawn agent:", err)
			os.Exit(1)
		}
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("avairy-node: shutting down")
			return
		case <-ticker.C:
			if err := n.Heartbeat(); err != nil {
				fmt.Fprintln(os.Stderr, "heartbeat:", err)
			}
			if *ws == "" {
				continue
			}
			conflicts, err := n.SyncUp(*ws)
			if err != nil {
				fmt.Fprintln(os.Stderr, "syncUp:", err)
			}
			for _, c := range conflicts {
				fmt.Printf("CONFLICT %s (hub v%d) — needs reconciliation\n", c.Path, c.HubVersion)
			}
			if err := n.SyncDown(*ws); err != nil {
				fmt.Fprintln(os.Stderr, "syncDown:", err)
			}
		}
	}
}

const defaultRole = "You are an avairy agent. Collaborate ONLY through the avairy MCP tools " +
	"(send_message, read_inbox, post_task, claim_task, list_tasks, report_status). Be terse."

// spawnAgent starts an agent on this node wired to the local MCP proxy, ships its events to
// the core journal, and injects inbound bus messages (pulled from core) into its session.
func spawnAgent(ctx context.Context, n *control.Node, family, agentID, role, model, ws, proxy string) error {
	if role == "" {
		role = defaultRole
	}
	_, pport, err := net.SplitHostPort(proxy)
	if err != nil {
		return err
	}
	proxyURL := "http://127.0.0.1:" + pport + "/mcp"

	ad, err := buildAdapter(family)
	if err != nil {
		return err
	}
	sess, err := ad.Start(ctx, agent.SessionConfig{
		AgentID:   agentID,
		Role:      role,
		Workspace: ws,
		Model:     model,
		MCP:       []agent.MCPServer{{Name: "avairy", Type: "http", URL: proxyURL}},
	})
	if err != nil {
		return err
	}
	fmt.Printf("spawned %s agent %q → bus via %s\n", family, agentID, proxyURL)

	// Ship the agent's events to the core journal (so they appear in the operator TUI).
	go func() {
		for ev := range sess.Events() {
			r := control.AgentEventReport{AgentID: agentID, Type: string(ev.Type), Text: ev.Text}
			if ev.Tool != nil {
				r.Tool = ev.Tool.Name
			}
			if ev.Usage != nil {
				r.CostUSD = ev.Usage.CostUSD
			}
			_ = n.PostEvents([]control.AgentEventReport{r})
		}
	}()

	// Pull inbound bus messages from core and inject them into the agent (the node-side runner).
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = sess.Close()
				return
			case <-t.C:
				msgs, err := n.PullInbox(agentID)
				if err != nil {
					continue
				}
				for _, m := range msgs {
					_ = sess.Send(ctx, m.Body, agent.DeliverySteer)
				}
			}
		}
	}()
	return nil
}

func buildAdapter(family string) (agent.Adapter, error) {
	switch family {
	case "claude":
		ca := claudecode.New()
		ca.ExtraArgs = []string{"--allowedTools", "mcp__avairy__post_task,mcp__avairy__claim_task,mcp__avairy__list_tasks,mcp__avairy__send_message,mcp__avairy__read_inbox,mcp__avairy__report_status"}
		return ca, nil
	case "codex":
		cx := codex.New()
		cx.Approve = codex.ApproverFromDecider(gating.Policy{}.Decide)
		return cx, nil
	default:
		return nil, fmt.Errorf("unknown family %q (want claude|codex)", family)
	}
}
