// Command avairy runs the collaboration loop. By default it starts no local agents — bring
// them via avairy-node, or:
//
//	go run ./cmd/avairy -demo           # TUI with mock agents alice+bob (zero credits)
//	go run ./cmd/avairy -live           # alice is a real Claude Code agent on the MCP bus
//	go run ./cmd/avairy -live -family grok
//	go run ./cmd/avairy -control-addr :7700 -headless   # serve, no TUI (nodes enroll; ctrl+c to stop)
//	go run ./cmd/avairy -live -send "create a task titled ping"
//	                                    # one real turn, print the journal, exit (for verification)
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"avairy/internal/adapter"
	"avairy/internal/adapter/claudecode"
	"avairy/internal/adapter/mock"
	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/buildinfo"
	"avairy/internal/bus"
	"avairy/internal/control"
	"avairy/internal/cost"
	"avairy/internal/dispatch"
	"avairy/internal/facilitator"
	"avairy/internal/gating"
	"avairy/internal/git"
	"avairy/internal/journal"
	"avairy/internal/mcp"
	"avairy/internal/operator"
	"avairy/internal/runner"
	"avairy/internal/supervisor"
	"avairy/internal/tui"
	"avairy/internal/workspace"
)

func main() {
	app := &cli.Command{
		Name:    "avairy",
		Usage:   "orchestrate a fleet of AI coding agents across machines",
		Version: buildinfo.Version,
		Commands: []*cli.Command{
			coreCommand(),
			hookCommand(),
			{Name: "version", Usage: "print the build version", Action: func(context.Context, *cli.Command) error {
				fmt.Println(buildinfo.String())
				return nil
			}},
		},
	}
	if err := app.Run(context.Background(), os.Args); err != nil {
		fail("avairy", err)
	}
}

// ref returns a pointer to v, so the core wiring can keep reading flags as *flag while urfave
// hands us plain values.
func ref[T any](v T) *T { return &v }

// hookCommand is the PreToolUse shim a locally-spawned Claude invokes per tool call
// (avairy hook -gate <url>). It parses its own flags, so urfave passes the raw args through.
func hookCommand() *cli.Command {
	return &cli.Command{Name: "hook", Hidden: true, SkipFlagParsing: true, Action: func(_ context.Context, cmd *cli.Command) error {
		gating.RunHookShim(cmd.Args().Slice())
		return nil
	}}
}

// coreFlags are shared by `core run` and `core serve` — they differ only in attaching the TUI.
func coreFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{Name: "demo", Usage: "spawn mock agents (alice, bob) for trying the loop / tests"},
		&cli.BoolFlag{Name: "live", Usage: "run 'alice' as a real agent on the MCP bus"},
		&cli.StringFlag{Name: "family", Value: "claude", Usage: "live agent family: claude | codex | copilot | grok"},
		&cli.StringFlag{Name: "model", Value: "haiku", Usage: "model for the live agent (kept cheap by default)"},
		&cli.StringFlag{Name: "send", Usage: "one-shot (dev): send this to a local 'alice', print the journal, and exit"},
		&cli.StringFlag{Name: "control-addr", Usage: "serve the node control + operator API here (e.g. 0.0.0.0:7700)"},
		&cli.StringFlag{Name: "mcp-addr", Value: "127.0.0.1:7702", Usage: "MCP bus listen address (0.0.0.0:7702 to allow remote nodes)"},
		&cli.StringFlag{Name: "advertise", Usage: "host/IP remote nodes use to reach this core"},
		&cli.StringFlag{Name: "workspace", Usage: "operator project dir to seed/sync into the canonical hub"},
		&cli.StringFlag{Name: "tls-cert", Usage: "PEM cert file: serve the control channel over TLS"},
		&cli.StringFlag{Name: "tls-key", Usage: "PEM private key file for --tls-cert"},
		&cli.BoolFlag{Name: "tls-auto", Usage: "self-manage a CA under .avairy and serve control + bus over TLS (enables mTLS)"},
		&cli.BoolFlag{Name: "allow-token-join", Usage: "allow temporary token-based node enrollment; default off — nodes join by minted mTLS cert (see `core add-node`)"},
		&cli.BoolFlag{Name: "gate-edits", Usage: "also require operator approval for file edits (not just risky commands)"},
		&cli.StringFlag{Name: "operator-token", Usage: "bearer token for the remote operator API; default: random"},
		&cli.BoolFlag{Name: "web", Usage: "serve the browser operator console at /operator/ui"},
		&cli.Float64Flag{Name: "budget", Usage: "fleet spend cap in USD: cross it and every agent is interrupted (0 = uncapped)"},
		&cli.Float64Flag{Name: "agent-budget", Usage: "per-agent spend cap in USD (0 = uncapped)"},
		&cli.DurationFlag{Name: "idle-sleep", Usage: "park an idle core agent after this long quiet; the next directed message respawns it"},
	}
}

// coreCommand groups running core and managing who may join it.
func coreCommand() *cli.Command {
	return &cli.Command{
		Name:  "core",
		Usage: "run core, or invite nodes/operators to join it",
		Commands: []*cli.Command{
			{Name: "run", Usage: "run core with the operator TUI", Flags: coreFlags(),
				Action: func(ctx context.Context, cmd *cli.Command) error { return runCore(ctx, cmd, false) }},
			{Name: "serve", Usage: "run core headless (no TUI — attach a remote console)", Flags: coreFlags(),
				Action: func(ctx context.Context, cmd *cli.Command) error { return runCore(ctx, cmd, true) }},
			addNodeCommand(),
			addOperatorCommand(),
		},
	}
}

