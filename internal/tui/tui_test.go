package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

// /consult spawns via the Consult dep (parsing an optional @target + family); /close tears down.
func TestConsultCommands(t *testing.T) {
	j := journal.NewMemory()
	d := depsFor(bus.New(j), board.New(j), j)
	var gotTarget, gotFamily, closed string
	d.Consult = func(target, family string) (string, error) {
		gotTarget, gotFamily = target, family
		return "consult-" + orCore(target), nil
	}
	d.CloseConsult = func(id string) bool { closed = id; return true }
	m := NewModel(d)

	// "/consult @linux codex" → target "linux", family "codex".
	m.input.SetValue("/consult @linux codex")
	if cmd := m.submit(); cmd != nil {
		cmd() // run the async consult off-thread closure
	}
	if gotTarget != "linux" || gotFamily != "codex" {
		t.Fatalf("consult args = (%q,%q), want (linux,codex)", gotTarget, gotFamily)
	}

	// bare "/consult" → core, no family.
	m.input.SetValue("/consult")
	if cmd := m.submit(); cmd != nil {
		cmd()
	}
	if gotTarget != "" || gotFamily != "" {
		t.Fatalf("bare consult args = (%q,%q), want empty", gotTarget, gotFamily)
	}

	// "/end consult-core" tears it down.
	m.input.SetValue("/end consult-core")
	m.submit()
	if closed != "consult-core" {
		t.Fatalf("close id = %q, want consult-core", closed)
	}
}

func orCore(s string) string {
	if s == "" {
		return "core"
	}
	return s
}

// Scrollback: at the tail the latest line shows and the oldest is off-screen; scrolling up reveals
// older lines and hides the latest; a new record while scrolled up keeps the viewport anchored.
func TestScrollback(t *testing.T) {
	m, _, _ := newTestModel()
	m.height = 20 // small body so only a few rows fit
	for i := 0; i < 40; i++ {
		m.apply(journal.Record{Seq: uint64(i + 1), Kind: journal.KindMessage, Actor: "human",
			Data: bus.Message{From: "human", To: bus.Broadcast(), Body: fmt.Sprintf("line-%d", i)}})
	}
	v := m.render()
	if !strings.Contains(v, "line-39") || strings.Contains(v, "line-0") {
		t.Fatalf("at the tail: latest visible, oldest hidden:\n%s", v)
	}

	m.scroll = 30 // scroll up
	v = m.render()
	if strings.Contains(v, "line-39") || !strings.Contains(v, "line-9") {
		t.Fatalf("scrolled up: latest hidden, older visible:\n%s", v)
	}

	// A new record while scrolled grows the offset by the added rows (viewport stays put).
	before := m.scroll
	m.Update(recordMsg(journal.Record{Seq: 100, Kind: journal.KindMessage, Actor: "human",
		Data: bus.Message{From: "human", To: bus.Broadcast(), Body: "NEW"}}))
	if m.scroll != before+1 {
		t.Fatalf("scroll should grow by the new row: got %d, want %d", m.scroll, before+1)
	}

	m.scroll = 0 // back to the tail
	if !strings.Contains(m.render(), "NEW") {
		t.Fatal("at the tail the newest line should show")
	}
}

func TestHighlightMentions(t *testing.T) {
	m, _, _ := newTestModel()
	m.touchAgent("linux") // a known agent

	out := m.highlightMentions("ping @linux and @all about @media settings")
	if !strings.Contains(out, mentionStyle.Render("@linux")) {
		t.Fatalf("known agent mention not highlighted: %q", out)
	}
	if !strings.Contains(out, mentionStyle.Render("@all")) {
		t.Fatalf("@all not highlighted: %q", out)
	}
	// @media is not an agent → must be left as-is (no false-match in prose/code).
	if strings.Contains(out, mentionStyle.Render("@media")) {
		t.Fatalf("non-agent token wrongly highlighted: %q", out)
	}
	if !strings.Contains(out, "@media") {
		t.Fatalf("non-agent token should survive verbatim: %q", out)
	}
}

