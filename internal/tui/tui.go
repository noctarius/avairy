// Package tui is avairy's operator UI (DESIGN.md §3). It renders the event-sourced journal
// live — fleet/progress, conversation, and the handover timeline — and gives the human a
// command line to inject messages onto the bus (interrupt/steer). Single operator in v1.
package tui

import (
	"fmt"
	"runtime"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

// Deps are the live core services the TUI observes and drives.
type Deps struct {
	Bus     *bus.Bus
	Board   *board.Board
	Journal journal.Log
	Control *ControlInfo    // non-nil when serving the node control API
	Roster  func() []string // current agent ids, so agents appear before their first message

	// PendingApprovals/ResolveApproval drive the human-in-the-loop gating view (DESIGN.md §7):
	// agents block on a gated action, the operator allows/denies it here. Nil disables the view.
	PendingApprovals func() []ApprovalItem
	ResolveApproval  func(id, decision string)

	// PendingConflicts/ResolveConflict drive the Conflicts view (DESIGN.md §9, item #19): owner-less
	// conflicts (the operator's seed workspace diverging from a node's edit, or a git conflict) the
	// human resolves themselves or delegates to an agent. target is the agent id when delegating.
	// Nil disables the view.
	PendingConflicts func() []ConflictItem
	ResolveConflict  func(id, decision, target string)

	// Commit signs a commit of the canonical repo on the operator's behalf (DESIGN.md §9, the
	// human-commits-via-TUI path). Returns the new short hash. Nil when core has no git repo.
	Commit func(message string) (string, error)
}

// ApprovalItem is one pending gated action awaiting the operator's verdict.
type ApprovalItem struct {
	ID      string
	AgentID string
	Kind    string
	Summary string
	Reason  string
}

// ConflictItem is one owner-less conflict awaiting the operator (resolve themselves or delegate).
type ConflictItem struct {
	ID         string
	Path       string
	HubVersion uint64
	Source     string
	Detail     string
}

// Decision strings passed to Deps.ResolveApproval / Deps.ResolveConflict.
const (
	decisionAllow        = "allow"
	decisionDeny         = "deny"
	decisionAllowSession = "allow_for_session"
	decisionMine         = "mine"
	decisionDelegate     = "delegate"
)

// ControlInfo surfaces node-enrollment details in the TUI (the alt-screen hides stdout, so
// printing the token wouldn't be visible).
type ControlInfo struct {
	ControlURL   string
	BusBase      string
	Warn         string
	CurrentToken func() string // the operator-facing token for the next node (auto-regenerates on use)
	NewToken     func() string // rotate to a fresh token (ctrl+e)
	JoinFile     string        // path to the one-string join bundle (core URL + CA + token) for a new node
}

const (
	tabConversation = iota
	tabHandovers
	tabTasks
	tabApprovals
	tabConflicts
	numTabs
)

var tabNames = []string{"Conversation", "Handovers", "Tasks", "Approvals", "Conflicts"}

// inputHeight is the number of rows the multi-line command input occupies.
const inputHeight = 3

// newlineKey is the modifier+Enter combo labeled for the host OS (Option on macOS, Alt elsewhere).
var newlineKey = func() string {
	if runtime.GOOS == "darwin" {
		return "option+enter"
	}
	return "alt+enter"
}()

type agentState struct {
	id     string
	status string // working | idle | blocked
}

// Model is the Bubble Tea model.
type Model struct {
	deps Deps
	sub  <-chan journal.Record

	width, height int
	tab           int
	input         textarea.Model

	conv       []string
	handovers  []string
	agents     map[string]*agentState
	agentOrder []string
	cost       float64

	control     *ControlInfo
	token       string
	approvalSel int  // selected row in the Approvals view
	conflictSel int  // selected row in the Conflicts view
	quitArmed   bool // first ctrl+c arms; second (in succession) quits

	seen map[uint64]bool
}

// recordMsg carries a journal record into the Bubble Tea update loop.
type recordMsg journal.Record

// commitResultMsg carries the outcome of an operator-initiated /commit back into the loop.
type commitResultMsg struct {
	hash string
	err  error
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	activeTab  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Underline(true)
	dimTab     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	workingDot = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("●")
	idleDot    = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render("○")
	blockedDot = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("●")
	offlineDot = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("⊘")
	helpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	sepStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	ctrlStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
)

// NewModel builds the model, backfilling existing journal records and subscribing to new ones.
func NewModel(deps Deps) *Model {
	ta := textarea.New()
	ta.Placeholder = "message… (@<id> to address an agent; " + newlineKey + " for a newline)"
	ta.Prompt = "▎ "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0 // no limit (paste long, multi-line prompts)
	ta.SetHeight(inputHeight)
	ta.SetWidth(98)
	ta.Focus()

	sub, _ := deps.Journal.Subscribe()
	m := &Model{
		deps:    deps,
		sub:     sub,
		width:   100,
		height:  30,
		input:   ta,
		agents:  make(map[string]*agentState),
		control: deps.Control,
		seen:    make(map[uint64]bool),
	}
	if m.control != nil && m.control.CurrentToken != nil {
		m.token = m.control.CurrentToken()
	}
	for _, rec := range deps.Journal.Records() {
		m.apply(rec)
	}
	m.refreshRoster()
	return m
}

// refreshRoster ensures every currently-registered agent has a fleet entry, so agents show
// up as soon as they're known — not only after their first message.
func (m *Model) refreshRoster() {
	if m.deps.Roster == nil {
		return
	}
	for _, id := range m.deps.Roster() {
		m.touchAgent(id)
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.input.Focus(), listen(m.sub))
}

func listen(sub <-chan journal.Record) tea.Cmd {
	return func() tea.Msg {
		rec, ok := <-sub
		if !ok {
			return nil
		}
		return recordMsg(rec)
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(msg.Width - 2)
		return m, nil

	case recordMsg:
		m.apply(journal.Record(msg))
		m.refreshRoster() // pick up newly-enrolled agents (enrollment journals a record)
		return m, listen(m.sub)

	case commitResultMsg:
		if msg.err != nil {
			m.addConv(warnStyle.Render("⚠ commit failed: " + msg.err.Error()))
		} else {
			m.addConv("✓ committed " + msg.hash)
		}
		return m, nil

	case tea.KeyPressMsg:
		// Ctrl+C twice in succession quits; the first press just arms (hint in the footer).
		// Any other key disarms. Esc stops running agents; it no longer quits.
		s := msg.String()
		if s == "ctrl+c" {
			if m.quitArmed {
				return m, tea.Quit
			}
			m.quitArmed = true
			return m, nil
		}
		m.quitArmed = false
		// On the Approvals tab, navigation/verdict keys are consumed here (not typed into the
		// message input). Global keys (tab, esc, ctrl+*) fall through to the switch below.
		if m.tab == tabApprovals && m.handleApprovalKey(s) {
			return m, nil
		}
		if m.tab == tabConflicts && m.handleConflictKey(s) {
			return m, nil
		}
		switch s {
		case "esc":
			m.deps.Bus.Interrupt("human", bus.Broadcast()) // stop whatever agents are running
			return m, nil
		case "tab":
			m.tab = (m.tab + 1) % numTabs
			return m, nil
		case "ctrl+e":
			if m.control != nil && m.control.NewToken != nil {
				m.token = m.control.NewToken()
			}
			return m, nil
		case "ctrl+t":
			m.cycleTarget(1) // cycle the recipient (broadcast / agent)
			return m, nil
		case "shift+enter", "alt+enter", "ctrl+j": // newline (shift+enter needs a Kitty-protocol terminal)
			m.input.InsertRune('\n')
			return m, nil
		case "enter":
			cmd := m.submit()
			m.input.Reset()
			return m, cmd
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// submit handles the input line: a "/commit <msg>" slash command commits the canonical repo;
// anything else is published to the bus as a human injection. Returns a tea.Cmd for async work
// (the commit, which may block on signing), or nil.
func (m *Model) submit() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return nil
	}
	if rest, ok := strings.CutPrefix(text, "/commit"); ok {
		return m.commitCmd(strings.TrimSpace(rest))
	}
	if mention, body := splitMention(text); mention != "" && body != "" {
		m.deps.Bus.Publish("human", bus.Agent(mention), body, agent.DeliverySteer)
		return nil
	}
	m.deps.Bus.Publish("human", bus.Broadcast(), text, agent.DeliverySteer)
	return nil
}

// commitCmd runs an operator-initiated signed commit off the UI thread (signing can block).
func (m *Model) commitCmd(message string) tea.Cmd {
	if m.deps.Commit == nil {
		m.addConv(helpStyle.Render("(commit unavailable — core has no git repo)"))
		return nil
	}
	if message == "" {
		m.addConv(helpStyle.Render("usage: /commit <message>"))
		return nil
	}
	commit := m.deps.Commit
	return func() tea.Msg {
		hash, err := commit(message)
		return commitResultMsg{hash: hash, err: err}
	}
}

// splitMention separates a leading "@name" recipient from the rest of the text.
func splitMention(s string) (mention, rest string) {
	if !strings.HasPrefix(s, "@") {
		return "", s
	}
	body := s[1:]
	if i := strings.IndexByte(body, ' '); i >= 0 {
		return body[:i], strings.TrimLeft(body[i:], " ")
	}
	return body, ""
}

// --- recipient selector (the "dropdown") ---

// targets is the recipient list: broadcast, then the known agents (sorted).
func (m *Model) targets() []string {
	ag := append([]string(nil), m.agentOrder...)
	sort.Strings(ag)
	return append([]string{"broadcast"}, ag...)
}

// selectedTarget derives the current recipient from the input's @mention (broadcast if none
// or unknown) — this is what keeps the selector in sync when you type "@name".
func (m *Model) selectedTarget() string {
	if mention, _ := splitMention(m.input.Value()); mention != "" {
		for _, id := range m.agentOrder {
			if id == mention {
				return mention
			}
		}
	}
	return "broadcast"
}

// setTarget rewrites the input's recipient prefix to name (or strips it for broadcast).
func (m *Model) setTarget(name string) {
	_, rest := splitMention(m.input.Value())
	if name == "broadcast" {
		m.input.SetValue(rest)
		return
	}
	m.input.SetValue("@" + name + " " + rest)
}

// cycleTarget advances the recipient selection by dir (+1/-1).
func (m *Model) cycleTarget(dir int) {
	ts := m.targets()
	cur := m.selectedTarget()
	i := 0
	for idx, t := range ts {
		if t == cur {
			i = idx
			break
		}
	}
	m.setTarget(ts[(i+dir+len(ts))%len(ts)])
}

func (m *Model) selectorLine() string {
	sel := m.selectedTarget()
	parts := make([]string, 0, 4)
	for _, t := range m.targets() {
		if t == sel {
			parts = append(parts, activeTab.Render("‹"+t+"›"))
		} else {
			parts = append(parts, dimTab.Render(t))
		}
	}
	return helpStyle.Render("to ") + strings.Join(parts, " ")
}

// --- approvals (human-in-the-loop gating) ---

func (m *Model) pendingApprovals() []ApprovalItem {
	if m.deps.PendingApprovals == nil {
		return nil
	}
	return m.deps.PendingApprovals()
}

// handleApprovalKey processes a keystroke on the Approvals tab; it reports whether it consumed
// the key (so it isn't also typed into the message input).
func (m *Model) handleApprovalKey(s string) bool {
	pend := m.pendingApprovals()
	switch s {
	case "up", "k":
		if m.approvalSel > 0 {
			m.approvalSel--
		}
		return true
	case "down", "j":
		if m.approvalSel < len(pend)-1 {
			m.approvalSel++
		}
		return true
	case "y", "enter":
		m.resolveSelected(pend, decisionAllow)
		return true
	case "a":
		m.resolveSelected(pend, decisionAllowSession)
		return true
	case "n", "d":
		m.resolveSelected(pend, decisionDeny)
		return true
	}
	return false
}

func (m *Model) resolveSelected(pend []ApprovalItem, decision string) {
	if m.approvalSel < 0 || m.approvalSel >= len(pend) || m.deps.ResolveApproval == nil {
		return
	}
	m.deps.ResolveApproval(pend[m.approvalSel].ID, decision)
	if m.approvalSel > 0 {
		m.approvalSel-- // keep selection in range as the list shrinks
	}
}

// --- conflicts (owner-less file conflicts the operator resolves or delegates) ---

func (m *Model) pendingConflicts() []ConflictItem {
	if m.deps.PendingConflicts == nil {
		return nil
	}
	return m.deps.PendingConflicts()
}

// handleConflictKey processes a keystroke on the Conflicts tab; it reports whether it consumed
// the key. 'm' takes it on yourself (the file already holds markers — fix it in your editor); 'd'
// delegates it to the currently-selected recipient agent (ctrl+t to pick who).
func (m *Model) handleConflictKey(s string) bool {
	pend := m.pendingConflicts()
	switch s {
	case "up", "k":
		if m.conflictSel > 0 {
			m.conflictSel--
		}
		return true
	case "down", "j":
		if m.conflictSel < len(pend)-1 {
			m.conflictSel++
		}
		return true
	case "m", "enter":
		m.resolveConflictSelected(pend, decisionMine, "")
		return true
	case "d":
		m.resolveConflictSelected(pend, decisionDelegate, m.selectedTarget())
		return true
	}
	return false
}

func (m *Model) resolveConflictSelected(pend []ConflictItem, decision, target string) {
	if m.conflictSel < 0 || m.conflictSel >= len(pend) || m.deps.ResolveConflict == nil {
		return
	}
	m.deps.ResolveConflict(pend[m.conflictSel].ID, decision, target)
	if m.conflictSel > 0 {
		m.conflictSel-- // keep selection in range as the list shrinks
	}
}

// apply folds a journal record into view state.
func (m *Model) apply(rec journal.Record) {
	if m.seen[rec.Seq] {
		return
	}
	m.seen[rec.Seq] = true

	switch rec.Kind {
	case journal.KindMessage:
		if msg, ok := rec.Data.(bus.Message); ok {
			m.addConv(fmt.Sprintf("%s → %s: %s", msg.From, addrStr(msg.To), msg.Body))
		}
	case journal.KindAgentEvent:
		if ev, ok := rec.Data.(agent.Event); ok {
			a := m.touchAgent(rec.Actor)
			switch ev.Type {
			case agent.EventText:
				a.status = "working"
				m.addConv(fmt.Sprintf("%s: %s", rec.Actor, ev.Text))
			case agent.EventToolUse:
				a.status = "working"
				m.addConv(fmt.Sprintf("%s ⚙ %s", rec.Actor, agent.ToolSummary(ev.Tool)))
			case agent.EventTurnDone:
				a.status = "idle"
				if ev.Usage != nil {
					m.cost += ev.Usage.CostUSD
				}
			case agent.EventError:
				m.addConv(fmt.Sprintf("%s ⚠ %s", rec.Actor, ev.Text))
			}
		}
	case journal.KindHandover:
		if t, ok := rec.Data.(board.Task); ok {
			m.handovers = append(m.handovers, fmt.Sprintf("%s claimed %s — %q", t.Claimant, t.ID, t.Title))
		}
	case journal.KindSystem:
		d, ok := rec.Data.(map[string]any)
		if !ok {
			return
		}
		switch d["event"] {
		case "report_status":
			// Only agent self-reports affect the fleet (node-lifecycle records carry a node
			// id as actor, not an agent — they must not create a phantom fleet entry).
			if a := m.touchAgent(rec.Actor); a != nil {
				switch s, _ := d["status"].(string); s {
				case "blocked", "low_confidence":
					a.status = "blocked"
				case "done":
					a.status = "idle"
				default:
					a.status = "working"
				}
			}
		case "node_enrolled", "node_rejoined":
			// A node consumed the enrollment token → refresh the displayed (regenerated) one.
			if m.control != nil && m.control.CurrentToken != nil {
				m.token = m.control.CurrentToken()
			}
		case "node_offline":
			// Heartbeats lapsed (node id == agent id). Mark an existing fleet entry offline;
			// don't create one (avoid a phantom for a node that never had an agent).
			if a := m.agents[rec.Actor]; a != nil {
				a.status = "offline"
			}
		case "node_online":
			if a := m.agents[rec.Actor]; a != nil && a.status == "offline" {
				a.status = "idle" // back in contact; real status follows on its next event
			}
		}
	}
}

func (m *Model) addConv(line string) {
	m.conv = append(m.conv, line)
	if len(m.conv) > 500 {
		m.conv = m.conv[len(m.conv)-500:]
	}
}

func (m *Model) touchAgent(id string) *agentState {
	if id == "" || id == "human" {
		return nil
	}
	if a, ok := m.agents[id]; ok {
		return a
	}
	a := &agentState{id: id, status: "idle"}
	m.agents[id] = a
	m.agentOrder = append(m.agentOrder, id)
	return a
}

func addrStr(a bus.Addr) string {
	switch a.Kind {
	case bus.ToBroadcast:
		return "all"
	default:
		return string(a.Kind) + ":" + a.Value
	}
}

func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	// Request enhanced keyboard reporting (Kitty protocol) so shift+enter is distinguishable
	// where the terminal supports it.
	v.KeyboardEnhancements = tea.KeyboardEnhancements{ReportAlternateKeys: true}
	return v
}

func (m *Model) render() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("avairy") + helpStyle.Render("  single-machine collaboration"))
	b.WriteString("\n")

	controlLines := 0
	if m.control != nil {
		line := fmt.Sprintf("control %s · bus %s · enroll token: %s", m.control.ControlURL, m.control.BusBase, m.token)
		if m.control.JoinFile != "" {
			line += " · join: " + m.control.JoinFile
		}
		b.WriteString(ctrlStyle.Render(truncate(line, m.width)) + "\n")
		controlLines++
		if m.control.Warn != "" {
			b.WriteString(warnStyle.Render(truncate("⚠ "+m.control.Warn, m.width)) + "\n")
			controlLines++
		}
	}

	b.WriteString(m.tabBar() + "\n")
	b.WriteString(m.fleetLine() + "\n")
	b.WriteString(sep(m.width) + "\n")

	// Flatten entries into visual lines — an agent message can be multi-line, and the row
	// budget must count actual rows (else a multi-line message overflows and gets clipped).
	var lines []string
	for _, e := range m.bodyLines() {
		lines = append(lines, strings.Split(e, "\n")...)
	}
	// Reserve rows: title + tabs + fleet + 2 seps + selector + help (+1 slack) + control + input.
	avail := max(m.height-8-controlLines-inputHeight, 1)
	if len(lines) > avail {
		lines = lines[len(lines)-avail:]
	}
	for _, l := range lines {
		b.WriteString(truncate(l, m.width) + "\n")
	}
	for i := len(lines); i < avail; i++ {
		b.WriteString("\n")
	}

	b.WriteString(sep(m.width) + "\n")
	b.WriteString(m.selectorLine() + "\n")
	b.WriteString(m.input.View() + "\n")
	if m.quitArmed {
		b.WriteString(warnStyle.Render("press ctrl+c again to quit"))
	} else {
		help := "tab: view · ctrl+t: recipient · enter: send · " + newlineKey + ": newline · esc: stop · ctrl+c ×2: quit"
		if m.tab == tabApprovals {
			help = "tab: view · ↑/↓ (j/k): select · y: allow · a: allow kind this session · n: deny · esc: stop · ctrl+c ×2: quit"
		}
		if m.tab == tabConflicts {
			help = "tab: view · ↑/↓ (j/k): select · m: I'll resolve · d: delegate to ‹" + m.selectedTarget() + "› (ctrl+t) · ctrl+c ×2: quit"
		}
		if m.control != nil {
			help += " · ctrl+e: new enroll token"
		}
		b.WriteString(helpStyle.Render(help))
	}
	return b.String()
}

