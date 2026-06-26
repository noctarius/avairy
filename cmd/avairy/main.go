// Command avairy runs the collaboration loop. By default it starts no local agents — bring
// them via avairy-node, or:
//
//	go run ./cmd/avairy -demo           # TUI with mock agents alice+bob (zero credits)
//	go run ./cmd/avairy -live           # alice is a real Claude Code agent on the MCP bus
//	go run ./cmd/avairy -live -family grok
//	go run ./cmd/avairy -live -headless "create a task titled ping"
//	                                    # one real turn, print the journal, exit (for verification)
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"avairy/internal/adapter/claudecode"
	"avairy/internal/adapter/codex"
	"avairy/internal/adapter/copilot"
	"avairy/internal/adapter/grok"
	"avairy/internal/adapter/mock"
	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/control"
	"avairy/internal/facilitator"
	"avairy/internal/gating"
	"avairy/internal/git"
	"avairy/internal/journal"
	"avairy/internal/mcp"
	"avairy/internal/runner"
	"avairy/internal/tui"
	"avairy/internal/workspace"
)

func main() {
	// `avairy hook -gate <url>` is the PreToolUse hook shim a locally-spawned Claude invokes
	// per tool call; it must run before flag parsing (its args are its own).
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		gating.RunHookShim(os.Args[2:])
		return
	}
	// `avairy mint-join -id <node> -core <https-url>` issues an mTLS client-cert join (no token)
	// from the self-managed CA under .avairy, for a node to authenticate by certificate.
	if len(os.Args) > 1 && os.Args[1] == "mint-join" {
		mintJoin(os.Args[2:])
		return
	}

	demo := flag.Bool("demo", false, "spawn mock agents (alice, bob) for trying the loop / tests — off by default")
	live := flag.Bool("live", false, "run 'alice' as a real agent on the MCP bus")
	family := flag.String("family", "claude", "live agent family: claude | codex | copilot | grok")
	headless := flag.String("headless", "", "send this message to alice, print the journal, and exit (no TUI)")
	model := flag.String("model", "haiku", "model for the live agent (kept cheap by default; ignored for codex unless set)")
	controlAddr := flag.String("control-addr", "", "if set, serve the node control API here (enrollment/sync) and print an enroll token")
	mcpAddr := flag.String("mcp-addr", "127.0.0.1:0", "MCP bus listen address (use 0.0.0.0:PORT to allow remote nodes)")
	advertise := flag.String("advertise", "", "host/IP remote nodes use to reach this core (defaults to the listen host)")
	workspaceDir := flag.String("workspace", "", "operator project dir to seed/sync into the canonical hub (with -control-addr)")
	tlsCert := flag.String("tls-cert", "", "PEM cert file: serve the node control channel over TLS (recommended for remote nodes)")
	tlsKey := flag.String("tls-key", "", "PEM private key file for -tls-cert")
	tlsAuto := flag.Bool("tls-auto", false, "self-manage a CA under .avairy and serve the control channel over TLS (the CA travels to nodes in the join bundle; enables mTLS)")
	flag.Parse()

	// Durable, append-only journal (DESIGN.md §10) under .avairy/; falls back to memory-only.
	// On restart, replay the persisted log so both the board and the TUI history resume.
	journalPath := filepath.Join(".avairy", "journal.jsonl")
	persisted, _ := journal.ReadFile(journalPath) // prior history (nil on first run)
	var jrnl journal.Log = journal.NewMemory()
	if jf, err := journal.OpenFile(journalPath); err == nil {
		jf.Memory.Restore(decodeRecords(persisted)) // seed in-memory history for the TUI's backfill
		jrnl = jf
		defer jf.Close()
	}
	b := bus.New(jrnl)
	bd := board.New(jrnl)
	bd.Restore(persisted) // rebuild the task board from the same history
	mcpSrv := mcp.NewServer(b, bd, jrnl)

	// Human-in-the-loop gating broker (DESIGN.md §7): gated actions from any agent (local or
	// via a node) block here until the operator allows/denies them in the TUI. Journaling the
	// lifecycle both audits it and wakes the TUI to refresh its approvals view. The wait is
	// bounded (just under the node hook's 300s timeout) and fails closed.
	approvals := control.NewApprovals(280 * time.Second)
	approvals.OnRequest = func(a control.Approval) {
		jrnl.Append(journal.KindSystem, a.AgentID, map[string]any{"event": "approval_requested", "id": a.ID, "kind": a.Kind, "summary": a.Summary})
	}
	approvals.OnResolve = func(a control.Approval, decision string) {
		jrnl.Append(journal.KindSystem, a.AgentID, map[string]any{"event": "approval_resolved", "id": a.ID, "kind": a.Kind, "summary": a.Summary, "decision": decision})
	}

	// Serve the MCP bus on a loopback port; agents connect here (the daemon will tunnel
	// this for remote nodes — DESIGN.md §4).
	ln, err := net.Listen("tcp", *mcpAddr)
	if err != nil {
		fail("listen", err)
	}
	go http.Serve(ln, mcpSrv.HTTPHandler())
	// Local agents (alice/bob on this machine) always reach the bus via loopback, regardless
	// of the bind/advertise host used for remote nodes.
	_, mcpPort, _ := net.SplitHostPort(ln.Addr().String())
	busURL := "http://127.0.0.1:" + mcpPort + mcp.EndpointPath

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Optionally serve the node control API so remote avairy-node daemons can enroll and sync.
	var ctrlInfo *tui.ControlInfo
	var commitFn func(string) (string, error)                    // operator-initiated /commit; nil unless git is enabled
	var bundleFn func(context.Context, []string) ([]byte, error) // repo bundle for node mirrors; nil unless git is enabled
	if *controlAddr != "" {
		// Restore the canonical hub from disk so a core restart doesn't lose state (DESIGN.md
		// §9); persist it periodically and on clean shutdown.
		hubPath := filepath.Join(".avairy", "hub.json")
		hub, err := workspace.LoadHub(hubPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "avairy: load hub (starting empty):", err)
			hub = workspace.NewHub()
		}
		defer func() {
			if err := hub.Save(hubPath); err != nil {
				fmt.Fprintln(os.Stderr, "avairy: persist hub:", err)
			}
		}()
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if _, err := hub.SaveIfDirty(hubPath); err != nil {
						fmt.Fprintln(os.Stderr, "avairy: persist hub:", err)
					}
				}
			}
		}()
		// Seed the canonical hub from the operator's project dir and keep it synced both ways,
		// so remote nodes receive a working copy on their first SyncDown.
		if *workspaceDir != "" {
			seed := workspace.NewNodeView("core")
			seed.ResumeFromHub(hub, *workspaceDir) // adopt restored versions; don't re-conflict/delete
			if _, err := seed.SyncUp(hub, *workspaceDir, workspace.IgnoreFor(*workspaceDir)); err != nil {
				fmt.Fprintln(os.Stderr, "avairy: seed workspace:", err)
			}
			var seedWatch <-chan struct{}
			if ch, werr := workspace.Watch(ctx, *workspaceDir, workspace.IgnoreFor(*workspaceDir)); werr != nil {
				fmt.Fprintln(os.Stderr, "avairy: watch (falling back to poll):", werr)
			} else {
				seedWatch = ch
			}
			go func() {
				t := time.NewTicker(2 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-seedWatch: // operator edited the project → push now
						_, _ = seed.SyncUp(hub, *workspaceDir, workspace.IgnoreFor(*workspaceDir))
					case <-t.C: // fallback poll + pull remote changes (no server→node push)
						_, _ = seed.SyncUp(hub, *workspaceDir, workspace.IgnoreFor(*workspaceDir))
						_ = seed.SyncDown(hub, *workspaceDir)
					}
				}
			}()
			// If the operator's workspace is a git repo, expose history reads + gated signed
			// commits over the bus (DESIGN.md §9: the canonical repo lives only on core).
			if repo, gerr := git.Open(ctx, *workspaceDir, true); gerr != nil {
				fmt.Fprintln(os.Stderr, "avairy: git tools disabled:", gerr)
			} else {
				mcpSrv.EnableGit(repo, gitApprover(approvals))
				defer repo.PruneWorktrees(context.Background()) // disposable: clean up scratch checkouts on exit
				commitFn = func(message string) (string, error) {
					hash, cerr := repo.Commit(context.Background(), nil, message)
					if cerr == nil {
						jrnl.Append(journal.KindSystem, "human", map[string]any{"event": "git_commit", "hash": hash, "message": message})
					}
					return hash, cerr
				}
				bundleFn = repo.Bundle
			}
		}
		// Conflict reconciliation (DESIGN.md §9): agents resolve divergent edits via the bus +
		// resolve_conflict tool. Only meaningful with a hub (always present here).
		mcpSrv.EnableConflicts(func(agentID, path string, content []byte) (uint64, error) {
			return hub.Resolve(agentID, path, content).Version, nil
		})

		core := control.NewCore(hub, jrnl)
		core.Approvals = approvals // one broker feeds both node agents and the operator TUI
		core.Bundle = bundleFn     // serve the repo as a bundle for node mirrors (nil if no repo)
		core.OnConflict = func(agentID, path string, hubVersion uint64, hubContent, yourContent []byte) {
			body := fmt.Sprintf("CONFLICT on %s — another agent changed it (now hub v%d). Merge the two versions below and call resolve_conflict(path=%q, content=<merged>). Your local copy was overwritten with the hub version, so use YOUR EDIT from here.\n--- hub v%d ---\n%s\n--- YOUR EDIT ---\n%s",
				path, hubVersion, path, hubVersion, truncateForBus(hubContent), truncateForBus(yourContent))
			b.Publish("avairy", bus.Agent(agentID), body, agent.DeliverySteer)
		}
		// When a node enrolls, register its agent on the bus (identity, caps, inbox); deliver
		// that agent's inbound bus messages back over the control channel.
		core.OnEnroll = func(nodeID string, caps map[string]string) {
			mcpSrv.RegisterAgent(nodeID, []string{"backend"}, caps) // node id == agent's bus identity
		}
		core.InboxDrainer = func(agentID string) []control.InboxMessage {
			var out []control.InboxMessage
			for _, m := range mcpSrv.DrainInbox(agentID) {
				out = append(out, control.InboxMessage{ID: m.ID, From: m.From, Body: m.Body, Delivery: string(m.Delivery), Interrupt: m.Interrupt})
			}
			return out
		}
		ctrlScheme := "http"
		serve := func() error {
			return http.ListenAndServe(*controlAddr, core.Handler())
		}
		var caPEM []byte
		ctrlHost := hostOf(advertised(*advertise, *controlAddr))
		switch {
		case *tlsAuto:
			ca, cerr := control.EnsureCA(".avairy")
			if cerr != nil {
				fail("ca", cerr)
			}
			cert, cerr := ca.ServerTLS([]string{ctrlHost, "127.0.0.1", "localhost", "::1"})
			if cerr != nil {
				fail("server cert", cerr)
			}
			caPEM = ca.CertPEM()
			ctrlScheme = "https"
			srv := &http.Server{
				Addr:    *controlAddr,
				Handler: core.Handler(),
				TLSConfig: &tls.Config{
					Certificates: []tls.Certificate{cert},
					ClientAuth:   tls.VerifyClientCertIfGiven, // token OR client-cert auth
					ClientCAs:    ca.Pool(),
				},
			}
			serve = func() error {
				return srv.ListenAndServeTLS("", "")
			}
		case *tlsCert != "" && *tlsKey != "":
			ctrlScheme = "https"
			serve = func() error {
				return http.ListenAndServeTLS(*controlAddr, *tlsCert, *tlsKey, core.Handler())
			}
		}
		go func() {
			if err := serve(); err != nil {
				fmt.Fprintln(os.Stderr, "control server:", err)
			}
		}()
		go core.RunLiveness(ctx) // mark nodes offline when heartbeats lapse

		ctrlURL := ctrlScheme + "://" + advertised(*advertise, *controlAddr)
		busBase := "http://" + advertised(*advertise, ln.Addr().String())
		warn := ""
		if unreachableHost(hostOf(advertised(*advertise, ln.Addr().String()))) {
			warn = "host not reachable from other machines — pass -advertise <ip/host> and bind -control-addr/-mcp-addr to 0.0.0.0:PORT"
		}
		// Write a one-string join bundle (core URL + CA + token) for the next node, refreshed
		// whenever the token is read or rotated, so "the pubcert travels with the token".
		joinPath := filepath.Join(".avairy", "join")
		writeJoin := func(tok string) {
			jb := control.EncodeJoin(control.JoinBundle{Core: ctrlURL, CA: caPEM, Token: tok})
			_ = os.WriteFile(joinPath, []byte(jb), 0o600)
		}
		curToken := func() string {
			t := core.CurrentToken()
			writeJoin(t)
			return t
		}
		newToken := func() string {
			t := core.NewPendingToken()
			writeJoin(t)
			return t
		}
		ctrlInfo = &tui.ControlInfo{ControlURL: ctrlURL, BusBase: busBase, Warn: warn, CurrentToken: curToken, NewToken: newToken, JoinFile: joinPath}
		// Under the TUI's alt-screen, stdout is hidden — so the token/join is shown in the TUI.
		// Only print here when there's no TUI (headless).
		if *headless != "" {
			fmt.Printf("control API:  %s\nMCP bus base: %s\nenroll token: %s\njoin file:    %s\n", ctrlURL, busBase, curToken(), joinPath)
			if warn != "" {
				fmt.Println("warning:", warn)
			}
		}
	}

	// Facilitator: watch the journal for stuck signals and nudge (DESIGN.md §5).
	fac := facilitator.New(b, facilitator.RosterFunc(func() []facilitator.Agent {
		metas := mcpSrv.AgentList()
		out := make([]facilitator.Agent, 0, len(metas))
		for _, m := range metas {
			out = append(out, facilitator.Agent{ID: m.ID, Caps: m.Caps})
		}
		return out
	}), facilitator.RuleNudger{})
	facSub, _ := jrnl.Subscribe()
	go fac.Run(ctx, facSub)

	caps := map[string]string{"os": runtime.GOOS}

	// Local agents are opt-in: none by default (bring agents via avairy-node). -live runs one
	// real 'alice'; -demo spawns mock alice+bob for the playground/tests; -headless needs an
	// 'alice' to talk to, so default it to a mock when neither -live nor -demo is set.
	runLiveAlice := *live
	runMockAlice := *demo && !*live
	runMockBob := *demo
	if *headless != "" && !runLiveAlice && !runMockAlice {
		runMockAlice = true
	}

	if runLiveAlice {
		mcpSrv.RegisterAgent("alice", []string{"backend"}, caps)
		startLiveAlice(ctx, *family, *model, busURL, b, jrnl, approvals)
	}
	if runMockAlice {
		mcpSrv.RegisterAgent("alice", []string{"backend"}, caps)
		startMock(ctx, "alice", b, jrnl)
	}
	if runMockBob {
		mcpSrv.RegisterAgent("bob", []string{"backend"}, caps)
		startMock(ctx, "bob", b, jrnl)
	}

	if *headless != "" {
		runHeadless(b, jrnl, *headless)
		return
	}
	roster := func() []string {
		metas := mcpSrv.AgentList()
		ids := make([]string, 0, len(metas))
		for _, mm := range metas {
			ids = append(ids, mm.ID)
		}
		return ids
	}
	deps := tui.Deps{
		Bus: b, Board: bd, Journal: jrnl, Control: ctrlInfo, Roster: roster,
		PendingApprovals: func() []tui.ApprovalItem {
			ps := approvals.Pending()
			out := make([]tui.ApprovalItem, 0, len(ps))
			for _, p := range ps {
				out = append(out, tui.ApprovalItem{ID: p.ID, AgentID: p.AgentID, Kind: p.Kind, Summary: p.Summary, Reason: p.Reason})
			}
			return out
		},
		ResolveApproval: func(id, decision string) { approvals.Resolve(id, decision) },
		Commit:          commitFn,
	}
	if err := tui.Run(deps); err != nil {
		fail("tui", err)
	}
}

