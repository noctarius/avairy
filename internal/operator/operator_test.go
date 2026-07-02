package operator_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/control"
	"avairy/internal/journal"
	"avairy/internal/operator"
)

// mTLS auth (#30): a verified operator client cert authenticates the API with no token; a node
// cert (CA-signed but not an operator cert) does not, and a plain client with neither is rejected.
func TestOperatorMTLSAuth(t *testing.T) {
	ca, err := control.EnsureCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := ca.ServerTLS([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	j := journal.NewMemory()
	svc := &operator.Services{Journal: j, Bus: bus.New(j), Approvals: control.NewApprovals(0), Conflicts: control.NewConflicts()}
	ts := httptest.NewUnstartedServer(operator.NewServer(svc, "sekret", false).Handler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: ca.Pool()}
	ts.StartTLS()
	defer ts.Close()

	client := func(cert *tls.Certificate) *http.Client {
		tc := &tls.Config{RootCAs: ca.Pool()}
		if cert != nil {
			tc.Certificates = []tls.Certificate{*cert}
		}
		return &http.Client{Transport: &http.Transport{TLSClientConfig: tc}}
	}
	status := func(c *http.Client) int {
		resp, err := c.Get(ts.URL + operator.PathState) // no ?token=
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	keypair := func(certPEM, keyPEM []byte) *tls.Certificate {
		kp, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return &kp
	}

	opCert, opKey, _ := ca.OperatorTLS("op")
	if got := status(client(keypair(opCert, opKey))); got != http.StatusOK {
		t.Fatalf("operator cert should authenticate token-less: got %d", got)
	}
	nodeCert, nodeKey, _ := ca.ClientTLS("linbot")
	if got := status(client(keypair(nodeCert, nodeKey))); got != http.StatusUnauthorized {
		t.Fatalf("node cert must NOT authenticate the operator API: got %d", got)
	}
	if got := status(client(nil)); got != http.StatusUnauthorized {
		t.Fatalf("no cert + no token should be unauthorized: got %d", got)
	}
}

// End-to-end: an in-process Services served over the operator API, attached by a Client, must
// replay journal history, stream new records, reflect state (tasks/approvals/conflicts/roster), and
// route an operator action (inject) back to the live bus.
func TestOperatorServerClientRoundTrip(t *testing.T) {
	j := journal.NewMemory()
	b := bus.New(j)
	bd := board.New(j)
	approvals := control.NewApprovals(time.Minute)
	conflicts := control.NewConflicts()

	// Pre-existing history (must arrive as backfill) + state the client should see.
	b.Publish("human", bus.Broadcast(), "hello before connect", agent.DeliverySteer)
	bd.Post("human", "repro the panic", map[string]string{"os": "linux"}, nil)
	approvals.OnRequest = func(control.Approval) {}
	go approvals.Ask(context.Background(), control.Approval{AgentID: "linbot", Kind: "command", Summary: "git push"})
	conflicts.Raise(control.OperatorConflict{Path: "f.go", HubVersion: 3, Source: "seed"})
	time.Sleep(20 * time.Millisecond) // let the async Ask register

	bb := board.NewBlackboard(j)
	bb.Write("alice", "repro/linux", "panics only on linux")
	svc := &operator.Services{
		Journal: j, Bus: b, Approvals: approvals, Conflicts: conflicts,
		Tasks:  bd.List,
		Notes:  func() []board.Note { return bb.Read("") },
		Roster: func() []string { return []string{"alice", "linbot"} },
	}
	ts := httptest.NewServer(operator.NewServer(svc, "sekret", true).Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := operator.Connect(ctx, ts.URL, "sekret", ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	deps := client.Deps()

	// Backfill replayed: the pre-connect message is in the client's journal.
	if !hasMessage(deps.Journal.Records(), "hello before connect") {
		t.Fatal("backfill did not replay the pre-connect message")
	}
	// State snapshot reflects tasks/approvals/conflicts/roster.
	if got := deps.Tasks(); len(got) != 1 || got[0].Title != "repro the panic" {
		t.Fatalf("tasks = %+v", got)
	}
	if got := deps.PendingApprovals(); len(got) != 1 || got[0].AgentID != "linbot" {
		t.Fatalf("approvals = %+v", got)
	}
	if got := deps.PendingConflicts(); len(got) != 1 || got[0].Path != "f.go" {
		t.Fatalf("conflicts = %+v", got)
	}
	if got := deps.Notes(); len(got) != 1 || got[0].Key != "repro/linux" || got[0].Text != "panics only on linux" {
		t.Fatalf("notes = %+v", got)
	}
	if got := deps.Roster(); len(got) != 2 {
		t.Fatalf("roster = %+v", got)
	}

	// A live record streams through after connect.
	b.Publish("alice", bus.Broadcast(), "live update", agent.DeliverySteer)
	if !eventually(t, func() bool { return hasMessage(deps.Journal.Records(), "live update") }) {
		t.Fatal("live record did not stream to the client")
	}

	// An operator action (inject) routes back to the live bus.
	deps.Inject("alice", "do the thing")
	if !eventually(t, func() bool { return hasMessage(j.Records(), "do the thing") }) {
		t.Fatal("inject did not reach the core bus")
	}

	// Resolving an approval through the client unblocks the waiting Ask (it's gone from pending).
	deps.ResolveApproval(deps.PendingApprovals()[0].ID, control.DecisionAllow)
	if !eventually(t, func() bool { return len(approvals.Pending()) == 0 }) {
		t.Fatal("approval was not resolved via the operator API")
	}
}

// Consult / close round-trip through the operator API: the client's Deps drive svc.Consult and
// svc.CloseConsult (so avairy-tui / the web can spawn ephemeral consults remotely, #24).
func TestOperatorConsultRoundTrip(t *testing.T) {
	j := journal.NewMemory()
	var gotTarget, gotFamily, closed string
	rc := make(chan [3]string, 1)
	svc := &operator.Services{
		Journal: j, Bus: bus.New(j), Approvals: control.NewApprovals(0), Conflicts: control.NewConflicts(),
		Consult: func(target, family string) (string, error) {
			gotTarget, gotFamily = target, family
			return "consult-" + nonEmpty(target, "core"), nil
		},
		CloseConsult:     func(id string) bool { closed = id; return true },
		ReconfigureAgent: func(agent, model, effort string) { rc <- [3]string{agent, model, effort} },
	}
	ts := httptest.NewServer(operator.NewServer(svc, "sekret", false).Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := operator.Connect(ctx, ts.URL, "sekret", ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	deps := client.Deps()

	id, err := deps.Consult("linux", "codex")
	if err != nil || id != "consult-linux" {
		t.Fatalf("consult = %q err=%v", id, err)
	}
	if gotTarget != "linux" || gotFamily != "codex" {
		t.Fatalf("consult args = (%q,%q)", gotTarget, gotFamily)
	}
	if !deps.CloseConsult("consult-linux") || closed != "consult-linux" {
		t.Fatalf("close not routed: closed=%q", closed)
	}

	deps.Reconfigure("linux", "opus", "high")
	select {
	case got := <-rc:
		if got != [3]string{"linux", "opus", "high"} {
			t.Fatalf("reconfigure args = %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconfigure did not route to the service")
	}
}

func nonEmpty(s, dflt string) string {
	if s == "" {
		return dflt
	}
	return s
}

// Unauthorized requests are rejected when a token is set.
func TestOperatorAuth(t *testing.T) {
	j := journal.NewMemory()
	svc := &operator.Services{Journal: j, Bus: bus.New(j), Approvals: control.NewApprovals(0), Conflicts: control.NewConflicts()}
	ts := httptest.NewServer(operator.NewServer(svc, "sekret", true).Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := operator.Connect(ctx, ts.URL, "wrong-token", ts.Client()); err == nil {
		t.Fatal("expected connect with a bad token to fail")
	}
}

func hasMessage(recs []journal.Record, body string) bool {
	for _, r := range recs {
		if m, ok := r.Data.(bus.Message); ok && m.Body == body {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, cond func() bool) bool {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// A leading "@<id> " in an injected message addresses that agent over the bus, even with the
// recipient selector on broadcast (the web composer's behavior). Reproducer for the bug where the
// web sent "@macos …" as a broadcast: before Inject parsed the mention it published to everyone
// with the "@macos" still in the body.
func TestServicesInjectParsesLeadingMention(t *testing.T) {
	j := journal.NewMemory()
	b := bus.New(j)
	macos, _ := b.Subscribe("macos") // direct recipient
	all, _ := b.Subscribe("watcher") // a bystander, to confirm it is NOT a broadcast
	svc := &operator.Services{Journal: j, Bus: b}

	svc.Inject("", "@macos rebuild please") // selector on broadcast; mention should target macos

	select {
	case m := <-macos:
		if m.Body != "rebuild please" || m.To.Kind != bus.ToAgent || m.To.Value != "macos" {
			t.Fatalf("macos got %+v, want body %q addressed agent:macos", m, "rebuild please")
		}
	case <-time.After(time.Second):
		t.Fatal("mention did not reach macos")
	}
	select {
	case m := <-all:
		t.Fatalf("a leading @mention must not broadcast; watcher got %+v", m)
	case <-time.After(100 * time.Millisecond):
	}

	// No leading mention → genuine broadcast (reaches the bystander).
	svc.Inject("", "status update for everyone")
	select {
	case m := <-all:
		if m.Body != "status update for everyone" || m.To.Kind != bus.ToBroadcast {
			t.Fatalf("broadcast got %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("plain message did not broadcast")
	}
}

// React: 👍 delivers context-only feedback (no interrupt), ❌ interrupts then steers a reconsider,
// each journals a reaction badge — and a message older than the per-agent window is ignored.
func TestReact(t *testing.T) {
	j := journal.NewMemory()
	b := bus.New(j)
	svc := &operator.Services{Journal: j, Bus: b}

	rec := j.Append(journal.KindAgentEvent, "linux", agent.Event{Type: agent.EventText, Text: "editing the hash"})
	inbox, _ := b.Subscribe("linux")

	svc.React(rec.Seq, operator.ReactUp)
	got := drainMsgs(inbox)
	if len(got) != 1 || !got[0].NoWake || got[0].Interrupt {
		t.Fatalf("👍 should deliver one context-only (NoWake) message, got %+v", got)
	}
	if !hasReaction(j, rec.Seq, "up") {
		t.Fatal("👍 should journal a reaction badge")
	}

	svc.React(rec.Seq, operator.ReactReject)
	got = drainMsgs(inbox)
	if len(got) != 2 || !got[0].Interrupt || got[1].Interrupt || got[1].NoWake {
		t.Fatalf("❌ should interrupt then steer a reconsider, got %+v", got)
	}

	// Push enough newer messages that rec falls out of linux's last-ReactWindow → no longer reactable.
	for i := 0; i < operator.ReactWindow; i++ {
		j.Append(journal.KindAgentEvent, "linux", agent.Event{Type: agent.EventText, Text: fmt.Sprintf("step %d", i)})
	}
	svc.React(rec.Seq, operator.ReactUp)
	if got := drainMsgs(inbox); len(got) != 0 {
		t.Fatalf("reacting to a message older than the window should be a no-op, got %+v", got)
	}
}

func drainMsgs(ch <-chan bus.Message) []bus.Message {
	var out []bus.Message
	for {
		select {
		case m := <-ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

func hasReaction(j journal.Log, seq uint64, kind string) bool {
	for _, r := range j.Records() {
		if r.Kind != journal.KindSystem {
			continue
		}
		if d, ok := r.Data.(map[string]any); ok && d["event"] == "reaction" && d["seq"] == seq && d["kind"] == kind {
			return true
		}
	}
	return false
}
