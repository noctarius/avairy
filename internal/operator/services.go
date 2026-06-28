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
	if mention, rest := splitMention(body); mention != "" && rest != "" {
		target, body = mention, rest
	}
	s.Bus.Publish("human", addrOf(target), body, agent.DeliverySteer)
}

// splitMention extracts a leading "@<id> " address from a message. "@id" with no following text is
// not a mention (returns "", s) so a bare mention isn't swallowed.
func splitMention(s string) (mention, rest string) {
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
		st.Approvals = append(st.Approvals, ApprovalItem{ID: p.ID, AgentID: p.AgentID, Kind: p.Kind, Summary: p.Summary, Reason: p.Reason})
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
		ResolveApproval: s.ResolveApproval,
		ResolveConflict: s.ResolveConflict,
		Consult:         s.Consult,
		CloseConsult:    s.CloseConsult,
		Commit:          s.Commit,
		PendingApprovals: func() []tui.ApprovalItem {
			ps := s.Approvals.Pending()
			out := make([]tui.ApprovalItem, 0, len(ps))
			for _, p := range ps {
				out = append(out, tui.ApprovalItem{ID: p.ID, AgentID: p.AgentID, Kind: p.Kind, Summary: p.Summary, Reason: p.Reason})
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