// runCore is the core's main loop (core run / core serve). headlessMode skips the in-process TUI.
// Flags are read into *flag locals so the wiring below is unchanged from the original main().
func runCore(_ context.Context, cmd *cli.Command, headlessMode bool) error {
	demo := ref(cmd.Bool("demo"))
	live := ref(cmd.Bool("live"))
	family := ref(cmd.String("family"))
	headless := ref(headlessMode)
	send := ref(cmd.String("send"))
	model := ref(cmd.String("model"))
	controlAddr := ref(cmd.String("control-addr"))
	mcpAddr := ref(cmd.String("mcp-addr"))
	advertise := ref(cmd.String("advertise"))
	workspaceDir := ref(cmd.String("workspace"))
	tlsCert := ref(cmd.String("tls-cert"))
	tlsKey := ref(cmd.String("tls-key"))
	tlsAuto := ref(cmd.Bool("tls-auto"))
	allowTokenJoin := ref(cmd.Bool("allow-token-join"))
	gateEdits := ref(cmd.Bool("gate-edits"))
	operatorToken := ref(cmd.String("operator-token"))
	web := ref(cmd.Bool("web"))
	budget := ref(cmd.Float64("budget"))
	agentBudget := ref(cmd.Float64("agent-budget"))
	idleSleep := ref(cmd.Duration("idle-sleep"))

	// Secure by default (b): token enrollment is OFF unless --allow-token-join. A node joins with a
	// minted mTLS client cert (`core add-node`); a token is a deliberate, temporary opt-in. So
	// mtlsEnabled ("cert-only, no token") is the default and only relaxes when tokens are allowed.
	mtlsEnabled := !*allowTokenJoin

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
	mcpSrv.Blackboard().Restore(persisted) // resume durable shared memory across restart (§4/§8)
	// fresh_look: any agent can request a clean-context second opinion (DESIGN.md §8).
	mcpSrv.EnableFreshLook(makeFreshLook(*family, *model, bd, mcpSrv.Blackboard()))

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

	// Owner-less conflicts (DESIGN.md §9, item #19): the operator's seed workspace diverging from a
	// node's edit (or a git conflict) has no agent to hand it to, so it surfaces in the TUI Conflicts
	// view. Journaling the lifecycle wakes the TUI to refresh.
	conflictBroker := control.NewConflicts()
	conflictBroker.OnRaise = func(oc control.OperatorConflict) {
		jrnl.Append(journal.KindSystem, "core", map[string]any{"event": "conflict_raised", "id": oc.ID, "path": oc.Path, "source": oc.Source})
	}
	conflictBroker.OnResolve = func(oc control.OperatorConflict, decision string) {
		jrnl.Append(journal.KindSystem, "core", map[string]any{"event": "conflict_resolved", "id": oc.ID, "path": oc.Path, "decision": decision})
	}

	roster := func() []string {
		metas := mcpSrv.AgentList()
		ids := make([]string, 0, len(metas))
		for _, mm := range metas {
			ids = append(ids, mm.ID)
		}
		return ids
	}
	// The single operator surface (#18): it yields both the in-process TUI deps (svc.Deps) and the
	// remote operator API (operator.NewServer below, mounted on the control listener). Commit /
	// Control / NewToken are func fields filled in once the control API + git are set up; they're
	// read at request time, so a remote client that connects later sees them.
	svc := &operator.Services{
		Journal: jrnl, Roster: roster, Tasks: bd.List,
		Notes:     func() []board.Note { return mcpSrv.Blackboard().Read("") }, // blackboard view (#27)
		Approvals: approvals, Conflicts: conflictBroker, Bus: b,
	}
	opToken := *operatorToken
	if opToken == "" {
		opToken = operator.RandomToken()
	}

	// Self-managed TLS material (DESIGN.md §4), shared by the MCP bus and the control channel, so
	// both encrypted listeners use one cert and the CA travels to nodes in the join bundle.
	ca, serverCert, caPEM := serverTLS(*tlsAuto, *advertise, *mcpAddr, *controlAddr)

	// HTTP servers' per-connection errors (e.g. a browser refusing the self-signed cert, which the
	// peer reports as a TLS alert) must NOT hit the terminal — under the TUI's alt-screen they
	// corrupt the display. Route every server's ErrorLog to a file instead of the default stderr.
	httpLog := log.New(io.Discard, "", 0)
	if f, ferr := os.OpenFile(filepath.Join(".avairy", "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
		httpLog = log.New(f, "", log.LstdFlags)
		defer f.Close()
	}

	// Local agents on this machine always reach the bus via a PLAIN loopback listener — they
	// never need TLS, even when the remote-facing bus is encrypted.
	lnLocal, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail("listen local bus", err)
	}
	go (&http.Server{Handler: mcpSrv.HTTPHandler(), ErrorLog: httpLog}).Serve(lnLocal)
	_, localPort, _ := net.SplitHostPort(lnLocal.Addr().String())
	busURL := "http://127.0.0.1:" + localPort + mcp.EndpointPath

	// Remote-facing bus on -mcp-addr: TLS when configured (a node's MCP proxy trusts the CA),
	// else plain. Carries inter-agent messages, which would otherwise cross the wire in cleartext.
	ln, err := net.Listen("tcp", *mcpAddr)
	if err != nil {
		fail("listen", err)
	}
	busScheme := "http"
	busSrv := &http.Server{Handler: mcpSrv.HTTPHandler(), ErrorLog: httpLog}
	switch {
	case *tlsAuto:
		busScheme = "https"
		busSrv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{serverCert}}
		go busSrv.ServeTLS(ln, "", "")
	case *tlsCert != "" && *tlsKey != "":
		busScheme = "https"
		go busSrv.ServeTLS(ln, *tlsCert, *tlsKey)
	default:
		go busSrv.Serve(ln)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Operator-spawned ephemeral consult agents (#24): /consult opens a disposable agent on core,
	// /close tears it down. Wired into the operator surface so the TUI and web both drive it.
	consults := &consultMgr{
		ctx: ctx, family: *family, model: *model, busURL: busURL, b: b, jrnl: jrnl,
		approvals: approvals, mcpSrv: mcpSrv, gateEdits: *gateEdits,
		cancel: map[string]context.CancelFunc{}, node: map[string]string{}, used: map[string]bool{},
	}
	svc.Consult = consults.Open
	svc.CloseConsult = consults.Close

	// Optionally serve the node control + operator API so remote daemons enroll/sync and remote
	// consoles attach. All of it (hub, seed sync, control.Core, servers, join bundles) lives in
	// serveControlAPI; it returns a cleanup to run at exit.
	if *controlAddr != "" {
		defer serveControlAPI(controlDeps{
			ctx: ctx, jrnl: jrnl, b: b, mcpSrv: mcpSrv, svc: svc,
			approvals: approvals, conflicts: conflictBroker, consults: consults,
			ca: ca, serverCert: serverCert, caPEM: caPEM, busScheme: busScheme, busLn: ln,
			opToken: opToken, httpLog: httpLog, mtlsEnabled: mtlsEnabled,
			controlAddr: controlAddr, workspaceDir: workspaceDir, tlsCert: tlsCert, tlsKey: tlsKey,
			advertise: advertise, send: send, tlsAuto: tlsAuto, web: web, headless: headless,
		})()
	}

	// Background services: the facilitator nudges stuck agents and routes @facilitator requests; the
	// cost monitor enforces budgets. An LLM-backed fresh look / dispatch picker needs a real agent
	// family, so it's gated to -live or a control server (a -demo mock loop must not spend credits).
	realAgents := *live || *controlAddr != ""
	startFacilitator(ctx, b, mcpSrv, bd, jrnl, realAgents, *family, *model)
	startCostMonitor(jrnl, b, *agentBudget, *budget)
	startFacilitatorDispatch(ctx, b, mcpSrv, jrnl, realAgents, *family, *model)

	caps := map[string]string{"os": runtime.GOOS}

	// Local agents are opt-in: none by default (bring agents via avairy-node). -live runs one
	// real 'alice'; -demo spawns mock alice+bob for the playground/tests; -send needs a local
	// 'alice' to talk to, so default it to a mock when neither -live nor -demo is set.
	runLiveAlice := *live
	runMockAlice := *demo && !*live
	runMockBob := *demo
	if *send != "" && !runLiveAlice && !runMockAlice {
		runMockAlice = true
	}

	if runLiveAlice {
		mcpSrv.RegisterAgent("alice", mcp.AgentRoles("alice", caps), caps)
		startLiveAlice(ctx, *family, *model, busURL, b, jrnl, approvals, *gateEdits, *idleSleep)
	}
	if runMockAlice {
		mcpSrv.RegisterAgent("alice", []string{"backend"}, caps)
		startMock(ctx, "alice", b, jrnl)
	}
	if runMockBob {
		mcpSrv.RegisterAgent("bob", []string{"backend"}, caps)
		startMock(ctx, "bob", b, jrnl)
	}

	if *send != "" {
		runOnce(b, jrnl, *send)
		return nil
	}
	if *headless {
		// Serve without a TUI: nodes enroll/sync and agents work; block until interrupted.
		fmt.Println("avairy: serving headless (no TUI) — ctrl+c to stop")
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Println("avairy: shutting down")
		return nil
	}
	// The attached TUI runs in-process against the same operator surface a remote client uses.
	return tui.Run(svc.Deps())
}

// controlDeps are the built components + flag pointers serveControlAPI needs. Flags are passed as
// pointers so the (verbatim) wiring below dereferences them exactly as it did inside main().
type controlDeps struct {
	ctx         context.Context
	jrnl        journal.Log
	b           *bus.Bus
	mcpSrv      *mcp.Server
	svc         *operator.Services
	approvals   *control.Approvals
	conflicts   *control.Conflicts
	consults    *consultMgr
	ca          *control.CA
	serverCert  tls.Certificate
	caPEM       []byte
	busScheme   string
	busLn       net.Listener
	opToken     string
	httpLog     *log.Logger
	mtlsEnabled bool

	controlAddr, workspaceDir, tlsCert, tlsKey, advertise, send *string
	tlsAuto, web, headless                                      *bool
}

// serveControlAPI stands up the node control API + operator API on one listener: restores/persists
// the canonical hub, seeds the workspace, wires the control.Core callbacks, serves (TLS per config),
// and writes the join bundles. Returns a cleanup to run at exit (persist hub, prune worktrees).
func serveControlAPI(d controlDeps) func() {
	ctx, jrnl, b, mcpSrv, svc := d.ctx, d.jrnl, d.b, d.mcpSrv, d.svc
	approvals, conflictBroker, consults := d.approvals, d.conflicts, d.consults
	ca, serverCert, caPEM := d.ca, d.serverCert, d.caPEM
	busScheme, ln, opToken, httpLog, mtlsEnabled := d.busScheme, d.busLn, d.opToken, d.httpLog, d.mtlsEnabled
	controlAddr, workspaceDir, tlsCert, tlsKey, advertise, send := d.controlAddr, d.workspaceDir, d.tlsCert, d.tlsKey, d.advertise, d.send
	tlsAuto, web, headless := d.tlsAuto, d.web, d.headless

	var commitFn func(string) (string, error)                    // operator-initiated /commit; nil unless git is enabled
	var bundleFn func(context.Context, []string) ([]byte, error) // repo bundle for node mirrors; nil unless git is enabled
	var operatorJoinFile string
	var cleanups []func()

	// Restore the canonical hub from disk so a core restart doesn't lose state (DESIGN.md §9);
	// persist it periodically and on clean shutdown.
	hubPath := filepath.Join(".avairy", "hub.json")
	hub, err := workspace.LoadHub(hubPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "avairy: load hub (starting empty):", err)
		hub = workspace.NewHub()
	}
	cleanups = append(cleanups, func() {
		if err := hub.Save(hubPath); err != nil {
			fmt.Fprintln(os.Stderr, "avairy: persist hub:", err)
		}
	})
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
	// Seed the canonical hub from the operator's project dir (synced both ways) and, if it's a git
	// repo, wire history/commit/bundle. Returns the /commit + bundle hooks (nil if no repo).
	if *workspaceDir != "" {
		var seedCleanup func()
		commitFn, bundleFn, seedCleanup = setupSeedWorkspace(ctx, mcpSrv, hub, *workspaceDir, conflictBroker, approvals, jrnl)
		cleanups = append(cleanups, seedCleanup)
	}
	svc.Commit = commitFn // operator /commit over the API/TUI (nil when no git repo)

	core := control.NewCore(hub, jrnl)
	consults.core = core                 // enable node-targeted consults (#24)
	core.RequireClientCert = mtlsEnabled // reject token enrollment; nodes join by mTLS client cert
	core.Approvals = approvals           // one broker feeds both node agents and the operator TUI
	core.Bundle = bundleFn               // serve the repo as a bundle for node mirrors (nil if no repo)
	core.OnConflict = func(agentID, path string, hubVersion uint64, hubContent, yourContent []byte) {
		body := fmt.Sprintf("CONFLICT on %s — another agent changed it (now hub v%d). Your working copy now has git-style conflict markers (<<<<<<< your edit / ======= / >>>>>>> hub); edit %s to resolve them and remove the markers — it will then sync as the next version. (Or submit a merge directly with resolve_conflict.)",
			path, hubVersion, path)
		b.Publish("avairy", bus.Agent(agentID), body, agent.DeliverySteer)
	}
	// A node's first-sync (startup) conflicts have no owning agent — surface the operator's choice
	// (resync / resolve / overview) on the Conflicts surface; the verdict rides back to the node on
	// its heartbeat (#21). One entry per node; Path carries the node id.
	core.OnNodeConflict = func(nodeID, summary string, paths []string) {
		conflictBroker.Raise(control.OperatorConflict{Path: nodeID, Source: "node-startup", Detail: summary})
	}
	svc.NodeDirective = core.SetNodeDirective
	// Conflict reconciliation (DESIGN.md §9): agents resolve divergent edits via resolve_conflict
	// (merged content → next canonical version). For a node agent, also unlock the path so the node
	// drops its stale markers and pulls the merged content (#22).
	mcpSrv.EnableConflicts(func(agentID, path string, content []byte) (uint64, error) {
		v := hub.Resolve(agentID, path, content).Version
		core.ResolveNodeConflict(agentID, path)
		return v, nil
	})
	// list_conflicts (#22): the agent's authoritative conflicted-file list, from what its node
	// reports on heartbeat — so it never greps for markers (agent id == node id).
	mcpSrv.EnableConflictList(core.NodeConflicts)
	// When a node enrolls, register its agent on the bus (identity, caps, inbox); deliver that
	// agent's inbound bus messages back over the control channel.
	core.OnEnroll = func(nodeID string, caps map[string]string) {
		mcpSrv.RegisterAgent(nodeID, mcp.AgentRoles(nodeID, caps), caps) // node id == agent's bus identity
	}
	core.InboxDrainer = func(agentID string) []control.InboxMessage {
		var out []control.InboxMessage
		for _, m := range mcpSrv.DrainInbox(agentID) {
			out = append(out, control.InboxMessage{ID: m.ID, From: m.From, Body: m.Body, Delivery: string(m.Delivery), Interrupt: m.Interrupt, ToKind: string(m.To.Kind)})
		}
		return out
	}
	// Serve the operator API (#18) alongside the node control API on one listener (shared TLS):
	// /operator/* → remote TUI/web clients; everything else → the node channel.
	rootHandler := func() http.Handler {
		mux := http.NewServeMux()
		mux.Handle("/operator/", operator.NewServer(svc, opToken, *web).Handler())
		mux.Handle("/", core.Handler())
		return mux
	}
	ctrlScheme := "http"
	ctrlSrv := &http.Server{Addr: *controlAddr, Handler: rootHandler(), ErrorLog: httpLog}
	serve := func() error {
		return ctrlSrv.ListenAndServe()
	}
	switch {
	case *tlsAuto:
		ctrlScheme = "https"
		ctrlSrv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{serverCert}, // shared self-CA cert (built above)
			ClientAuth:   tls.VerifyClientCertIfGiven,   // verify a node's client cert (mTLS) if presented; the operator API on this listener presents none
			ClientCAs:    ca.Pool(),
		}
		serve = func() error {
			return ctrlSrv.ListenAndServeTLS("", "")
		}
	case *tlsCert != "" && *tlsKey != "":
		ctrlScheme = "https"
		serve = func() error {
			return ctrlSrv.ListenAndServeTLS(*tlsCert, *tlsKey)
		}
	}
	go func() {
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, "control server:", err)
		}
	}()
	go core.RunLiveness(ctx) // mark nodes offline when heartbeats lapse

	ctrlURL := ctrlScheme + "://" + advertised(*advertise, *controlAddr)
	busBase := busScheme + "://" + advertised(*advertise, ln.Addr().String())
	warn := ""
	if unreachableHost(hostOf(advertised(*advertise, ln.Addr().String()))) {
		warn = "host not reachable from other machines — pass --advertise <ip/host> and bind --control-addr/--mcp-addr to 0.0.0.0:PORT"
	}
	// Token enrollment (opt-in, temporary): write a one-string join bundle (core URL + CA + token)
	// for the next node, refreshed when the token is read or rotated. Skipped when token joins are
	// off (the default) — nodes authenticate by minted mTLS client cert (`avairy core add-node`).
	joinPath := filepath.Join(".avairy", "join")
	writeJoin := func(tok string) {
		jb := control.EncodeJoin(control.JoinBundle{Core: ctrlURL, Bus: busBase, CA: caPEM, Token: tok})
		_ = os.WriteFile(joinPath, []byte(jb), 0o600)
	}
	curToken := func() string {
		if mtlsEnabled {
			return ""
		}
		t := core.CurrentToken()
		writeJoin(t)
		return t
	}
	var newTokenFn func() string
	if !mtlsEnabled {
		newTokenFn = func() string {
			t := core.NewPendingToken()
			writeJoin(t)
			return t
		}
	}
	// A remote operator attaches the same TUI from another machine. Bundle core URL + CA + the
	// operator token into one .avairy/operator-join string so `avairy tui connect --join-file` is a
	// single argument. (For mTLS, `core add-operator` mints a cert-carrying join instead.)
	operatorJoinFile = filepath.Join(".avairy", "operator-join")
	_ = os.WriteFile(operatorJoinFile, []byte(control.EncodeJoin(control.JoinBundle{Core: ctrlURL, CA: caPEM, Token: opToken})), 0o600)
	// The web console URL is only meaningful when --web actually serves the page.
	webURL := ""
	if *web {
		webURL = ctrlURL + operator.PathUI + "?token=" + opToken
	}
	joinFileShown := joinPath
	if mtlsEnabled {
		joinFileShown = "" // no token join when token enrollment is off
	}
	svc.Control = func() *operator.ControlState {
		return &operator.ControlState{ControlURL: ctrlURL, BusBase: busBase, Warn: warn, Token: curToken(), JoinFile: joinFileShown, OperatorJoin: operatorJoinFile, WebURL: webURL, MTLSOnly: mtlsEnabled}
	}
	svc.NewToken = newTokenFn
	// Under the TUI's alt-screen, stdout is hidden — so the token/join is shown in the TUI. Only
	// print here when there's no TUI (headless serve, or a one-shot --send).
	if *headless || *send != "" {
		fmt.Printf("control API:  %s\nMCP bus base: %s\n", ctrlURL, busBase)
		if !mtlsEnabled {
			fmt.Printf("enroll token: %s\nnode join:    %s\n", curToken(), joinPath)
		}
		fmt.Printf("operator token: %s\n", opToken)
		if webURL != "" {
			fmt.Printf("web console:  %s\n", webURL)
		}
		if warn != "" {
			fmt.Println("warning:", warn)
		}
	}

	return func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}

