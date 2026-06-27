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
	"strings"
	"syscall"
	"time"

	"avairy/internal/adapter/claudecode"
	"avairy/internal/adapter/codex"
	"avairy/internal/adapter/copilot"
	"avairy/internal/adapter/grok"
	"avairy/internal/agent"
	"avairy/internal/bus"
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
	gateEdits := flag.Bool("gate-edits", false, "also require operator approval for file edits (per-edit gating; allow-for-session avoids per-diff prompts)")
	caFile := flag.String("ca", "", "PEM cert/CA to trust for an https core (self-signed/internal CA)")
	insecure := flag.Bool("insecure", false, "skip TLS verification for an https core (DEV ONLY — exposes the channel to MITM)")
	join := flag.String("join", "", "join string from core (carries core URL + CA + token or client cert); supplies -core/-token/-ca")
	joinFile := flag.String("join-file", "", "file containing a join string (e.g. core's .avairy/join)")
	flag.Parse()

	// A join bundle supplies core URL + how to trust/authenticate in one string, overriding the
	// individual flags (DESIGN.md §4). It carries either an enrollment token or an mTLS client cert.
	var clientCertPEM, clientKeyPEM, joinCA []byte
	if *join != "" || *joinFile != "" {
		raw := *join
		if raw == "" {
			b, err := os.ReadFile(*joinFile)
			if err != nil {
				fmt.Fprintln(os.Stderr, "avairy-node: join-file:", err)
				os.Exit(1)
			}
			raw = strings.TrimSpace(string(b))
		}
		jb, err := control.DecodeJoin(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node:", err)
			os.Exit(1)
		}
		*core = jb.Core
		joinCA = jb.CA
		if jb.Bus != "" && *coreMCP == "" {
			*coreMCP = jb.Bus // so -family works from a join alone (needs the MCP bus base)
		}
		if jb.Token != "" {
			*token = jb.Token
		}
		if jb.NodeID != "" {
			*id = jb.NodeID // mTLS: id must match the client cert's SAN
		}
		clientCertPEM, clientKeyPEM = jb.ClientCert, jb.ClientKey
	}

	mtls := len(clientCertPEM) > 0
	if *core == "" || *id == "" || (*token == "" && !mtls) {
		fmt.Fprintln(os.Stderr, "avairy-node: need -core and -id, plus -token (or a join with a client cert)")
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
	// TLS trust + (optional) mTLS client identity. A join's CA/client-cert take precedence; else
	// -ca / -insecure. With a publicly-trusted cert and no client cert, none of this is needed.
	switch {
	case len(joinCA) > 0 || mtls:
		client, err := control.TLSClientPEM(joinCA, *insecure, clientCertPEM, clientKeyPEM)
		if err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node: tls:", err)
			os.Exit(1)
		}
		n.HTTP = client
		// mTLS auth is stateless on core, so the node can transparently re-enroll if core
		// restarts and forgets its session (a token node couldn't — its binding is gone).
		n.ReenrollOnExpiry = mtls
	case *caFile != "" || *insecure:
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

	// Adopt the hub's versions for files already present locally before the first sync, so a
	// pre-populated workspace (or a restart — base is in-memory) doesn't push everything from base 0
	// and report a spurious conflict on every file.
	if *ws != "" {
		if err := n.ResumeFromHub(*ws); err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node: resume from hub:", err)
		}
	}

	// Local HTTP server for agents on this machine: the MCP proxy → core bus (stamping this
	// node's identity == agent id) plus the /gate endpoint the Claude PreToolUse hook calls.
	if *coreMCP != "" {
		h, err := n.MCPProxy(*coreMCP, *id)
		if err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node: proxy:", err)
			os.Exit(1)
		}
		mux := http.NewServeMux()
		mux.Handle("/gate", gating.HookHandler(gateDecider(n, *id, *gateEdits)))
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
		if err := spawnAgent(ctx, n, *family, *id, *role, *model, *ws, *proxy, mirrorDir, agent.SessionPersistent, *gateEdits); err != nil {
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

	nodeConsults := &nodeConsultMgr{
		n: n, coreMCP: *coreMCP, family: *family, model: *model, gateEdits: *gateEdits,
		cancel: map[string]context.CancelFunc{},
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
			// The operator's verdict on a held startup conflict (#21) rides back on the heartbeat.
			if d := n.TakeDirective(); d != "" && *ws != "" {
				fmt.Printf("operator chose %q for held startup conflicts\n", d)
				if err := n.ApplyDirective(*ws, d); err != nil {
					fmt.Fprintln(os.Stderr, "apply directive:", err)
				}
			}
			// Conflicts the agent resolved via resolve_conflict (#22): unlock + pull canonical BEFORE
			// syncUp, so the marker scan doesn't re-lock the still-markered file first.
			if u := n.TakeUnlocks(); len(u) > 0 && *ws != "" {
				if err := n.ApplyUnlocks(*ws, u); err != nil {
					fmt.Fprintln(os.Stderr, "apply unlocks:", err)
				}
			}
			// Operator-spawned ephemeral consults targeted at this node (#24).
			for _, cmd := range n.TakeConsults() {
				nodeConsults.apply(ctx, cmd)
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
	"(send_message, read_inbox, list_agents, post_task, claim_task, list_tasks, report_status, git_history, request_commit, scratch_worktree, list_conflicts, resolve_conflict, fresh_look, note, read_notes). Be terse."

// readSession reads a persisted agent session id (empty if absent/unreadable).
func readSession(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeSession persists the agent's session id (best-effort) so a respawn can resume it.
func writeSession(path, id string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(id), 0o600)
}

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

// nodeConsultRole is the persona for an operator-spawned ephemeral consult agent running ON a node
// (#24) — same disposable contract as the core consult, but it answers from THIS machine's OS.
const nodeConsultRole = "You are an ephemeral CONSULT agent in avairy, running on this node — so " +
	"you answer from THIS machine's actual OS and filesystem (e.g. validating whether a path exists " +
	"or is valid here). Be concise and direct. You have the avairy MCP tools — use send_message to " +
	"ask other agents and read_inbox for replies. CRITICAL: this session is disposable and NOT saved " +
	"— when it closes, everything here is gone. Anything worth keeping you MUST write to the shared " +
	"blackboard with note(key, text) or open a task with post_task."

// nodeConsultMgr spawns/tears down ephemeral consult agents on this node, on command from core
// (#24). Each runs on its own local proxy (stamping its bus id) and is torn down on "close".
type nodeConsultMgr struct {
	n         *control.Node
	coreMCP   string
	family    string
	model     string
	gateEdits bool
	cancel    map[string]context.CancelFunc // id -> cancel (loop-goroutine only; no mutex needed)
}

func (m *nodeConsultMgr) apply(parent context.Context, cmd control.ConsultCommand) {
	switch cmd.Action {
	case "open":
		if m.coreMCP == "" {
			fmt.Fprintln(os.Stderr, "consult: cannot spawn without -core-mcp")
			return
		}
		fam := cmd.Family
		if fam == "" {
			fam = m.family
		}
		cctx, cancel := context.WithCancel(parent)
		proxyAddr, err := startConsultProxy(cctx, m.n, m.coreMCP, cmd.ID, m.gateEdits)
		if err != nil {
			cancel()
			fmt.Fprintln(os.Stderr, "consult proxy:", err)
			return
		}
		ws, err := os.MkdirTemp("", "avairy-"+cmd.ID+"-")
		if err != nil {
			cancel()
			fmt.Fprintln(os.Stderr, "consult workspace:", err)
			return
		}
		if err := spawnAgent(cctx, m.n, fam, cmd.ID, nodeConsultRole, m.model, ws, proxyAddr, "", agent.SessionEphemeral, m.gateEdits); err != nil {
			cancel()
			fmt.Fprintln(os.Stderr, "consult spawn:", err)
			return
		}
		m.cancel[cmd.ID] = cancel
		fmt.Printf("opened ephemeral consult %q on this node\n", cmd.ID)
	case "close":
		if c := m.cancel[cmd.ID]; c != nil {
			c()
			delete(m.cancel, cmd.ID)
			fmt.Printf("closed consult %q\n", cmd.ID)
		}
	}
}

// startConsultProxy serves a fresh local MCP proxy (stamping id) + gate on an ephemeral port for a
// consult agent, torn down when ctx cancels. Returns the proxy's listen address.
func startConsultProxy(ctx context.Context, n *control.Node, coreMCP, id string, gateEdits bool) (string, error) {
	h, err := n.MCPProxy(coreMCP, id)
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	mux.Handle("/gate", gating.HookHandler(gateDecider(n, id, gateEdits)))
	mux.Handle("/", h)
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	go func() { <-ctx.Done(); _ = srv.Close() }()
	return ln.Addr().String(), nil
}

// spawnAgent starts an agent on this node wired to the local MCP proxy, ships its events to
// the core journal, and injects inbound bus messages (pulled from core) into its session.
func spawnAgent(ctx context.Context, n *control.Node, family, agentID, role, model, ws, proxy, mirrorDir string, mode agent.SessionMode, gateEdits bool) error {
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

	ad, err := buildAdapter(family, gateURL, gateDecider(n, agentID, gateEdits))
	if err != nil {
		return err
	}
	cfg := agent.SessionConfig{
		AgentID:   agentID,
		Role:      role,
		Mode:      mode,
		Workspace: ws,
		Model:     model,
		MCP:       []agent.MCPServer{{Name: "avairy", Type: "http", URL: proxyURL}},
	}
	// Resume the agent's prior conversation across a node restart (DESIGN.md §8): the session id
	// is persisted under the sync-excluded .avairy dir and passed back as ResumeID. Only for
	// families that actually honor it (claude --resume, codex thread/resume).
	// Persist/resume only for a persistent session — never an ephemeral one (a one-shot fresh
	// look must not overwrite the agent's real session). The node only spawns persistent agents
	// today; the Mode guard keeps the invariant if that ever changes.
	var sessionFile string
	if ws != "" && ad.Capabilities().SupportsResume && cfg.Mode != agent.SessionEphemeral {
		sessionFile = filepath.Join(ws, ".avairy", "session")
		if prev := readSession(sessionFile); prev != "" {
			cfg.ResumeID = prev
			fmt.Printf("resuming %s session %s for agent %q\n", family, prev, agentID)
		}
	}
	sess, err := ad.Start(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("spawned %s agent %q → bus via %s\n", family, agentID, proxyURL)

	// Ship the agent's events to the core journal (so they appear in the operator TUI), and
	// persist the session id once the agent reports it so a respawn can resume.
	go func() {
		savedSession := ""
		for ev := range sess.Events() {
			if sessionFile != "" {
				if id := sess.ID(); id != "" && id != savedSession {
					savedSession = id
					writeSession(sessionFile, id)
				}
			}
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
	waker := bus.NewWaker()
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
					// Bus hardening (#25): only wake the agent for messages that should trigger a
					// turn (direct, or human/facilitator, within the autonomous-wake budget).
					if !waker.Wake(m.From, bus.ToKind(m.ToKind), false, time.Now()) {
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
func gateDecider(n *control.Node, agentID string, gateEdits bool) gating.Decider {
	policy := gating.Policy{GateEdits: gateEdits, Approve: func(ctx context.Context, req gating.Request) (gating.Decision, error) {
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