func (m *Model) tabBar() string {
	parts := make([]string, numTabs)
	for i, name := range tabNames {
		if i == tabApprovals {
			if n := len(m.pendingApprovals()); n > 0 {
				name = fmt.Sprintf("%s (%d)", name, n)
			}
		}
		if i == tabConflicts {
			if n := len(m.pendingConflicts()); n > 0 {
				name = fmt.Sprintf("%s (%d)", name, n)
			}
		}
		if i == m.tab {
			parts[i] = activeTab.Render(name)
		} else {
			parts[i] = dimTab.Render(name)
		}
	}
	return strings.Join(parts, dimTab.Render("  |  "))
}

func (m *Model) fleetLine() string {
	if len(m.agentOrder) == 0 {
		return helpStyle.Render("fleet: (no agents yet)") + fmt.Sprintf("   cost $%.2f", m.cost)
	}
	var parts []string
	for _, id := range m.agentOrder {
		a := m.agents[id]
		dot := idleDot
		switch a.status {
		case "working":
			dot = workingDot
		case "blocked":
			dot = blockedDot
		case "offline":
			dot = offlineDot
		}
		parts = append(parts, fmt.Sprintf("%s %s[%s]", dot, id, a.status))
	}
	return "fleet: " + strings.Join(parts, "  ") + fmt.Sprintf("   cost $%.2f", m.cost)
}