// setupSeedWorkspace seeds the canonical hub from the operator's project dir and keeps it synced
// both ways (so nodes get a working copy on their first SyncDown), routing seed conflicts to the
// operator's Conflicts view. If the dir is a git repo it also wires history/commit/bundle over the
// bus. Returns the operator /commit and repo-bundle hooks (nil when there's no repo) and a cleanup
// to run at exit.
func setupSeedWorkspace(ctx context.Context, mcpSrv *mcp.Server, hub *workspace.Hub, dir string, conflicts *control.Conflicts, approvals *control.Approvals, jrnl journal.Log) (commitFn func(string) (string, error), bundleFn func(context.Context, []string) ([]byte, error), cleanup func()) {
	cleanup = func() {}
	seed := workspace.NewNodeView("core")
	seed.ResumeFromHub(hub, dir) // adopt restored versions; don't re-conflict/delete
	// seedSyncUp pushes the operator's edits; for any that lost a race with a node's edit it writes
	// git-style markers locally + routes the owner-less conflict to the operator. A path held last
	// tick but not now → its markers were removed and it synced → clear the notification.
	seedSyncUp := func() {
		before := seed.LockedPaths()
		raised, serr := seed.SyncUp(hub, dir, workspace.IgnoreFor(dir))
		if serr != nil {
			fmt.Fprintln(os.Stderr, "avairy: seed workspace:", serr)
			return
		}
		for _, cf := range raised {
			if mErr := seed.MarkConflict(dir, cf); mErr != nil {
				continue
			}
			conflicts.Raise(control.OperatorConflict{
				Path: cf.Path, HubVersion: cf.Hub.Version, Source: "seed",
				Detail: "a node changed it while you were editing — your copy now has markers",
			})
		}
		for _, p := range before {
			if !seed.IsLocked(p) {
				conflicts.ClearPath(p)
			}
		}
	}
	seedSyncUp()
	var seedWatch <-chan struct{}
	if ch, werr := workspace.Watch(ctx, dir, workspace.IgnoreFor(dir)); werr != nil {
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
				seedSyncUp()
			case <-t.C: // fallback poll + pull remote changes (no server→node push)
				seedSyncUp()
				_ = seed.SyncDown(hub, dir)
			}
		}
	}()
	// If the operator's workspace is a git repo, expose history reads + gated signed commits over
	// the bus (DESIGN.md §9: the canonical repo lives only on core).
	if repo, gerr := git.Open(ctx, dir, true); gerr != nil {
		fmt.Fprintln(os.Stderr, "avairy: git tools disabled:", gerr)
	} else {
		mcpSrv.EnableGit(repo, gitApprover(approvals))
		cleanup = func() { repo.PruneWorktrees(context.Background()) } // disposable scratch checkouts
		commitFn = func(message string) (string, error) {
			hash, cerr := repo.Commit(context.Background(), nil, message)
			if cerr == nil {
				jrnl.Append(journal.KindSystem, "human", map[string]any{"event": "git_commit", "hash": hash, "message": message})
			}
			return hash, cerr
		}
		bundleFn = repo.Bundle
	}
	return commitFn, bundleFn, cleanup
}

