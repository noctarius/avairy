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
)

// EnrollRequest is sent by a node to join, authenticated by a one-time enrollment token
// (SSH bootstrap seeds it, or the operator pastes it for manual/Windows provisioning).
type EnrollRequest struct {
	Token  string            `json:"token"`
	NodeID string            `json:"nodeId"`
	OS     string            `json:"os"`
	Caps   map[string]string `json:"caps"`
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