func newTestModel() (*Model, *bus.Bus, journal.Log) {
	j := journal.NewMemory()
	b := bus.New(j)
	bd := board.New(j)
	return NewModel(depsFor(b, bd, j)), b, j
}

// depsFor wires the interface-level Deps to a live bus+board, the same way the in-process operator
// services do (kept here so the tui package's tests don't import operator, which imports tui).
func depsFor(b *bus.Bus, bd *board.Board, j journal.Log) Deps {
	return Deps{
		Journal: j,
		Tasks:   bd.List,
		Inject: func(target, body string) {
			to := bus.Broadcast()
			if target != "" && target != "broadcast" {
				to = bus.Agent(target)
			}
			b.Publish("human", to, body, agent.DeliverySteer)
		},
		Interrupt: func() { b.Interrupt("human", bus.Broadcast()) },
	}
}

// Records fold into the conversation view and the fleet, and View renders them.
func TestApplyAndView(t *testing.T) {
	m, _, _ := newTestModel()
	m.apply(journal.Record{Seq: 1, Kind: journal.KindMessage, Actor: "human",
		Data: bus.Message{ID: "m1", From: "human", To: bus.Agent("alice"), Body: "repro it"}})
	m.apply(journal.Record{Seq: 2, Kind: journal.KindAgentEvent, Actor: "alice",
		Data: agent.Event{Type: agent.EventText, Text: "on it"}})

	v := m.render()
	for _, want := range []string{"repro it", "on it", "alice"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view missing %q:\n%s", want, v)
		}
	}
}

// The Approvals tab renders pending gated actions, and y/n on a selected row resolves it.
func TestApprovalsViewAndResolve(t *testing.T) {
	pending := []ApprovalItem{
		{ID: "ap1", AgentID: "linbot", Kind: "command", Summary: "git push origin main", Reason: "ship it"},
		{ID: "ap2", AgentID: "macbot", Kind: "install", Summary: "brew install jq"},
	}
	var resolved [2]string // id, decision
	j := journal.NewMemory()
	b := bus.New(j)
	bd := board.New(j)
	d := depsFor(b, bd, j)
	d.PendingApprovals = func() []ApprovalItem { return pending }
	d.ResolveApproval = func(id, decision string) {
		resolved = [2]string{id, decision}
		// drop the resolved item so the list shrinks like the real broker
		out := pending[:0]
		for _, p := range pending {
			if p.ID != id {
				out = append(out, p)
			}
		}
		pending = out
	}
	m := NewModel(d)
	// The pending count badges in the tab bar from any tab (inactive tab → unstyled text).
	if v := m.render(); !strings.Contains(v, "Approvals (2)") {
		t.Fatalf("tab bar missing pending badge:\n%s", v)
	}

	m.tab = tabApprovals
	v := m.render()
	for _, want := range []string{"git push origin main", "brew install jq", "linbot"} {
		if !strings.Contains(v, want) {
			t.Fatalf("approvals view missing %q:\n%s", want, v)
		}
	}

	// Move to the second row and deny it.
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if resolved != [2]string{"ap2", "deny"} {
		t.Fatalf("expected ap2 denied, got %v", resolved)
	}
	// Allow the remaining one for the whole session with 'a'.
	m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if resolved != [2]string{"ap1", "allow_for_session"} {
		t.Fatalf("expected ap1 allowed-for-session, got %v", resolved)
	}
}