// serverTLS builds the self-managed CA + one server cert covering the advertised control/bus hosts
// and loopback (DESIGN.md §4), shared by the MCP bus and the control channel. Returns zero values
// when tlsAuto is off.
func serverTLS(tlsAuto bool, advertise, mcpAddr, controlAddr string) (*control.CA, tls.Certificate, []byte) {
	if !tlsAuto {
		return nil, tls.Certificate{}, nil
	}
	ca, err := control.EnsureCA(".avairy")
	if err != nil {
		fail("ca", err)
	}
	cert, err := ca.ServerTLS([]string{
		hostOf(advertised(advertise, mcpAddr)),
		hostOf(advertised(advertise, controlAddr)),
		"127.0.0.1", "localhost", "::1",
	})
	if err != nil {
		fail("server cert", err)
	}
	return ca, cert, ca.CertPEM()
}

// startFacilitator wires the journal-watching facilitator that nudges stuck agents (DESIGN.md §5).
// With a real agent family in play (freshLook), it can run a clean-context fresh look on a loop.
func startFacilitator(ctx context.Context, b *bus.Bus, mcpSrv *mcp.Server, bd *board.Board, jrnl journal.Log, freshLook bool, family, model string) {
	fac := facilitator.New(b, facilitator.RosterFunc(func() []facilitator.Agent {
		metas := mcpSrv.AgentList()
		out := make([]facilitator.Agent, 0, len(metas))
		for _, m := range metas {
			out = append(out, facilitator.Agent{ID: m.ID, Caps: m.Caps})
		}
		return out
	}), facilitator.RuleNudger{})
	if freshLook {
		fac.FreshLook = makeFreshLook(family, model, bd, mcpSrv.Blackboard())
	}
	sub, _ := jrnl.Subscribe()
	go fac.Run(ctx, sub)
}

