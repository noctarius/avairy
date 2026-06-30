// Package control is the node↔core wire protocol and daemon plumbing (DESIGN.md §3/§4/§9):
// token enrollment, workspace sync over the channel, a local MCP proxy, and heartbeats.
// The channel here is plain HTTP for the MVP; production flips it to TLS (node→core
// outbound, NAT-friendly). The node is a single cross-platform Go binary (cmd/avairy-node).
package control

// Wire paths on the core control API.
const (
	PathEnroll    = "/enroll"
	PathHeartbeat = "/heartbeat"
	PathPush      = "/sync/push"
	PathPull      = "/sync/pull"
	PathInbox     = "/inbox/pull"
	PathEvents    = "/events"
	PathApprove   = "/approve"
	PathBundle    = "/repo/bundle"
	PathManifest  = "/sync/manifest"
)

// ApprovalRequest asks core to route a gated action to the human operator (DESIGN.md §7). The
// node blocks on the response; core holds the request open until the operator rules on it.
type ApprovalRequest struct {
	AgentID string `json:"agentId"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	Reason  string `json:"reason,omitempty"`
	Diff    string `json:"diff,omitempty"` // unified diff for a file edit, for the operator to review
}

// ApprovalResponse carries the operator's verdict (DecisionAllow | DecisionDeny).
type ApprovalResponse struct {
	Decision string `json:"decision"`
}

// BundleRequest asks core for a repo bundle, listing the commit shas the node already has so
// core can ship an incremental bundle (DESIGN.md §9). Empty Have → a full bundle.
type BundleRequest struct {
	Have []string `json:"have,omitempty"`
}

// EnrollRequest is sent by a node to join, authenticated by a one-time enrollment token
// (SSH bootstrap seeds it, or the operator pastes it for manual/Windows provisioning). A node
// is 1:1 with the agent it hosts, so NodeID is also the agent's bus identity (run multiple
// node processes for multiple agents).
type EnrollRequest struct {
	Token  string            `json:"token"`
	NodeID string            `json:"nodeId"`
	OS     string            `json:"os"`
	Caps   map[string]string `json:"caps"`
}

// InboxMessage is a bus message addressed to a node's agent, delivered over the channel.
type InboxMessage struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	Body      string `json:"body"`
	Delivery  string `json:"delivery"`
	Interrupt bool   `json:"interrupt,omitempty"` // cancel the agent's current turn
	NoWake    bool   `json:"noWake,omitempty"`    // context-only: read via read_inbox but doesn't trigger a turn
	ToKind    string `json:"toKind,omitempty"`    // "agent" | "role" | "broadcast" — for the node's wake policy (#25)
}

// InboxPullRequest asks core for messages buffered for agentId.
type InboxPullRequest struct {
	AgentID string `json:"agentId"`
}

// InboxPullResponse returns the drained messages.
type InboxPullResponse struct {
	Messages []InboxMessage `json:"messages"`
}

// Pseudo-event types a node reports over the events channel to signal an agent's idle-teardown
// lifecycle (#28); core translates these into agent_sleeping/agent_awake system events rather than
// journaling them as agent stream events. Distinct from any agent.EventType.
const (
	EventAgentSleeping = "sleeping"
	EventAgentAwake    = "awake"
)

// AgentEventReport is a normalized agent stream event shipped to the core journal so a
// remote agent's activity shows in the operator TUI.
type AgentEventReport struct {
	AgentID   string         `json:"agentId"`
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	Tool      string         `json:"tool,omitempty"`
	ToolInput map[string]any `json:"toolInput,omitempty"` // trimmed (agent.TrimInput) — keeps command/file_path, drops bodies
	CostUSD   float64        `json:"costUsd,omitempty"`
}

// EventsRequest carries a batch of agent events.
type EventsRequest struct {
	Events []AgentEventReport `json:"events"`
}

// EnrollResponse returns a per-node session token used for all subsequent calls.
type EnrollResponse struct {
	SessionToken string `json:"sessionToken"`
}

// HeartbeatRequest keeps a node marked live and reports the node's currently-conflicted paths
// (marker-locked + startup-held) so core can answer the agent's list_conflicts without a grep (#22).
type HeartbeatRequest struct {
	NodeID string `json:"nodeId"`
	// Conflicts is the node's current conflicted paths; always sent (even empty) so resolving the
	// last one clears core's view. nil only from an older node that doesn't report — then core keeps
	// its prior value.
	Conflicts []string `json:"conflicts"`
}

// HeartbeatResponse carries any operator directive for the node — currently the verdict on a held
// startup conflict (item #21): "resync" (checksum-manifest reconcile) or "resolve" (write markers,
// reconcile as usual). Empty when there's nothing to do. Delivered (and cleared) on heartbeat.
type HeartbeatResponse struct {
	Directive string `json:"directive,omitempty"`
	// Unlock lists paths the operator/agent resolved via resolve_conflict (#22): the node drops its
	// lock so the next SyncDown lands the merged canonical content (clearing the stale markers).
	Unlock []string `json:"unlock,omitempty"`
	// Consults are spawn/close commands for operator-targeted ephemeral consult agents on this node
	// (#24): the node opens/closes them and wires each to the bus under its own id.
	Consults []ConsultCommand `json:"consults,omitempty"`
}

// ConsultCommand tells a node to open or close an ephemeral consult agent (#24). For "open", Family
// selects the agent family (empty = the node's default). ID is the bus identity (e.g. consult-linux).
type ConsultCommand struct {
	ID     string `json:"id"`
	Action string `json:"action"` // "open" | "close"
	Family string `json:"family,omitempty"`
}

// SyncChange is a node's proposed change to one path (content is base64 via []byte).
type SyncChange struct {
	Path    string `json:"path"`
	Content []byte `json:"content,omitempty"`
	Mode    uint32 `json:"mode"`
	Deleted bool   `json:"deleted,omitempty"`
	Base    uint64 `json:"base"`
}

// PushRequest carries a batch of changes to the canonical hub. FirstSync marks a node's very first
// push after (re)start: conflicts then are owner-less startup conflicts routed to the operator's
// choice (resync/resolve, item #21) rather than auto-marked for the agent.
type PushRequest struct {
	Changes   []SyncChange `json:"changes"`
	FirstSync bool         `json:"firstSync,omitempty"`
}

// SyncResult reports the outcome of one pushed change. On Conflict, HubVersion/HubContent
// expose the canonical side for agent reconciliation (DESIGN.md §9).
type SyncResult struct {
	Path       string `json:"path"`
	Applied    bool   `json:"applied"`
	Version    uint64 `json:"version"`
	Conflict   bool   `json:"conflict"`
	HubVersion uint64 `json:"hubVersion,omitempty"`
	HubContent []byte `json:"hubContent,omitempty"`
}

// PushResponse returns a result per change.
type PushResponse struct {
	Results []SyncResult `json:"results"`
}

// PullRequest sends the node's known per-path versions; core returns what's newer.
type PullRequest struct {
	Base map[string]uint64 `json:"base"`
}

// PullFile is one file the node hasn't seen yet (or a deletion).
type PullFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content,omitempty"`
	Mode    uint32 `json:"mode"`
	Version uint64 `json:"version"`
	Deleted bool   `json:"deleted,omitempty"`
}

// PullResponse carries the updates.
type PullResponse struct {
	Files []PullFile `json:"files"`
}

// ManifestEntry is one path's canonical fingerprint (checksum + version + age) — see
// workspace.ManifestEntry. The node diffs it against its local tree to resync only the delta (#21).
type ManifestEntry struct {
	Path     string `json:"path"`
	Checksum uint64 `json:"checksum"`
	Version  uint64 `json:"version"`
	Modified string `json:"modified"` // RFC3339; "" if unknown (pre-timestamp version)
}

// ManifestResponse is the full canonical fingerprint set.
type ManifestResponse struct {
	Files []ManifestEntry `json:"files"`
}