const aliceRole = "You are 'alice', a backend engineer agent in the avairy multi-agent system. " +
	"Collaborate ONLY through the avairy MCP tools: post_task, claim_task, list_tasks, " +
	"send_message, read_inbox, report_status, git_history, request_commit, scratch_worktree, resolve_conflict. Be terse and do exactly what you are asked, then stop."

func startLiveAlice(ctx context.Context, family, model, busURL string, b *bus.Bus, jrnl journal.Log, approvals *control.Approvals) {
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
		cx := codex.New()
		// Real gating with a human in the loop: app-server approvals route through the §7
		// policy; gated actions block on the operator's allow/deny in the TUI approvals view.
		cx.Approve = codex.ApproverFromDecider(localGateDecider(approvals, "alice"))
		ad = cx
	case "copilot":
		ad = copilot.New(localGateDecider(approvals, "alice")) // ACP; needs `copilot login`
	case "grok":
		ad = grok.New(localGateDecider(approvals, "alice")) // ACP; needs xAI auth
	default: // claude
		ca := claudecode.New()
		// Real gating, same as a node: serve a local /gate and register a PreToolUse hook for
		// every tool call (free actions allowed without a prompt, gated ones routed to the
		// operator's Approvals tab). The hook is the permission system — no --allowedTools.
		gateURL, err := startLocalGate(localGateDecider(approvals, "alice"))
		if err != nil {
			fail("local gate", err)
		}
		settings, err := gating.ClaudeHookSettings(gateURL)
		if err != nil {
			fail("hook settings", err)
		}
		ca.ExtraArgs = []string{"--settings", settings}
		ad = ca
	}

	sess, err := ad.Start(ctx, cfg)
	if err != nil {
		fail("start alice", err)
	}
	go runner.New(runner.Agent{ID: "alice", Roles: []string{"backend"}}, sess, b, jrnl).Run(ctx)
}