// startCostMonitor folds per-agent/total spend off the journal and, when a cap is crossed, journals
// a budget_exceeded event (surfaced in the consoles) and interrupts the runaway (#26).
func startCostMonitor(jrnl journal.Log, b *bus.Bus, agentBudget, totalBudget float64) {
	mon := cost.New(agentBudget, totalBudget)
	mon.OnExceed = func(agentID, scope string, spent float64) {
		actor := agentID
		if actor == "" {
			actor = "core"
		}
		jrnl.Append(journal.KindSystem, actor, map[string]any{
			"event": "budget_exceeded", "scope": scope, "agent": agentID, "spent": spent,
		})
		if scope == "agent" && agentID != "" {
			b.Interrupt("avairy", bus.Agent(agentID))
		} else {
			b.Interrupt("avairy", bus.Broadcast())
		}
	}
	sub, _ := jrnl.Subscribe()
	go mon.Run(sub)
}

// startFacilitatorDispatch handles @facilitator requests: triage via the dispatch cascade (rules,
// then an ephemeral LLM picker when several candidates qualify) and auto-assign one agent (or open a
// @team claim), journaling the routing (#team phase 2). Only this loop receives @facilitator messages.
func startFacilitatorDispatch(ctx context.Context, b *bus.Bus, mcpSrv *mcp.Server, jrnl journal.Log, llm bool, family, model string) {
	inbox, _ := b.Subscribe(bus.SenderFacilitator)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case req, ok := <-inbox:
				if !ok {
					return
				}
				if req.Interrupt {
					continue
				}
				roster := mcpSrv.AgentList()
				var workers []string
				for _, m := range roster {
					if m.ID == req.From || strings.HasPrefix(m.ID, "consult-") {
						continue // don't route a request back to its sender or to an ephemeral consult
					}
					workers = append(workers, m.ID)
				}
				var pick func() string
				if llm && len(workers) > 1 {
					pick = func() string { return pickWorker(ctx, family, model, req.Body, roster, workers) }
				}
				d, routed := dispatch.Decide(workers, pick)
				rec := map[string]any{"event": "facilitator_dispatch", "rule": d.Rule, "request": req.Body}
				if routed {
					b.Publish(bus.SenderFacilitator, d.To, req.Body, agent.DeliverySteer)
					if rec["to"] = d.To.Value; d.To.Value == "" {
						rec["to"] = string(d.To.Kind) // "team"
					}
				}
				jrnl.Append(journal.KindSystem, bus.SenderFacilitator, rec)
			}
		}
	}()
}

