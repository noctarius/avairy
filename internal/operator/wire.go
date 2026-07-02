// Package operator is the seam that lets the TUI (and, later, the web UI #17) run either in-process
// on core or attached from another machine (DESIGN.md §3, item #18). A Services value wraps core's
// live operator surface — the event journal plus the actions an operator can take — and yields both
// an in-process tui.Deps (Services.Deps) and an HTTP API (NewServer). A Client dials that API from
// remote and yields an equivalent tui.Deps, so the TUI can't tell local from remote.
package operator

import (
	"encoding/json"

	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

// HTTP routes of the operator API (mounted under the control listener, reusing its TLS + addr).
const (
	PathStream      = "/operator/stream"      // GET: SSE of journal records (backfill, then live)
	PathState       = "/operator/state"       // GET: a snapshot of tasks/approvals/conflicts/roster/control
	PathInject      = "/operator/inject"      // POST: publish a human message
	PathReact       = "/operator/react"       // POST: 👍/👎/❌ on an agent message (by journal seq)
	PathInterrupt   = "/operator/interrupt"   // POST: stop running agents
	PathApproval    = "/operator/approval"    // POST: resolve a gated action
	PathConflict    = "/operator/conflict"    // POST: resolve/delegate an owner-less conflict
	PathCommit      = "/operator/commit"      // POST: signed commit of the canonical repo
	PathToken       = "/operator/token"       // POST: rotate the node-enrollment token
	PathUI          = "/operator/ui"          // GET: the browser operator console (#17)
	PathConsult     = "/operator/consult"     // POST: spawn an ephemeral consult agent (#24)
	PathClose       = "/operator/close"       // POST: close an ephemeral consult agent (#24)
	PathReconfigure = "/operator/reconfigure" // POST: change an agent's model/effort
)

// readyEvent is the SSE event name core sends once the journal backfill is fully streamed, so the
// client knows history is complete and can start refreshing its state cache on later records.
const readyEvent = "ready"

// ApprovalItem mirrors tui.ApprovalItem on the wire (kept here so operator doesn't depend on tui's
// view types for transport — the client maps these into tui.ApprovalItem).
type ApprovalItem struct {
	ID      string `json:"id"`
	AgentID string `json:"agentId"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	Reason  string `json:"reason,omitempty"`
	Diff    string `json:"diff,omitempty"` // unified diff for a file edit, shown behind "Show Diff"
}

// ConflictItem mirrors tui.ConflictItem on the wire.
type ConflictItem struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	HubVersion uint64 `json:"hubVersion"`
	Source     string `json:"source"`
	Detail     string `json:"detail,omitempty"`
}

// ControlState is the operator-facing control info (enrollment token + endpoints) over the wire.
type ControlState struct {
	ControlURL   string `json:"controlUrl"`
	BusBase      string `json:"busBase"`
	Warn         string `json:"warn,omitempty"`
	Token        string `json:"token"`
	JoinFile     string `json:"joinFile,omitempty"`
	OperatorJoin string `json:"operatorJoin,omitempty"`
	WebURL       string `json:"webUrl,omitempty"` // browser console URL (with token), #17
	MTLSOnly     bool   `json:"mtlsOnly,omitempty"`
}

// State is the snapshot the client polls/refreshes (everything the TUI reads synchronously while
// rendering — the journal stream carries the rest).
type State struct {
	Tasks     []board.Task   `json:"tasks"`
	Notes     []board.Note   `json:"notes"` // blackboard — durable shared memory (#27)
	Approvals []ApprovalItem `json:"approvals"`
	Conflicts []ConflictItem `json:"conflicts"`
	Roster    []string       `json:"roster"`
	Control   *ControlState  `json:"control,omitempty"`
}

// Action request/response bodies.
type (
	injectRequest struct{ Target, Body string }
	reactRequest  struct {
		Seq  uint64 `json:"seq"`
		Kind string `json:"kind"`
	}
	approvalDecision struct{ ID, Decision string }
	conflictDecision struct{ ID, Decision, Target string }
	commitRequest    struct{ Message string }
	commitResponse   struct {
		Hash  string `json:"hash"`
		Error string `json:"error,omitempty"`
	}
	tokenResponse struct {
		Token string `json:"token"`
	}
	reconfigureRequest struct {
		Agent  string `json:"agent"`
		Model  string `json:"model,omitempty"`
		Effort string `json:"effort,omitempty"`
	}
	consultRequest  struct{ Target, Family string }
	consultResponse struct {
		ID    string `json:"id"`
		Error string `json:"error,omitempty"`
	}
	closeRequest struct{ ID string }
)

// encodeRecord renders a journal record for the wire as a PersistedRecord (Data as raw JSON), the
// same shape the on-disk journal uses — so decodeRecord can re-type it on the client.
func encodeRecord(rec journal.Record) journal.PersistedRecord {
	raw, _ := json.Marshal(rec.Data)
	return journal.PersistedRecord{Seq: rec.Seq, Time: rec.Time, Kind: rec.Kind, Actor: rec.Actor, Data: raw}
}

// decodeRecord re-types a wire record's payload to the concrete type the TUI's apply() expects
// (bus.Message / agent.Event / board.Task / map[string]any), preserving the original Seq so callers
// can dedup the backfill/stream overlap. Reports false if the payload didn't decode.
func decodeRecord(pr journal.PersistedRecord) (journal.Record, bool) {
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
	default: // system / approval / note — map payloads
		var mm map[string]any
		if json.Unmarshal(pr.Data, &mm) == nil {
			data = mm
		}
	}
	if data == nil {
		return journal.Record{}, false
	}
	return journal.Record{Seq: pr.Seq, Time: pr.Time, Kind: pr.Kind, Actor: pr.Actor, Data: data}, true
}
