package operator

import (
	"fmt"

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
	Approvals *control.Approvals
	Conflicts *control.Conflicts
	Bus       *bus.Bus
	Commit    func(string) (string, error) // nil when core has no git repo
	Control   func() *ControlState         // nil when not serving the control API
	NewToken  func() string                // rotate the enrollment token; nil when no control API
}

// Inject publishes a human message: target "" broadcasts, else it's an agent id.
func (s *Services) Inject(target, body string) {
	s.Bus.Publish("human", addrOf(target), body, agent.DeliverySteer)
}

// Interrupt stops whatever agents are running (broadcast interrupt).
func (s *Services) Interrupt() { s.Bus.Interrupt("human", bus.Broadcast()) }

// ResolveApproval delivers the operator's verdict on a gated action to the waiting agent.
func (s *Services) ResolveApproval(id, decision string) { s.Approvals.Resolve(id, decision) }

// ResolveConflict clears an owner-less conflict; on "delegate" it steers the chosen agent to fix
// the markers (target "" / "broadcast" → broadcast the request).
func (s *Services) ResolveConflict(id, decision, target string) {
	oc, ok := s.Conflicts.Resolve(id, decision)
	if !ok || decision != control.ConflictDelegate {
		return // "mine": the operator edits the marked file; the next seed sync picks it up
	}
	s.Bus.Publish("human", addrOf(target), delegateBody(oc), agent.DeliverySteer)
}

// state assembles the snapshot the client reads while rendering.
func (s *Services) state() State {
	st := State{Roster: callOrNil(s.Roster)}
	if s.Tasks != nil {
		st.Tasks = s.Tasks()
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
		Inject:          s.Inject,
		Interrupt:       s.Interrupt,
		ResolveApproval: s.ResolveApproval,
		ResolveConflict: s.ResolveConflict,
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
	if target == "" || target == "broadcast" {
		return bus.Broadcast()
	}
	return bus.Agent(target)
}

func callOrNil(f func() []string) []string {
	if f == nil {
		return nil
	}
	return f()
}