const aliceRole = "You are 'alice', a backend engineer agent in the avairy multi-agent system. " +
	"Collaborate ONLY through the avairy MCP tools: post_task, claim_task, list_tasks, " +
	"send_message, read_inbox, list_agents, claim_response, report_status, git_history, request_commit, scratch_worktree, list_conflicts, resolve_conflict, fresh_look, note, read_notes. Be terse and do exactly what you are asked, then stop. " +
	"When an inbox message has \"to\":\"team\", it's a request for ONE agent to take: call claim_response with its id before replying — answer only if \"granted\"; if \"denied\", stand down."

func startLiveAlice(ctx context.Context, family, model, busURL string, b *bus.Bus, jrnl journal.Log, approvals *control.Approvals, gateEdits bool, idle time.Duration) {
	if err := spawnLocalAgent(ctx, "alice", aliceRole, agent.SessionPersistent, family, model, busURL, b, jrnl, approvals, gateEdits, idle); err != nil {
		fail("start alice", err)
	}
}

// spawnLocalAgent starts an agent on core wired to the bus: it builds the family adapter with real
// §7 gating (keyed by id) once, then hands a spawn closure to a supervisor that drives the session
// and (when idle > 0) sleeps/respawns it on idle (#28). idle == 0 means never sleep — behaviorally a
// plain runner, which is what ephemeral consults (#24) use (ctx-cancel still tears them down).
func spawnLocalAgent(ctx context.Context, id, role string, mode agent.SessionMode, family, model, busURL string, b *bus.Bus, jrnl journal.Log, approvals *control.Approvals, gateEdits bool, idle time.Duration) error {
	ad, err := buildLocalAdapter(family, id, approvals, gateEdits)
	if err != nil {
		return err
	}
	// Each (re)spawn gets a fresh temp workspace; the adapter (and any gate server) is reused.
	spawn := func(sctx context.Context) (agent.Session, error) {
		ws, err := os.MkdirTemp("", "avairy-"+id+"-")
		if err != nil {
			return nil, err
		}
		cfg := agent.SessionConfig{
			AgentID:   id,
			Role:      role,
			Mode:      mode,
			Workspace: ws,
			Model:     model,
			MCP: []agent.MCPServer{{
				Name:    "avairy",
				Type:    "http",
				URL:     busURL,
				Headers: map[string]string{"X-Avairy-Agent": id},
			}},
		}
		if family == "codex" && cfg.Model == "haiku" { // the claude-flavored default isn't a codex model
			cfg.Model = ""
		}
		return ad.Start(sctx, cfg)
	}
	go supervisor.New(id, []string{"backend"}, spawn, b, jrnl, idle).Run(ctx)
	return nil
}

// buildLocalAdapter constructs the family adapter with real §7 gating keyed by id. For claude this
// starts a long-lived local /gate server whose URL is baked into the PreToolUse hook settings —
// built once and reused across respawns so sleep/wake cycles don't leak gate servers.
func buildLocalAdapter(family, id string, approvals *control.Approvals, gateEdits bool) (agent.Adapter, error) {
	dec := localGateDecider(approvals, id, gateEdits) // gated actions block on the operator's allow/deny
	if ad, ok := adapter.NewGated(family, dec); ok {
		return ad, nil
	}
	// claude (the default family): serve a local /gate and register a PreToolUse hook for every
	// tool call — the hook is the permission system, so we don't bypass permissions.
	gateURL, err := startLocalGate(dec)
	if err != nil {
		return nil, err
	}
	settings, err := gating.ClaudeHookSettings(gateURL)
	if err != nil {
		return nil, err
	}
	ca := claudecode.New()
	ca.ExtraArgs = []string{"--settings", settings}
	return ca, nil
}

// consultRole is the persona for an operator-spawned ephemeral consult agent (#24).
const consultRole = "You are an ephemeral CONSULT agent in avairy. The operator spun you up to " +
	"answer a question, validate something (e.g. whether a path is valid on THIS machine's OS), or " +
	"discuss an idea. Be concise and direct. You have the avairy MCP tools — use send_message to ask " +
	"other agents (e.g. one on a different OS) and read_inbox for their replies. CRITICAL: this " +
	"session is disposable and NOT saved — when it closes, everything here is gone. So anything worth " +
	"keeping (a finding, a decision, a follow-up) you MUST write to the shared blackboard with " +
	"note(key, text) or open a task with post_task. Don't rely on the operator to remember it."

// consultMgr spawns and tears down operator-requested ephemeral consult agents on core, assigning
// each a unique, location-encoded id (#24).
type consultMgr struct {
	ctx       context.Context
	family    string
	model     string
	busURL    string
	b         *bus.Bus
	jrnl      journal.Log
	approvals *control.Approvals
	mcpSrv    *mcp.Server
	gateEdits bool
	core      *control.Core // set when the control API is up; needed for node-targeted consults

	mu     sync.Mutex
	cancel map[string]context.CancelFunc // id -> cancel (core-local consults only)
	node   map[string]string             // id -> node id (node-targeted consults)
	used   map[string]bool
}

// Open spawns an ephemeral consult agent and returns its bus id. target "" / "core" runs it on core;
// otherwise it runs on that node (for OS-specific feedback). family overrides the default family.
func (cm *consultMgr) Open(target, family string) (string, error) {
	fam := family
	if fam == "" {
		fam = cm.family
	}
	if target != "" && target != "core" {
		return cm.openOnNode(target, fam)
	}
	id := cm.assignID("consult-core")
	cctx, cancel := context.WithCancel(cm.ctx)
	cm.mcpSrv.RegisterAgent(id, []string{"consult"}, map[string]string{"os": runtime.GOOS, "ephemeral": "true"})
	if err := spawnLocalAgent(cctx, id, consultRole, agent.SessionEphemeral, fam, cm.model, cm.busURL, cm.b, cm.jrnl, cm.approvals, cm.gateEdits, 0); err != nil {
		cancel()
		cm.mcpSrv.Unregister(id)
		cm.release(id)
		return "", err
	}
	cm.mu.Lock()
	cm.cancel[id] = cancel
	cm.mu.Unlock()
	// Journal it (not a TUI-local line) so both the TUI and the web console show it, and it's
	// audited. Registration already happened above, so the roster reflects it when this record lands.
	cm.jrnl.Append(journal.KindSystem, "human", map[string]any{"event": "consult_opened", "id": id})
	return id, nil
}

