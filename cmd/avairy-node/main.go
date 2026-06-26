// Command avairy-node is the avairy node daemon: a single cross-platform binary that
// enrolls with core, serves a local MCP proxy for agents on this machine, continuously
// syncs a workspace directory to/from the canonical hub, and heartbeats. It dials core
// (node→core outbound, NAT-friendly); the channel is HTTP here and TLS in production.
//
//	avairy-node -core http://core:7700 -core-mcp http://core:7701 -token <T> \
//	            -id linux-box -workspace ./repo -proxy 127.0.0.1:7800
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"avairy/internal/adapter/claudecode"
	"avairy/internal/adapter/codex"
	"avairy/internal/adapter/copilot"
	"avairy/internal/adapter/grok"
	"avairy/internal/agent"
	"avairy/internal/control"
	"avairy/internal/gating"
	"avairy/internal/git"
	"avairy/internal/workspace"
)

func main() {
	// `avairy-node hook -gate <url>` is the PreToolUse hook shim Claude invokes per tool call;
	// it must run before flag parsing (its args are its own).
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		gating.RunHookShim(os.Args[2:])
		return
	}

	core := flag.String("core", "", "core control API base URL (required)")
	coreMCP := flag.String("core-mcp", "", "core MCP bus base URL for the local proxy")
	token := flag.String("token", "", "one-time enrollment token (required)")
	id := flag.String("id", "", "node id — also the agent's bus identity (required). Run one process per agent.")
	osName := flag.String("os", runtime.GOOS, "node OS capability")
	ws := flag.String("workspace", "", "workspace directory to sync (optional)")
	proxy := flag.String("proxy", "127.0.0.1:7800", "local MCP proxy listen address")
	interval := flag.Duration("interval", 2*time.Second, "sync/heartbeat interval")
	family := flag.String("family", "", "spawn & drive the agent here: claude | codex | copilot | grok (empty = proxy only, run the agent yourself)")
	model := flag.String("model", "", "model for the spawned agent (family default if empty)")
	role := flag.String("role", "", "system prompt / role for the spawned agent")
	caFile := flag.String("ca", "", "PEM cert/CA to trust for an https core (self-signed/internal CA)")
	insecure := flag.Bool("insecure", false, "skip TLS verification for an https core (DEV ONLY — exposes the channel to MITM)")
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
	// Trust config for an https core: a CA/cert PEM the node trusts, or insecure (dev). With a
	// publicly-trusted cert neither is needed (system roots apply).
	if *caFile != "" || *insecure {
		client, err := control.TLSClient(*caFile, *insecure)
		if err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node: tls:", err)
			os.Exit(1)
		}
		n.HTTP = client
	}
	if err := n.Enroll(*token, *osName, map[string]string{"os": *osName}); err != nil {
		fmt.Fprintln(os.Stderr, "avairy-node: enroll:", err)
		os.Exit(1)
	}
	fmt.Printf("enrolled node %q (os=%s) with core %s\n", *id, *osName, *core)

	// Local HTTP server for agents on this machine: the MCP proxy → core bus (stamping this
	// node's identity == agent id) plus the /gate endpoint the Claude PreToolUse hook calls.
	if *coreMCP != "" {
		h, err := n.MCPProxy(*coreMCP, *id)
		if err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node: proxy:", err)
			os.Exit(1)
		}
		mux := http.NewServeMux()
		mux.Handle("/gate", gating.HookHandler(gateDecider(n, *id)))
		mux.Handle("/", h) // MCP proxy (serves /mcp)
		go func() {
			fmt.Printf("MCP proxy for agent %q at http://%s/mcp → %s (gate at /gate)\n", *id, *proxy, *coreMCP)
			if err := http.ListenAndServe(*proxy, mux); err != nil {
				fmt.Fprintln(os.Stderr, "avairy-node: proxy server:", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Maintain a read-only mirror of core's repo so the agent can bisect/build past commits
	// locally — on this node's OS — without commit rights (DESIGN.md §9). Lives under the
	// sync-excluded .avairy dir; refreshed in the background, best-effort.
	mirrorDir := ""
	if *ws != "" {
		mirrorDir = filepath.Join(*ws, ".avairy", "mirror.git")
		go func() {
			refreshMirror(ctx, n, mirrorDir)
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					refreshMirror(ctx, n, mirrorDir)
				}
			}
		}()
	}

	// Optionally spawn & drive the agent on this node, wired to the local MCP proxy.
	if *family != "" {
		if *coreMCP == "" {
			fmt.Fprintln(os.Stderr, "avairy-node: -family requires -core-mcp")
			os.Exit(2)
		}
		if err := spawnAgent(ctx, n, *family, *id, *role, *model, *ws, *proxy, mirrorDir); err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node: spawn agent:", err)
			os.Exit(1)
		}
	}

	syncUp := func() {
		if *ws == "" {
			return
		}
		conflicts, err := n.SyncUp(*ws)
		if err != nil {
			fmt.Fprintln(os.Stderr, "syncUp:", err)
		}
		for _, c := range conflicts {
			fmt.Printf("CONFLICT %s (hub v%d) — needs reconciliation\n", c.Path, c.HubVersion)
		}
	}

	// Push local edits the instant they happen (fsnotify), with the ticker as the fallback poll
	// and the driver for heartbeat + SyncDown (pulling others' changes can't be event-driven —
	// there's no server→node push).
	var watch <-chan struct{}
	if *ws != "" {
		if ch, err := workspace.Watch(ctx, *ws, workspace.IgnoreFor(*ws)); err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node: watch (falling back to poll):", err)
		} else {
			watch = ch
		}
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("avairy-node: shutting down")
			return
		case <-watch:
			syncUp() // local change → propagate now
		case <-ticker.C:
			if err := n.Heartbeat(); err != nil {
				fmt.Fprintln(os.Stderr, "heartbeat:", err)
			}
			syncUp()
			if *ws != "" {
				if err := n.SyncDown(*ws); err != nil {
					fmt.Fprintln(os.Stderr, "syncDown:", err)
				}
			}
		}
	}
}