// "/commit <msg>" runs the Commit dep off-thread and folds the result into the conversation.
func TestSlashCommit(t *testing.T) {
	j := journal.NewMemory()
	b := bus.New(j)
	bd := board.New(j)
	var gotMsg string
	d := depsFor(b, bd, j)
	d.Commit = func(message string) (string, error) { gotMsg = message; return "abc1234", nil }
	m := NewModel(d)

	m.input.SetValue("/commit fix the panic")
	cmd := m.submit()
	if cmd == nil {
		t.Fatal("/commit should return a command")
	}
	if msg, ok := cmd().(commitResultMsg); !ok || msg.hash != "abc1234" {
		t.Fatalf("commit cmd produced %#v", cmd())
	}
	if gotMsg != "fix the panic" {
		t.Fatalf("commit message = %q", gotMsg)
	}
	// Feeding the result back renders a confirmation line.
	m.Update(commitResultMsg{hash: "abc1234"})
	if !strings.Contains(strings.Join(m.conv, "\n"), "committed abc1234") {
		t.Fatalf("conversation missing commit confirmation: %v", m.conv)
	}

	// No message → usage hint, no command. No Commit dep → unavailable note.
	m.input.SetValue("/commit")
	if m.submit() != nil {
		t.Fatal("/commit with no message should not run")
	}
	m.deps.Commit = nil
	m.input.SetValue("/commit something")
	if m.submit() != nil {
		t.Fatal("/commit with no git repo should not run")
	}
}

// Dedup: applying the same Seq twice doesn't double-render.
func TestApplyDedup(t *testing.T) {
	m, _, _ := newTestModel()
	rec := journal.Record{Seq: 7, Kind: journal.KindAgentEvent, Actor: "alice",
		Data: agent.Event{Type: agent.EventText, Text: "hi"}}
	m.apply(rec)
	m.apply(rec)
	if got := strings.Count(strings.Join(m.conv, "\n"), "hi"); got != 1 {
		t.Fatalf("expected 1 conv line, got %d", got)
	}
}

// Submitting "@alice ..." publishes a directed bus message (human injection).
func TestSubmitPublishesDirected(t *testing.T) {
	m, _, j := newTestModel()
	m.input.SetValue("@alice please reproduce")
	m.submit()

	found := false
	for _, rec := range j.Records() {
		if rec.Kind != journal.KindMessage {
			continue
		}
		if msg, ok := rec.Data.(bus.Message); ok &&
			msg.From == "human" && msg.To.Kind == bus.ToAgent && msg.To.Value == "alice" && msg.Body == "please reproduce" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a directed human message to agent:alice on the bus")
	}
}

// A bare line broadcasts.
func TestSubmitBroadcast(t *testing.T) {
	m, _, j := newTestModel()
	m.input.SetValue("standup in 5")
	m.submit()

	found := false
	for _, rec := range j.Records() {
		if msg, ok := rec.Data.(bus.Message); ok && msg.To.Kind == bus.ToBroadcast && msg.Body == "standup in 5" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a broadcast human message on the bus")
	}
}

// Node-lifecycle system records must not appear as agents in the fleet; only agent
// report_status does.
func TestSystemRecordsFleet(t *testing.T) {
	m, _, _ := newTestModel()
	m.apply(journal.Record{Seq: 1, Kind: journal.KindSystem, Actor: "claude-macos",
		Data: map[string]any{"event": "node_enrolled", "os": "darwin"}})
	if len(m.agentOrder) != 0 {
		t.Fatalf("node_enrolled must not create a fleet agent, got %v", m.agentOrder)
	}
	m.apply(journal.Record{Seq: 2, Kind: journal.KindSystem, Actor: "claude",
		Data: map[string]any{"event": "report_status", "status": "blocked"}})
	if a := m.agents["claude"]; a == nil || a.status != "blocked" {
		t.Fatalf("report_status should mark claude blocked, got %+v", a)
	}
}

