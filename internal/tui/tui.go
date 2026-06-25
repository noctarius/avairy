// Package tui is avairy's operator UI (DESIGN.md §3). It renders the event-sourced journal
// live — fleet/progress, conversation, and the handover timeline — and gives the human a
// command line to inject messages onto the bus (interrupt/steer). Single operator in v1.
package tui

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	Control *ControlInfo // non-nil when serving the node control API
}

// ControlInfo surfaces node-enrollment details in the TUI (the alt-screen hides stdout, so
// printing the token wouldn't be visible).
type ControlInfo struct {
	ControlURL string
	BusBase    string
	Warn       string
	NewToken   func() string // mints a fresh single-use enrollment token
}

const (
	tabConversation = iota
	tabHandovers
	tabTasks
	numTabs
)

var tabNames = []string{"Conversation", "Handovers", "Tasks"}

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

	control   *ControlInfo
	token     string
	quitArmed bool // first ctrl+c arms; second (in succession) quits

	seen map[uint64]bool
}

// recordMsg carries a journal record into the Bubble Tea update loop.
type recordMsg journal.Record

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	activeTab  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Underline(true)
	dimTab     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	workingDot = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("●")
	idleDot    = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render("○")
	blockedDot = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("●")
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
	if m.control != nil && m.control.NewToken != nil {
		m.token = m.control.NewToken()
	}
	for _, rec := range deps.Journal.Records() {
		m.apply(rec)
	}
	return m
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
		return m, listen(m.sub)

	case tea.KeyMsg:
		// Ctrl+C twice in succession quits; the first press just arms (hint shown in the
		// footer). Any other key disarms. Esc no longer quits.
		if msg.Type == tea.KeyCtrlC {
			if m.quitArmed {
				return m, tea.Quit
			}
			m.quitArmed = true
			return m, nil
		}
		m.quitArmed = false
		switch msg.Type {
		case tea.KeyEsc:
			m.deps.Bus.Interrupt("human", bus.Broadcast()) // stop whatever agents are running
			return m, nil
		case tea.KeyTab:
			m.tab = (m.tab + 1) % numTabs
			return m, nil
		case tea.KeyCtrlE:
			if m.control != nil && m.control.NewToken != nil {
				m.token = m.control.NewToken()
			}
			return m, nil
		case tea.KeyCtrlJ:
			m.input.InsertRune('\n') // newline fallback for any terminal
			return m, nil
		case tea.KeyEnter:
			if msg.Alt { // alt/option+enter → newline (Shift+Enter isn't distinguishable here)
				m.input.InsertRune('\n')
				return m, nil
			}
			m.submit()
			m.input.Reset()
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// submit publishes the input line to the bus as a human injection.
func (m *Model) submit() {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return
	}
	if rest, ok := strings.CutPrefix(text, "@"); ok {
		id, body, _ := strings.Cut(rest, " ")
		if id != "" && body != "" {
			m.deps.Bus.Publish("human", bus.Agent(id), body, agent.DeliverySteer)
			return
		}
	}
	m.deps.Bus.Publish("human", bus.Broadcast(), text, agent.DeliverySteer)
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
				name := ""
				if ev.Tool != nil {
					name = ev.Tool.Name
				}
				m.addConv(fmt.Sprintf("%s ⚙ %s", rec.Actor, name))
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
		// Only agent self-reports (report_status) affect the fleet. Node-lifecycle records
		// (node_enrolled, sync_conflict, …) carry a node id as actor — not an agent — so they
		// must not create a phantom fleet entry.
		if d, ok := rec.Data.(map[string]any); ok && d["event"] == "report_status" {
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

func (m *Model) View() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("avairy") + helpStyle.Render("  single-machine collaboration"))
	b.WriteString("\n")

	controlLines := 0
	if m.control != nil {
		line := fmt.Sprintf("control %s · bus %s · enroll token: %s", m.control.ControlURL, m.control.BusBase, m.token)
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

	body := m.bodyLines()
	// Reserve rows: title + tabs + fleet + 2 seps + help (+1 slack) + control + multi-line input.
	avail := max(m.height-7-controlLines-inputHeight, 1)
	if len(body) > avail {
		body = body[len(body)-avail:]
	}
	for _, l := range body {
		b.WriteString(truncate(l, m.width) + "\n")
	}
	for i := len(body); i < avail; i++ {
		b.WriteString("\n")
	}

	b.WriteString(sep(m.width) + "\n")
	b.WriteString(m.input.View() + "\n")
	if m.quitArmed {
		b.WriteString(warnStyle.Render("press ctrl+c again to quit"))
	} else {
		help := "tab: switch view · @<id>: address agent · enter: send · " + newlineKey + ": newline · esc: stop agents · ctrl+c ×2: quit"
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
func Run(deps Deps) error {
	p := tea.NewProgram(NewModel(deps), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