// localGateDecider gates a local agent's actions via the §7 policy, routing gated ones to the
// operator (TUI approvals) through the shared broker — the in-process twin of the node's
// gateDecider. Free actions pass; gated actions block until allowed/denied (timeout → deny).
func localGateDecider(approvals *control.Approvals, agentID string) gating.Decider {
	policy := gating.Policy{Approve: func(ctx context.Context, req gating.Request) (gating.Decision, error) {
		dec := approvals.Ask(ctx, control.Approval{
			AgentID: agentID, Kind: string(req.Kind), Summary: req.Summary, Reason: req.Reason,
		})
		if dec == control.DecisionAllow || dec == control.DecisionAllowForSession {
			return gating.Allow, nil
		}
		return gating.Deny, nil
	}}
	return policy.Decide
}

// startLocalGate serves the PreToolUse gate endpoint on a loopback port for a locally-spawned
// Claude and returns its URL. Tool calls POSTed here are ruled on by decide (free → allow,
// gated → the operator's Approvals tab). The listener lives for the process lifetime.
func startLocalGate(decide gating.Decider) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	mux.Handle("/gate", gating.HookHandler(decide))
	go http.Serve(ln, mux)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return "http://127.0.0.1:" + port + "/gate", nil
}

// gitApprover routes a request_commit (a §7 git mutation) to the operator via the shared
// approvals broker, keyed by the requesting agent — so commits surface in the Approvals tab.
func gitApprover(approvals *control.Approvals) gating.Decider {
	return func(ctx context.Context, req gating.Request) (gating.Decision, error) {
		dec := approvals.Ask(ctx, control.Approval{AgentID: req.AgentID, Kind: string(req.Kind), Summary: req.Summary})
		if dec == control.DecisionAllow || dec == control.DecisionAllowForSession {
			return gating.Allow, nil
		}
		return gating.Deny, nil
	}
}