// A node that enrolls WITH an agent family must appear in the fleet immediately (online/idle), not
// stay invisible until its first turn; a re-enroll after an offline lapse brings it back online.
func TestNodeEnrolledWithFamilyShowsOnline(t *testing.T) {
	m, _, _ := newTestModel()
	caps := map[string]string{"os": "linux", "family": "claude"}
	m.apply(journal.Record{Seq: 1, Kind: journal.KindSystem, Actor: "claude-linux",
		Data: map[string]any{"event": "node_enrolled", "os": "linux", "caps": caps}})
	if a := m.agents["claude-linux"]; a == nil || a.status != "idle" {
		t.Fatalf("an agent-driving node should appear idle on enroll, got %+v", a)
	}

	m.apply(journal.Record{Seq: 2, Kind: journal.KindSystem, Actor: "claude-linux",
		Data: map[string]any{"event": "node_offline"}})
	if m.agents["claude-linux"].status != "offline" {
		t.Fatal("node_offline should mark it offline")
	}
	m.apply(journal.Record{Seq: 3, Kind: journal.KindSystem, Actor: "claude-linux",
		Data: map[string]any{"event": "node_rejoined", "os": "linux", "caps": caps}})
	if got := m.agents["claude-linux"].status; got != "idle" {
		t.Fatalf("node_rejoined should bring it back online, got %q", got)
	}
}

// /diff opens an agent's most recent edit diff in the scrollable modal; esc closes it; and 'd' on a
// pending edit approval opens that edit's diff in the same modal.
func TestDiffViewing(t *testing.T) {
	j := journal.NewMemory()
	m := NewModel(Deps{Journal: j, Inject: func(string, string) {}})
	m.width, m.height = 120, 40
	rec := j.Append(journal.KindAgentEvent, "linux", agent.Event{Type: agent.EventToolUse,
		Tool: &agent.ToolCall{Name: "Edit", Input: map[string]any{"file_path": "f.go", "_diff": "@@ -1 +1 @@\n-a\n+b"}}})
	m.apply(rec)

	m.input.SetValue("/diff")
	m.submit()
	if !m.diffOpen {
		t.Fatal("/diff should open the modal")
	}
	if !strings.Contains(m.diffVP.View(), "+b") {
		t.Fatalf("modal should show the diff, got:\n%s", m.diffVP.View())
	}
	if !strings.Contains(m.composed(), "─") {
		t.Fatal("composed view should overlay the bordered modal")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.diffOpen {
		t.Fatal("esc should close the modal")
	}

	m2 := NewModel(Deps{
		Journal:          journal.NewMemory(),
		PendingApprovals: func() []ApprovalItem { return []ApprovalItem{{ID: "a1", AgentID: "linux", Kind: "file_write", Summary: "f.go", Diff: "@@ -1 +1 @@\n-x\n+y"}} },
		ResolveApproval:  func(string, string) {},
	})
	m2.width, m2.height = 120, 40
	m2.tab = tabApprovals
	m2.handleApprovalKey("d")
	if !m2.diffOpen || !strings.Contains(m2.diffVP.View(), "+y") {
		t.Fatal("'d' on an edit approval should open its diff in the modal")
	}
}

// /react targets an agent's most recent message (overall, or a named @agent) and forwards the seq
// + kind to Deps.React.
func TestReactCommand(t *testing.T) {
	j := journal.NewMemory()
	var gotSeq uint64
	var gotKind string
	m := NewModel(Deps{
		Journal: j,
		Inject:  func(string, string) {},
		React:   func(seq uint64, kind string) { gotSeq, gotKind = seq, kind },
	})
	rec := j.Append(journal.KindAgentEvent, "linux", agent.Event{Type: agent.EventText, Text: "done the thing"})
	m.apply(rec)

	m.input.SetValue("/react up")
	m.submit()
	if gotSeq != rec.Seq || gotKind != "up" {
		t.Fatalf("/react up should target seq %d/up, got %d/%q", rec.Seq, gotSeq, gotKind)
	}

	m.input.SetValue("/react reject @linux")
	m.submit()
	if gotSeq != rec.Seq || gotKind != "reject" {
		t.Fatalf("/react reject @linux should target seq %d/reject, got %d/%q", rec.Seq, gotSeq, gotKind)
	}

	// No recent message for an unknown agent → no call.
	gotSeq, gotKind = 0, ""
	m.input.SetValue("/react up @ghost")
	m.submit()
	if gotSeq != 0 || gotKind != "" {
		t.Fatalf("/react for an unknown agent should be a no-op, got %d/%q", gotSeq, gotKind)
	}
}

// Esc never quits; quitting takes two ctrl+c in succession (any other key disarms).
func TestQuitRequiresDoubleCtrlC(t *testing.T) {
	m, _, _ := newTestModel()

	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}); cmd != nil || m.quitArmed {
		t.Fatal("esc does not quit")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd != nil || !m.quitArmed {
		t.Fatalf("first ctrl+c should arm without quitting (cmd=%v)", cmd)
	}
	if m.Update(tea.KeyPressMsg{Code: tea.KeyTab}); m.quitArmed {
		t.Fatal("a different key should disarm the pending quit")
	}
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("two ctrl+c in succession should return a quit command")
	}
}

