package operator

import (
	"fmt"
	"strings"

	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/control"
	"avairy/internal/journal"
	"avairy/internal/tui"
)

// Services is core's live operator surface: the event journal plus the brokers/bus an operator
// drives. One value yields both the in-process TUI deps (Deps) and the remote HTTP API (NewServer),
// so the action logic — injection, approvals, conflict delegation, commits — lives in one place.
type Services struct {
	Journal   journal.Log
	Roster    func() []string
	Tasks     func() []board.Task
	Notes     func() []board.Note // blackboard read view (#27); nil → empty
	Approvals *control.Approvals
	Conflicts *control.Conflicts
	Bus       *bus.Bus
	Commit    func(string) (string, error) // nil when core has no git repo
	Control   func() *ControlState         // nil when not serving the control API
	NewToken  func() string                // rotate the enrollment token; nil when no control API
	// NodeDirective delivers the operator's verdict on a node's held startup conflict (#21) back to
	// that node (resync/resolve). nil when there's no control API.
	NodeDirective func(nodeID, decision string)
	// Consult / CloseConsult spawn and tear down operator-requested ephemeral consult agents (#24):
	// Consult returns the new agent's bus id; CloseConsult reports whether one was torn down. nil
	// disables the feature.
	Consult      func(target, family string) (string, error)
	CloseConsult func(id string) bool
}

// Inject publishes a human message: target "" broadcasts, else it's an agent id. A leading "@<id> "
// in the body addresses that agent and overrides target — so typing "@macos …" reaches macos even
// when the recipient selector is on broadcast (the web composer relies on this; the TUI also sends
// a clean target, for which the parse is a no-op).
func (s *Services) Inject(target, body string) {
	if mention, rest := splitAddressedMention(body); mention != "" && rest != "" {
		target, body = mention, rest
	}
	s.Bus.Publish("human", addrOf(target), body, agent.DeliverySteer)
}

// splitAddressedMention extracts a leading "@<id> " address from a message, but ONLY when there's a
// real body after it: a bare "@id" returns ("", s) so it isn't swallowed as an empty injection.
// (The TUI's own splitMention differs — it returns the id even for a bare "@id" — because there it
// drives the recipient selector, not a publish.)
func splitAddressedMention(s string) (mention, rest string) {
	if !strings.HasPrefix(s, "@") {
		return "", s
	}
	body := s[1:]
	if i := strings.IndexByte(body, ' '); i >= 0 {
		return body[:i], strings.TrimLeft(body[i:], " ")
	}
	return "", s
}

// Interrupt stops whatever agents are running (broadcast interrupt).
func (s *Services) Interrupt() { s.Bus.Interrupt("human", bus.Broadcast()) }

// Reaction kinds an operator attaches to an agent message (the quick-feedback control).
const (
	ReactUp     = "up"     // 👍 positive — delivered as context, no interrupt
	ReactDown   = "down"   // 👎 negative — delivered as context, no interrupt
	ReactReject = "reject" // ❌ hard stop + reconsider — interrupt the agent and steer it to rethink
)

// ReactWindow is how many of an agent's most recent text messages stay reactable. Older messages
// keep any badge they already have but can no longer be reacted to — reacting to something an agent
// has long moved past would just deliver stale feedback. The consoles mirror this for the buttons.
const ReactWindow = 5

// React records the operator's 👍/👎/❌ on the agent message at journal seq and acts on it: 👍/👎
// reach the agent as context-only feedback (seen on its next turn, never interrupting); ❌
// interrupts the agent now and steers it to reconsider that step. The reaction is journaled so both
// consoles can render the badge and it survives a reload. Only the agent's last ReactWindow text
// messages are reactable; anything older is ignored.
func (s *Services) React(seq uint64, kind string) {
	agentID, snippet, ok := s.reactable(seq)
	if !ok {
		return // unknown seq, not an agent message, or too old to react to
	}
	s.Journal.Append(journal.KindSystem, "human", map[string]any{
		"event": "reaction", "seq": seq, "kind": kind, "agent": agentID,
	})
	to := bus.Agent(agentID)
	switch kind {
	case ReactUp:
		s.Bus.PublishContext("human", to, "👍 the operator approved this step: \""+snippet+"\"")
	case ReactDown:
		s.Bus.PublishContext("human", to, "👎 the operator flagged this step as off-track: \""+snippet+"\" — reconsider it on your next step (no need to stop now).")
	case ReactReject:
		s.Bus.Interrupt("human", to)
		s.Bus.Publish("human", to, "✗ the operator REJECTED this step: \""+snippet+"\". Stop this approach now and reconsider before continuing.", agent.DeliverySteer)
	}
}

// reactable reports the agent and a short quote for the message at seq, and whether it's currently
// reactable — i.e. among that agent's last ReactWindow text messages (tool actions don't count).
// This both finds the target and enforces the recency limit, so a stale client can't react to an
// old message and deliver confusing late feedback.
func (s *Services) reactable(seq uint64) (agentID, snippet string, ok bool) {
	type entry struct {
		seq  uint64
		text string
	}
	byAgent := map[string][]entry{}
	for _, r := range s.Journal.Records() {
		var who, text string
		switch r.Kind {
		case journal.KindAgentEvent:
			if ev, k := r.Data.(agent.Event); k && ev.Type == agent.EventText {
				who, text = r.Actor, ev.Text
			}
		case journal.KindMessage:
			// an agent's own utterance (send_message) — not the human, facilitator, or a
			// control/reaction-delivery message.
			if m, k := r.Data.(bus.Message); k && m.From != "human" && m.From != "facilitator" && !m.Interrupt && !m.NoWake {
				who, text = m.From, m.Body
			}
		}
		if who != "" {
			byAgent[who] = append(byAgent[who], entry{r.Seq, text})
		}
	}
	for who, es := range byAgent {
		start := len(es) - ReactWindow
		if start < 0 {
			start = 0
		}
		for _, e := range es[start:] {
			if e.seq == seq {
				return who, snip(e.text), true
			}
		}
	}
	return "", "", false
}