// truncateForBus bounds hub content embedded in a conflict notification (huge/binary files
// would bloat the message); the agent has its own side locally and can read more if needed.
func truncateForBus(b []byte) string {
	const max = 4000
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "\n… (truncated)"
}

// mintJoin issues an mTLS client-cert join bundle from the self-managed CA (no enrollment
// token): the node it's given to authenticates by certificate. Prints the join string.
func mintJoin(argv []string) {
	fs := flag.NewFlagSet("mint-join", flag.ExitOnError)
	id := fs.String("id", "", "node id (becomes the client cert CN) — required")
	coreURL := fs.String("core", "", "control API URL the node will dial (https://…) — required")
	dir := fs.String("dir", ".avairy", "directory holding the CA (ca.crt/ca.key)")
	_ = fs.Parse(argv)
	if *id == "" || *coreURL == "" {
		fmt.Fprintln(os.Stderr, "mint-join: -id and -core are required")
		os.Exit(2)
	}
	ca, err := control.EnsureCA(*dir)
	if err != nil {
		fail("mint-join: ca", err)
	}
	cert, key, err := ca.ClientTLS(*id)
	if err != nil {
		fail("mint-join: client cert", err)
	}
	fmt.Println(control.EncodeJoin(control.JoinBundle{
		Core: *coreURL, CA: ca.CertPEM(), NodeID: *id, ClientCert: cert, ClientKey: key,
	}))
}