const defaultRole = "You are an avairy agent. Collaborate ONLY through the avairy MCP tools " +
	"(send_message, read_inbox, post_task, claim_task, list_tasks, report_status, git_history, request_commit, scratch_worktree, resolve_conflict). Be terse."

// refreshMirror pulls a fresh repo bundle from core and (re)builds the node's read-only mirror.
// Best-effort: a missing repo on core or a transient error just leaves the mirror as-is.
func refreshMirror(ctx context.Context, n *control.Node, mirrorDir string) {
	have, _ := git.MirrorRefs(ctx, mirrorDir) // what we already have → incremental bundle
	b, err := n.PullBundle(ctx, have)
	if err != nil {
		return // core may have no repo, or be briefly unreachable; try again next tick
	}
	if len(b) == 0 {
		return // already current (nothing new)
	}
	if err := git.UpdateMirror(ctx, mirrorDir, b); err != nil {
		fmt.Fprintln(os.Stderr, "avairy-node: mirror update:", err)
	}
}

// mirrorRole describes, for the agent's system prompt, how to use the local read-only mirror
// for isolated bisect/build/repro without touching the synced workspace.
func mirrorRole(ws, mirrorDir string) string {
	scratch := filepath.Join(ws, ".avairy", "scratch")
	return " For root-cause analysis you have a READ-ONLY git mirror of the repo at " + mirrorDir +
		". To build/bisect a past commit on this machine, make a throwaway checkout: " +
		"`git --git-dir=" + mirrorDir + " worktree add " + scratch + "/<name> <ref>`, build/test there, " +
		"then `git --git-dir=" + mirrorDir + " worktree remove " + scratch + "/<name>`. Keep scratch checkouts under " +
		scratch + " (NOT the synced workspace), and commit via request_commit — you cannot push the mirror."
}

// spawnAgent starts an agent on this node wired to the local MCP proxy, ships its events to
// the core journal, and injects inbound bus messages (pulled from core) into its session.
func spawnAgent(ctx context.Context, n *control.Node, family, agentID, role, model, ws, proxy, mirrorDir string) error {
	if role == "" {
		role = defaultRole
	}
	if mirrorDir != "" {
		role += mirrorRole(ws, mirrorDir)
	}
	_, pport, err := net.SplitHostPort(proxy)
	if err != nil {
		return err
	}
	proxyURL := "http://127.0.0.1:" + pport + "/mcp"
	gateURL := "http://127.0.0.1:" + pport + "/gate"

	ad, err := buildAdapter(family, gateURL, gateDecider(n, agentID))
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
				r.ToolInput = agent.TrimInput(ev.Tool.Input) // ship the args so core sees what the agent did
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
					if m.Interrupt {
						_ = sess.Interrupt(ctx)
						continue
					}
					_ = sess.Send(ctx, m.Body, agent.DeliverySteer)
				}
			}
		}
	}()
	return nil
}

func buildAdapter(family, gateURL string, dec gating.Decider) (agent.Adapter, error) {
	switch family {
	case "claude":
		ca := claudecode.New()
		// The agent runs headless (-p, stream-json) with no interactive prompt, so the
		// PreToolUse hook must decide every tool call: it returns allow for free actions
		// (no prompt) and deny for gated ones (DESIGN.md §7). With the hook governing all
		// tools we don't bypass permissions — the hook *is* the permission system.
		settings, err := gating.ClaudeHookSettings(gateURL)
		if err != nil {
			return nil, err
		}
		ca.ExtraArgs = []string{"--settings", settings}
		return ca, nil
	case "codex":
		cx := codex.New()
		cx.Approve = codex.ApproverFromDecider(dec)
		return cx, nil
	case "copilot":
		return copilot.New(dec), nil
	case "grok":
		return grok.New(dec), nil
	default:
		return nil, fmt.Errorf("unknown family %q (want claude|codex|copilot|grok)", family)
	}
}

// gateDecider is the node's §7 enforcement decision. Free actions pass; gated actions
// (destructive commands, git mutations, installs) are routed to the human operator at core,
// which blocks until the operator allows/denies (or it times out → deny). If core is
// unreachable it fails closed. The verdict is logged so node-side activity is visible.
func gateDecider(n *control.Node, agentID string) gating.Decider {
	policy := gating.Policy{Approve: func(ctx context.Context, req gating.Request) (gating.Decision, error) {
		dec, err := n.RequestApproval(ctx, control.ApprovalRequest{
			AgentID: agentID, Kind: string(req.Kind), Summary: req.Summary, Reason: req.Reason,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "GATE ask-core failed, denying [%s] %s: %v\n", req.Kind, req.Summary, err)
			return gating.Deny, nil
		}
		if dec == control.DecisionAllow || dec == control.DecisionAllowForSession {
			return gating.Allow, nil
		}
		return gating.Deny, nil
	}}
	return func(ctx context.Context, req gating.Request) (gating.Decision, error) {
		d, err := policy.Decide(ctx, req)
		if err != nil || d == gating.Deny {
			fmt.Fprintf(os.Stderr, "GATE deny [%s] %s\n", req.Kind, req.Summary)
		}
		return d, err
	}
}