// openOnNode registers the consult on the bus and queues an open command for the node, which spawns
// it (with that OS/filesystem) and wires it to the bus on its next heartbeat (#24).
func (cm *consultMgr) openOnNode(node, family string) (string, error) {
	if cm.core == nil {
		return "", fmt.Errorf("node-targeted consults need the control API (-control-addr)")
	}
	if !cm.core.NodeOnline(node) {
		return "", fmt.Errorf("node %q is not online", node)
	}
	id := cm.assignID("consult-" + node)
	cm.mcpSrv.RegisterAgent(id, []string{"consult"}, map[string]string{"ephemeral": "true"})
	cm.core.QueueConsult(node, control.ConsultCommand{ID: id, Action: "open", Family: family})
	cm.mu.Lock()
	cm.node[id] = node
	cm.mu.Unlock()
	cm.jrnl.Append(journal.KindSystem, "human", map[string]any{"event": "consult_opened", "id": id, "node": node})
	return id, nil
}

// Close tears down a consult (local: cancel the session; node: queue a close command) and drops it
// from the bus. Reports whether it existed.
func (cm *consultMgr) Close(id string) bool {
	cm.mu.Lock()
	cancel := cm.cancel[id]
	node := cm.node[id]
	delete(cm.cancel, id)
	delete(cm.node, id)
	delete(cm.used, id)
	cm.mu.Unlock()
	switch {
	case cancel != nil: // core-local
		cancel()
	case node != "" && cm.core != nil: // on a node
		cm.core.QueueConsult(node, control.ConsultCommand{ID: id, Action: "close"})
	default:
		return false
	}
	cm.mcpSrv.Unregister(id)
	cm.jrnl.Append(journal.KindSystem, "human", map[string]any{"event": "consult_closed", "id": id})
	return true
}

func (cm *consultMgr) release(id string) {
	cm.mu.Lock()
	delete(cm.used, id)
	cm.mu.Unlock()
}

// assignID returns base, or base-2, base-3, … if earlier consults already took it.
func (cm *consultMgr) assignID(base string) string {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	id := base
	for i := 2; cm.used[id]; i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	cm.used[id] = true
	return id
}