func (m *Model) bodyLines() []string {
	switch m.tab {
	case tabHandovers:
		if len(m.handovers) == 0 {
			return []string{helpStyle.Render("(no handovers yet)")}
		}
		return m.handovers
	case tabApprovals:
		pend := m.pendingApprovals()
		if len(pend) == 0 {
			return []string{helpStyle.Render("(no pending approvals — gated actions appear here for allow/deny)")}
		}
		if m.approvalSel >= len(pend) {
			m.approvalSel = len(pend) - 1
		}
		out := make([]string, 0, len(pend))
		for i, ap := range pend {
			marker := "  "
			if i == m.approvalSel {
				marker = activeTab.Render("▸ ")
			}
			line := fmt.Sprintf("%s%s wants [%s]: %s", marker, ap.AgentID, ap.Kind, ap.Summary)
			if ap.Reason != "" {
				line += helpStyle.Render("  — " + ap.Reason)
			}
			out = append(out, line)
		}
		return out
	case tabConflicts:
		pend := m.pendingConflicts()
		if len(pend) == 0 {
			return []string{helpStyle.Render("(no conflicts — seed/git conflicts with no owning agent appear here to resolve or delegate)")}
		}
		if m.conflictSel >= len(pend) {
			m.conflictSel = len(pend) - 1
		}
		out := make([]string, 0, len(pend))
		for i, cf := range pend {
			marker := "  "
			if i == m.conflictSel {
				marker = activeTab.Render("▸ ")
			}
			line := fmt.Sprintf("%s%s (hub v%d, %s) — has git-style markers; edit to resolve", marker, cf.Path, cf.HubVersion, cf.Source)
			if cf.Detail != "" {
				line += helpStyle.Render("  — " + cf.Detail)
			}
			out = append(out, line)
		}
		return out
	case tabTasks:
		tasks := m.deps.Board.List()
		if len(tasks) == 0 {
			return []string{helpStyle.Render("(no tasks yet)")}
		}
		out := make([]string, 0, len(tasks))
		for _, t := range tasks {
			claim := t.Claimant
			if claim == "" {
				claim = "-"
			}
			out = append(out, fmt.Sprintf("%s [%s] %q  requires=%v  claimant=%s", t.ID, t.State, t.Title, t.Requires, claim))
		}
		return out
	default:
		if len(m.conv) == 0 {
			return []string{helpStyle.Render("(no messages yet — type below to inject)")}
		}
		return m.conv
	}
}

func sep(w int) string {
	if w < 1 {
		w = 1
	}
	return sepStyle.Render(strings.Repeat("─", w))
}

func truncate(s string, w int) string {
	if w <= 0 || lipgloss.Width(s) <= w {
		return s
	}
	return s[:max(0, w-1)] + "…"
}

// Run starts the TUI against the live core services. It blocks until the user quits.
// (Alt-screen is requested per-View in v2; see View().)
func Run(deps Deps) error {
	p := tea.NewProgram(NewModel(deps))
	_, err := p.Run()
	return err
}