func snip(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
}

// ResolveApproval delivers the operator's verdict on a gated action to the waiting agent.
func (s *Services) ResolveApproval(id, decision string) { s.Approvals.Resolve(id, decision) }

// ResolveConflict clears an owner-less conflict; on "delegate" it steers the chosen agent to fix
// the markers (target "" / "broadcast" → broadcast the request).
func (s *Services) ResolveConflict(id, decision, target string) {
	oc, ok := s.Conflicts.Resolve(id, decision)
	if !ok {
		return
	}
	// Node startup conflict (#21): the verdict (resync/resolve) goes back to the node, not the bus.
	if oc.Source == "node-startup" {
		if s.NodeDirective != nil {
			s.NodeDirective(oc.Path, decision) // oc.Path carries the node id
		}
		return
	}
	if decision != control.ConflictDelegate {
		return // seed "mine": the operator edits the marked file; the next seed sync picks it up
	}
	s.Bus.Publish("human", addrOf(target), delegateBody(oc), agent.DeliverySteer)
}

// state assembles the snapshot the client reads while rendering.
func (s *Services) state() State {
	st := State{Roster: callOrNil(s.Roster)}
	if s.Tasks != nil {
		st.Tasks = s.Tasks()
	}
	if s.Notes != nil {
		st.Notes = s.Notes()
	}
	for _, p := range s.Approvals.Pending() {
		st.Approvals = append(st.Approvals, ApprovalItem{ID: p.ID, AgentID: p.AgentID, Kind: p.Kind, Summary: p.Summary, Reason: p.Reason, Diff: p.Diff})
	}
	for _, c := range s.Conflicts.Pending() {
		st.Conflicts = append(st.Conflicts, ConflictItem{ID: c.ID, Path: c.Path, HubVersion: c.HubVersion, Source: c.Source, Detail: c.Detail})
	}
	if s.Control != nil {
		st.Control = s.Control()
	}
	return st
}

// Deps builds the in-process TUI deps from these services (the attached-TUI run mode).
func (s *Services) Deps() tui.Deps {
	d := tui.Deps{
		Journal:         s.Journal,
		Roster:          s.Roster,
		Tasks:           s.Tasks,
		Notes:           s.Notes,
		Inject:          s.Inject,
		Interrupt:       s.Interrupt,
		React:           s.React,
		ResolveApproval: s.ResolveApproval,
		ResolveConflict: s.ResolveConflict,
		Consult:         s.Consult,
		CloseConsult:    s.CloseConsult,
		Commit:          s.Commit,
		PendingApprovals: func() []tui.ApprovalItem {
			ps := s.Approvals.Pending()
			out := make([]tui.ApprovalItem, 0, len(ps))
			for _, p := range ps {
				out = append(out, tui.ApprovalItem{ID: p.ID, AgentID: p.AgentID, Kind: p.Kind, Summary: p.Summary, Reason: p.Reason, Diff: p.Diff})
			}
			return out
		},
		PendingConflicts: func() []tui.ConflictItem {
			cs := s.Conflicts.Pending()
			out := make([]tui.ConflictItem, 0, len(cs))
			for _, c := range cs {
				out = append(out, tui.ConflictItem{ID: c.ID, Path: c.Path, HubVersion: c.HubVersion, Source: c.Source, Detail: c.Detail})
			}
			return out
		},
	}
	if s.Control != nil {
		if cs := s.Control(); cs != nil {
			d.Control = &tui.ControlInfo{
				ControlURL:   cs.ControlURL,
				BusBase:      cs.BusBase,
				Warn:         cs.Warn,
				JoinFile:     cs.JoinFile,
				OperatorJoin: cs.OperatorJoin,
				WebURL:       cs.WebURL,
				MTLSOnly:     cs.MTLSOnly,
				CurrentToken: func() string { return s.Control().Token },
				NewToken:     s.NewToken,
			}
		}
	}
	return d
}

// delegateBody is the steer message handed to an agent asked to resolve a conflict (shared by the
// in-process and remote paths so the wording stays identical).
func delegateBody(oc control.OperatorConflict) string {
	return fmt.Sprintf("Please resolve the conflict in %s (hub v%d). The file has git-style conflict markers (<<<<<<< / ======= / >>>>>>>); edit it to merge both sides and remove the markers, then call resolve_conflict (or just save it marker-free).", oc.Path, oc.HubVersion)
}

// addrOf maps an operator target string to a bus address ("" or "broadcast" → broadcast).
func addrOf(target string) bus.Addr {
	switch target {
	case "", "broadcast", "all":
		return bus.Broadcast()
	case "team":
		return bus.Team() // one agent claims it and answers (claim_response); the rest stand down
	case "facilitator":
		return bus.Facilitator() // triage: pick/assign one agent
	}
	return bus.Agent(target)
}

func callOrNil(f func() []string) []string {
	if f == nil {
		return nil
	}
	return f()
}
