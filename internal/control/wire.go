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
)

// ApprovalRequest asks core to route a gated action to the human operator (DESIGN.md §7). The
// node blocks on the response; core holds the request open until the operator rules on it.
type ApprovalRequest struct {
	AgentID string `json:"agentId"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	Reason  string `json:"reason,omitempty"`
}

// ApprovalResponse carries the operator's verdict (DecisionAllow | DecisionDeny).
type ApprovalResponse struct {
	Decision string `json:"decision"`
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
}

// InboxPullRequest asks core for messages buffered for agentId.
type InboxPullRequest struct {
	AgentID string `json:"agentId"`
}

// InboxPullResponse returns the drained messages.
type InboxPullResponse struct {
	Messages []InboxMessage `json:"messages"`
}

// AgentEventReport is a normalized agent stream event shipped to the core journal so a
// remote agent's activity shows in the operator TUI.
type AgentEventReport struct {
	AgentID string  `json:"agentId"`
	Type    string  `json:"type"`
	Text    string  `json:"text,omitempty"`
	Tool    string  `json:"tool,omitempty"`
	CostUSD float64 `json:"costUsd,omitempty"`
}

// EventsRequest carries a batch of agent events.
type EventsRequest struct {
	Events []AgentEventReport `json:"events"`
}

// EnrollResponse returns a per-node session token used for all subsequent calls.
type EnrollResponse struct {
	SessionToken string `json:"sessionToken"`
}

// HeartbeatRequest keeps a node marked live.
type HeartbeatRequest struct {
	NodeID string `json:"nodeId"`
}

// SyncChange is a node's proposed change to one path (content is base64 via []byte).
type SyncChange struct {
	Path    string `json:"path"`
	Content []byte `json:"content,omitempty"`
	Mode    uint32 `json:"mode"`
	Deleted bool   `json:"deleted,omitempty"`
	Base    uint64 `json:"base"`
}

// PushRequest carries a batch of changes to the canonical hub.
type PushRequest struct {
	Changes []SyncChange `json:"changes"`
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