// decodeRecords turns persisted journal records back into typed in-memory records, so the TUI
// can replay history after a restart. The journal package can't type these itself (it can't
// import its consumers), so we do it here. Seqs are renumbered contiguously for stable de-dup.
func decodeRecords(prs []journal.PersistedRecord) []journal.Record {
	out := make([]journal.Record, 0, len(prs))
	for _, pr := range prs {
		var data any
		switch pr.Kind {
		case journal.KindMessage:
			var m bus.Message
			if json.Unmarshal(pr.Data, &m) == nil {
				data = m
			}
		case journal.KindAgentEvent:
			var e agent.Event
			if json.Unmarshal(pr.Data, &e) == nil {
				data = e
			}
		case journal.KindTask, journal.KindHandover:
			var tk board.Task
			if json.Unmarshal(pr.Data, &tk) == nil {
				data = tk
			}
		default: // system / approval — map payloads
			var mm map[string]any
			if json.Unmarshal(pr.Data, &mm) == nil {
				data = mm
			}
		}
		if data == nil {
			continue
		}
		out = append(out, journal.Record{Seq: uint64(len(out) + 1), Time: pr.Time, Kind: pr.Kind, Actor: pr.Actor, Data: data})
	}
	return out
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

// advertised returns the host:port remote nodes should dial: the -advertise host (if given)
// combined with the bound port, else the bound address as-is.
func advertised(adv, bound string) string {
	_, port, _ := net.SplitHostPort(bound)
	if adv == "" {
		return bound
	}
	if _, _, err := net.SplitHostPort(adv); err == nil {
		return adv // already host:port
	}
	return net.JoinHostPort(adv, port)
}

func hostOf(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// unreachableHost reports whether a host can't be dialed from another machine.
func unreachableHost(h string) bool {
	if h == "" || h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}