// localGateDecider gates a local agent's actions via the §7 policy, routing gated ones to the
// operator (TUI approvals) through the shared broker — the in-process twin of the node's
// gateDecider. Free actions pass; gated actions block until allowed/denied (timeout → deny).
func localGateDecider(approvals *control.Approvals, agentID string, gateEdits bool) gating.Decider {
	policy := gating.Policy{GateEdits: gateEdits, Approve: func(ctx context.Context, req gating.Request) (gating.Decision, error) {
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

const freshLookRole = "You are a fresh pair of eyes with NO prior conversation context. Reason " +
	"independently from the facts you are given and answer concisely. You have no tools — just think and reply."

// makeFreshLook returns a fresh_look runner: each call spins up an ephemeral, clean-context
// session (same family/model as the live agent), seeds it with the current task board + the
// question, returns the answer, and tears the session down (DESIGN.md §8). Tools are denied so
// it stays a pure thinker.
func makeFreshLook(family, model string, bd *board.Board, bb *board.Blackboard) mcp.FreshLookFunc {
	return func(ctx context.Context, question string) (string, error) {
		ws, err := os.MkdirTemp("", "avairy-fresh-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(ws)
		prompt := "Current task board:\n" + boardSummary(bd) +
			"\n\nShared notes (blackboard):\n" + notesSummary(bb) +
			"\n\nQuestion: " + question + "\n\nGive your independent analysis."
		return oneShot(ctx, freshLookAdapter(family), freshLookRole, model, ws, prompt)
	}
}

func notesSummary(bb *board.Blackboard) string {
	notes := bb.Read("")
	if len(notes) == 0 {
		return "(none)"
	}
	var sb strings.Builder
	for _, n := range notes {
		fmt.Fprintf(&sb, "- [%s] %s\n", n.Key, n.Text)
	}
	return sb.String()
}

// denyAll gates every action closed — the fresh-look session is a pure thinker; any tool attempt
// is denied fast (not left pending), so a one-shot turn can't hang on a permission prompt.
func denyAll(context.Context, gating.Request) (gating.Decision, error) { return gating.Deny, nil }

// freshLookAdapter builds a plain, bus-less, tool-denied adapter for one-shot thinking.
func freshLookAdapter(family string) agent.Adapter {
	if ad, ok := adapter.NewGated(family, denyAll); ok {
		return ad
	}
	ca := claudecode.New() // claude (default): no tools — pure reasoning
	ca.ExtraArgs = []string{"--allowedTools", ""}
	return ca
}

// dispatchRole steers the ephemeral picker used by @facilitator: choose ONE agent by capability.
const dispatchRole = "You are a dispatcher. Choose the single best-suited agent for a request based " +
	"on the agents' capabilities and roles. Reply with ONLY one agent id, or the word \"team\" if any " +
	"of them could handle it equally well. No explanation, no punctuation."

// pickWorker asks an ephemeral one-shot session which agent should own a request (#team phase 2).
// Returns the chosen id (or "team"); on any error or garbage, an empty/unknown string, which the
// dispatch cascade treats as "open a team claim".
func pickWorker(ctx context.Context, family, model, request string, roster []mcp.AgentMeta, workers []string) string {
	ws, err := os.MkdirTemp("", "avairy-dispatch-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(ws)
	want := make(map[string]bool, len(workers))
	for _, w := range workers {
		want[w] = true
	}
	var b strings.Builder
	for _, m := range roster {
		if want[m.ID] {
			fmt.Fprintf(&b, "- %s (caps: %v, roles: %v)\n", m.ID, m.Caps, m.Roles)
		}
	}
	prompt := "An operator needs exactly ONE agent to own this request:\n\n" + request +
		"\n\nAvailable agents:\n" + b.String() + "\nReply with ONLY the best-suited id, or \"team\"."
	out, err := oneShot(ctx, freshLookAdapter(family), dispatchRole, model, ws, prompt)
	if err != nil {
		return ""
	}
	out = strings.Trim(strings.TrimSpace(out), "\"'`.")
	if i := strings.IndexAny(out, " \n\t"); i >= 0 {
		out = out[:i] // first token only
	}
	return out
}

// oneShot runs one ephemeral turn: start a fresh session, send prompt, collect assistant text
// until the turn completes, then close. Bounded so it can't hang. It deliberately persists
// NOTHING (no session id, throwaway workspace) — a fresh look must not touch the agent's real
// persistent session.
func oneShot(ctx context.Context, ad agent.Adapter, role, model, workspace, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	sess, err := ad.Start(ctx, agent.SessionConfig{
		AgentID:   "fresh-look",
		Role:      role,
		Mode:      agent.SessionEphemeral,
		Model:     model,
		Workspace: workspace,
	})
	if err != nil {
		return "", err
	}
	defer sess.Close()
	if err := sess.Send(ctx, prompt, agent.DeliverySteer); err != nil {
		return "", err
	}
	var sb strings.Builder
	for {
		select {
		case <-ctx.Done():
			return strings.TrimSpace(sb.String()), ctx.Err()
		case ev, ok := <-sess.Events():
			if !ok {
				return strings.TrimSpace(sb.String()), nil
			}
			switch ev.Type {
			case agent.EventText:
				sb.WriteString(ev.Text)
			case agent.EventError:
				return strings.TrimSpace(sb.String()), fmt.Errorf("%s", ev.Text)
			case agent.EventTurnDone:
				return strings.TrimSpace(sb.String()), nil
			}
		}
	}
}

func boardSummary(bd *board.Board) string {
	tasks := bd.List()
	if len(tasks) == 0 {
		return "(no tasks)"
	}
	var sb strings.Builder
	for _, t := range tasks {
		sb.WriteString(fmt.Sprintf("- %s [%s] %q\n", t.ID, t.State, t.Title))
	}
	return sb.String()
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

// Default ports avairy serves on, used to infer URLs from -advertise: control on 7700 (the
// convention), the MCP bus on 7702 (matches -mcp-addr's default). Give -core/-mcp to override.
const (
	defaultControlPort = "7700"
	defaultBusPort     = "7702"
)

// addNodeCommand (`avairy core add-node`) invites a node by minting an mTLS client-cert join
// bundle from the self-managed CA — no enrollment token. The node it's given to authenticates by
// certificate (durable membership). Prints the join string (redirect it to a file).
func addNodeCommand() *cli.Command {
	return &cli.Command{
		Name:  "add-node",
		Usage: "invite a node: mint an mTLS client-cert join bundle (prints the join string)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "id", Required: true, Usage: "node id (becomes the client cert identity)"},
			&cli.StringFlag{Name: "advertise", Usage: "core host/IP — derives --core (https://host:7700) and --mcp (https://host:7702)"},
			&cli.StringFlag{Name: "core", Usage: "control API URL the node will dial (https://…); overrides --advertise"},
			&cli.StringFlag{Name: "mcp", Usage: "MCP bus base URL to bundle (so the node can run the agent); overrides --advertise"},
			&cli.StringFlag{Name: "dir", Value: ".avairy", Usage: "directory holding the CA (ca.crt/ca.key)"},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			coreURL, busURL := cmd.String("core"), cmd.String("mcp")
			if adv := cmd.String("advertise"); adv != "" {
				if coreURL == "" {
					coreURL = "https://" + adv + ":" + defaultControlPort
				}
				if busURL == "" {
					busURL = "https://" + adv + ":" + defaultBusPort
				}
			}
			if coreURL == "" {
				return fmt.Errorf("need --advertise <host> (or an explicit --core URL)")
			}
			ca, err := control.EnsureCA(cmd.String("dir"))
			if err != nil {
				return fmt.Errorf("ca: %w", err)
			}
			cert, key, err := ca.ClientTLS(cmd.String("id"))
			if err != nil {
				return fmt.Errorf("client cert: %w", err)
			}
			fmt.Println(control.EncodeJoin(control.JoinBundle{
				Core: coreURL, Bus: busURL, CA: ca.CertPEM(), NodeID: cmd.String("id"), ClientCert: cert, ClientKey: key,
			}))
			return nil
		},
	}
}

// addOperatorCommand (`avairy core add-operator`) invites an operator console. It mints ONE
// operator client cert and emits it in both forms the console can use: a password-protected .p12
// to import into a browser / OS keychain, and a join bundle (CA + cert + key) so
// `avairy tui connect --join-file <file>` authenticates by mTLS. Run on the core host.
func addOperatorCommand() *cli.Command {
	return &cli.Command{
		Name:  "add-operator",
		Usage: "invite an operator console: mint a cert (.p12 for the browser + join for `tui connect`)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "name", Value: "operator", Usage: "operator identity embedded in the cert"},
			&cli.StringFlag{Name: "advertise", Usage: "core host/IP — derives --core (https://host:7700)"},
			&cli.StringFlag{Name: "core", Usage: "control API URL the console will dial (https://…); overrides --advertise"},
			&cli.StringFlag{Name: "dir", Value: ".avairy", Usage: "directory holding the CA (ca.crt/ca.key)"},
			&cli.StringFlag{Name: "p12", Value: "operator.p12", Usage: "output PKCS#12 file to import into the browser"},
			&cli.StringFlag{Name: "join", Value: "operator.join", Usage: "output join bundle for `avairy tui connect --join-file`"},
			&cli.StringFlag{Name: "password", Usage: "PKCS#12 password (default: random, printed)"},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			coreURL := cmd.String("core")
			if coreURL == "" && cmd.String("advertise") != "" {
				coreURL = "https://" + cmd.String("advertise") + ":" + defaultControlPort
			}
			ca, err := control.EnsureCA(cmd.String("dir"))
			if err != nil {
				return fmt.Errorf("ca: %w", err)
			}
			name := cmd.String("name")
			pw := cmd.String("password")
			if pw == "" {
				pw = operator.RandomToken()[:16]
			}
			// One identity, two artifacts: the .p12 for browser import…
			p12, err := ca.OperatorP12(name, pw)
			if err != nil {
				return err
			}
			if err := os.WriteFile(cmd.String("p12"), p12, 0o600); err != nil {
				return fmt.Errorf("write p12: %w", err)
			}
			fmt.Printf("wrote %s (password: %s) — import into your browser/OS keychain, then open the console with NO ?token=\n", cmd.String("p12"), pw)
			// …and a join bundle carrying the same cert+key, for the remote TUI (mTLS).
			if coreURL == "" {
				fmt.Fprintln(os.Stderr, "note: no --core/--advertise given, so no join bundle for `tui connect` was written (the .p12 still works in a browser)")
				return nil
			}
			cert, key, err := ca.OperatorTLS(name)
			if err != nil {
				return fmt.Errorf("operator cert: %w", err)
			}
			join := control.EncodeJoin(control.JoinBundle{Core: coreURL, CA: ca.CertPEM(), ClientCert: cert, ClientKey: key})
			if err := os.WriteFile(cmd.String("join"), []byte(join), 0o600); err != nil {
				return fmt.Errorf("write join: %w", err)
			}
			fmt.Printf("wrote %s — attach with: avairy tui connect --join-file %s\n", cmd.String("join"), cmd.String("join"))
			return nil
		},
	}
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

// runOnce sends one message to a local alice, waits for her turn to complete, and prints the journal.
func runOnce(b *bus.Bus, jrnl journal.Log, msg string) {
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