// Esc publishes an interrupt control signal on the bus (stop running agents).
func TestEscSendsInterrupt(t *testing.T) {
	m, _, j := newTestModel()
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	found := false
	for _, rec := range j.Records() {
		if msg, ok := rec.Data.(bus.Message); ok && msg.Interrupt {
			found = true
		}
	}
	if !found {
		t.Fatal("esc should publish an interrupt on the bus")
	}
}

// The recipient selector and the input @mention stay in sync both ways.
func TestRecipientSelector(t *testing.T) {
	m, _, _ := newTestModel()
	m.touchAgent("alice")
	m.touchAgent("bob")

	// input → selector
	m.input.SetValue("@bob hi")
	if got := m.selectedTarget(); got != "bob" {
		t.Fatalf("typing @bob → selectedTarget=%q", got)
	}
	m.input.SetValue("hello")
	if got := m.selectedTarget(); got != "broadcast" {
		t.Fatalf("plain text → selectedTarget=%q", got)
	}

	// selector → input (cycle: broadcast → team → facilitator → alice → bob), preserving the body
	m.cycleTarget(1)
	if got := m.input.Value(); got != "@team hello" {
		t.Fatalf("cycle 1 → %q", got)
	}
	if got := m.selectedTarget(); got != "team" {
		t.Fatalf("@team → selectedTarget=%q", got)
	}
	m.cycleTarget(1)
	if got := m.input.Value(); got != "@facilitator hello" {
		t.Fatalf("cycle 2 → %q", got)
	}
	m.cycleTarget(1)
	if got := m.input.Value(); got != "@alice hello" {
		t.Fatalf("cycle 3 → %q", got)
	}
	m.setTarget("broadcast")
	if got := m.input.Value(); got != "hello" {
		t.Fatalf("broadcast strips mention → %q", got)
	}
}

// Roster agents appear in the fleet at startup, before any message.
func TestRosterPopulatesFleet(t *testing.T) {
	j := journal.NewMemory()
	d := depsFor(bus.New(j), board.New(j), j)
	d.Roster = func() []string { return []string{"alice", "bob"} }
	m := NewModel(d)
	if m.agents["alice"] == nil || m.agents["bob"] == nil {
		t.Fatalf("roster agents should populate the fleet at startup: %v", m.agentOrder)
	}
	if m.agents["alice"].status != "idle" {
		t.Fatalf("roster agent should start idle, got %q", m.agents["alice"].status)
	}
}

// A multi-line agent message renders all its lines (not clipped to the first).
func TestMultilineAgentMessage(t *testing.T) {
	m, _, _ := newTestModel()
	m.height = 40 // plenty of room
	m.apply(journal.Record{Seq: 1, Kind: journal.KindAgentEvent, Actor: "claude",
		Data: agent.Event{Type: agent.EventText, Text: "Once approved I'll:\n- step one\n- step two"}})
	v := m.render()
	for _, want := range []string{"Once approved I'll:", "step one", "step two"} {
		if !strings.Contains(v, want) {
			t.Fatalf("render missing %q:\n%s", want, v)
		}
	}
}
