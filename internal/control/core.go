package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"avairy/internal/agent"
	"avairy/internal/journal"
	"avairy/internal/workspace"
)

// NodeInfo is the core's record of an enrolled node (its ID is also the agent's bus identity).
type NodeInfo struct {
	ID       string
	OS       string
	Caps     map[string]string
	LastSeen time.Time
	Live     bool // false once heartbeats lapse past LivenessTimeout (see RunLiveness)
}

// Core is the server side of the node↔core channel: enrollment, node registry, and
// workspace sync against the canonical hub.
type Core struct {
	hub  *workspace.Hub
	jrnl journal.Log

	// OnEnroll, if set, runs when a node enrolls — used to register it on the bus (the node id
	// is the agent's bus identity).
	OnEnroll func(nodeID string, caps map[string]string)
	// InboxDrainer, if set, returns and clears bus messages buffered for an agent.
	InboxDrainer func(agentID string) []InboxMessage
	// Approvals routes a node agent's gated actions to the operator (DESIGN.md §7). NewCore
	// installs a default broker; share one across local agents + the TUI by replacing it.
	Approvals *Approvals
	// LivenessTimeout is how long a node may go without contact before it's marked offline.
	// Must exceed the node's heartbeat interval (default 2s); NewCore defaults it to 15s.
	LivenessTimeout time.Duration
	// RequireClientCert disables token enrollment: a node must present a verified mTLS client cert
	// (its node id in the SAN). Set with -mtls-only — once every node authenticates by certificate,
	// the shared enrollment token is just a weaker credential to leak, so lock it off.
	RequireClientCert bool
	// OnConflict, if set, routes a rejected (divergent) push to the responsible agent for
	// reconciliation (DESIGN.md §9). It carries BOTH sides — the hub's current content and the
	// agent's rejected edit — because the node's SyncDown will overwrite the local file with the
	// hub version before the agent acts, so the message is the agent's only copy of its own edit.
	// Deduped per (agent, path, hub version) so re-pushing a stale edit doesn't re-notify each tick.
	OnConflict func(agentID, path string, hubVersion uint64, hubContent, yourContent []byte)
	// Bundle, if set, returns a git bundle of the canonical repo excluding the commit shas the
	// node already has (incremental; DESIGN.md §9). (nil, nil) means the node is already current.
	// Nodes pull this to build a local read-only mirror. nil field → /repo/bundle 404s.
	Bundle func(ctx context.Context, have []string) ([]byte, error)

	mu        sync.Mutex
	conflicts map[string]uint64 // agent\x00path -> last-notified hub version
	pending   string            // current operator-facing token (hand to the next node)
	bound     map[string]string // enrollment token -> node id it's bound to (reusable for rejoin)
	sessions  map[string]string // sessionToken -> nodeID
	nodes     map[string]*NodeInfo
}

// NewCore returns a Core backed by hub, journaling lifecycle events to jrnl.
func NewCore(hub *workspace.Hub, jrnl journal.Log) *Core {
	return &Core{
		hub:             hub,
		jrnl:            jrnl,
		Approvals:       NewApprovals(0),
		LivenessTimeout: 15 * time.Second,
		bound:           make(map[string]string),
		sessions:        make(map[string]string),
		nodes:           make(map[string]*NodeInfo),
		conflicts:       make(map[string]uint64),
	}
}

// CurrentToken returns the operator-facing enrollment token for the next node (minting one if
// needed). It stays stable until a new node consumes it (then it auto-regenerates).
func (c *Core) CurrentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == "" {
		c.pending = randToken()
	}
	return c.pending
}

// NewPendingToken rotates the operator-facing token (manual rotation; invalidates the old
// unused one). Already-bound node tokens are unaffected.
func (c *Core) NewPendingToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = randToken()
	return c.pending
}

// Nodes returns a snapshot of enrolled nodes (for the TUI fleet/health view).
func (c *Core) Nodes() []NodeInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]NodeInfo, 0, len(c.nodes))
	for _, n := range c.nodes {
		out = append(out, *n)
	}
	return out
}

// Handler mounts the control API.
func (c *Core) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PathEnroll, c.handleEnroll)
	mux.Handle(PathHeartbeat, c.auth(c.handleHeartbeat))
	mux.Handle(PathPush, c.auth(c.handlePush))
	mux.Handle(PathPull, c.auth(c.handlePull))
	mux.Handle(PathInbox, c.auth(c.handleInbox))
	mux.Handle(PathEvents, c.auth(c.handleEvents))
	mux.Handle(PathApprove, c.auth(c.handleApprove))
	mux.Handle(PathBundle, c.auth(c.handleBundle))
	return mux
}

// handleBundle streams an (incremental) git bundle of the canonical repo to an enrolled node as
// raw bytes. The request body lists the shas the node already has; 204 means nothing new.
func (c *Core) handleBundle(nodeID string, w http.ResponseWriter, r *http.Request) {
	var req BundleRequest
	if !readJSON(w, r, &req) {
		return
	}
	c.touch(nodeID)
	if c.Bundle == nil {
		http.Error(w, "no canonical repo on core", http.StatusNotFound)
		return
	}
	b, err := c.Bundle(r.Context(), req.Have)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(b) == 0 {
		w.WriteHeader(http.StatusNoContent) // node already current
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(b)
}

// handleApprove blocks until the operator rules on a node agent's gated action (or it times
// out / the node disconnects → deny). The request surfaces in the operator TUI via the broker.
func (c *Core) handleApprove(nodeID string, w http.ResponseWriter, r *http.Request) {
	var req ApprovalRequest
	if !readJSON(w, r, &req) {
		return
	}
	c.touch(nodeID)
	if req.AgentID == "" {
		req.AgentID = nodeID
	}
	decision := c.Approvals.Ask(r.Context(), Approval{
		AgentID: req.AgentID, Kind: req.Kind, Summary: req.Summary, Reason: req.Reason,
	})
	writeJSON(w, ApprovalResponse{Decision: decision})
}

func (c *Core) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req EnrollRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.NodeID == "" {
		http.Error(w, "invalid enrollment: missing node id", http.StatusUnauthorized)
		return
	}
	// mTLS path: a verified client cert authenticates the node by its SAN, no token needed.
	certID := verifiedNodeID(r)
	if c.RequireClientCert && certID == "" {
		http.Error(w, "invalid enrollment: client certificate required (token enrollment is disabled)", http.StatusUnauthorized)
		return
	}
	if certID == "" && req.Token == "" {
		http.Error(w, "invalid enrollment: token or client certificate required", http.StatusUnauthorized)
		return
	}
	if certID != "" && certID != req.NodeID {
		http.Error(w, "client certificate identity does not match node id", http.StatusUnauthorized)
		return
	}

	c.mu.Lock()
	accepted, rejoin := false, false
	switch {
	case certID != "":
		// Authenticated by client cert: accept; rejoin if we've seen this node before.
		_, known := c.nodes[req.NodeID]
		accepted, rejoin = true, known
	case req.Token == c.pending:
		// First use: bind the token to this node and regenerate the operator-facing token.
		c.bound[req.Token] = req.NodeID
		c.pending = randToken()
		accepted = true
	case c.bound[req.Token] == req.NodeID:
		// The same node re-enrolling with its bound token — a rejoin (restart/crash recovery).
		accepted, rejoin = true, true
	}
	session := randToken()
	if accepted {
		c.sessions[session] = req.NodeID
		c.nodes[req.NodeID] = &NodeInfo{ID: req.NodeID, OS: req.OS, Caps: req.Caps, LastSeen: time.Now(), Live: true}
	}
	c.mu.Unlock()
	if !accepted {
		http.Error(w, "invalid enrollment token (unknown, or bound to another node)", http.StatusUnauthorized)
		return
	}

	event := "node_enrolled"
	if rejoin {
		event = "node_rejoined"
	}
	// Register the agent on the bus BEFORE journaling the enrollment: the journal record wakes the
	// operator's roster refresh, so if it ran first the refresh could read an empty roster (and not
	// re-run until the agent later emits an event) — leaving an enrolled node invisible in the fleet.
	if c.OnEnroll != nil {
		c.OnEnroll(req.NodeID, req.Caps)
	}
	c.jrnl.Append(journal.KindSystem, req.NodeID, map[string]any{"event": event, "os": req.OS, "caps": req.Caps})
	writeJSON(w, EnrollResponse{SessionToken: session})
}

// verifiedNodeID returns the node id from a verified client cert (mTLS), or "" if none was
// presented. VerifiedChains is non-empty only when the TLS layer verified the cert against the
// configured ClientCAs, so trusting it here is safe.
func verifiedNodeID(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return ""
	}
	return nodeIDFromCert(r.TLS.VerifiedChains[0][0])
}

func (c *Core) handleInbox(nodeID string, w http.ResponseWriter, r *http.Request) {
	var req InboxPullRequest
	if !readJSON(w, r, &req) {
		return
	}
	c.touch(nodeID)
	var msgs []InboxMessage
	if c.InboxDrainer != nil {
		msgs = c.InboxDrainer(req.AgentID)
	}
	writeJSON(w, InboxPullResponse{Messages: msgs})
}

func (c *Core) handleEvents(nodeID string, w http.ResponseWriter, r *http.Request) {
	var req EventsRequest
	if !readJSON(w, r, &req) {
		return
	}
	c.touch(nodeID)
	for _, e := range req.Events {
		ev := agent.Event{Type: agent.EventType(e.Type), Text: e.Text}
		if e.Tool != "" {
			ev.Tool = &agent.ToolCall{Name: e.Tool, Input: e.ToolInput}
		}
		if e.CostUSD != 0 {
			ev.Usage = &agent.Usage{CostUSD: e.CostUSD}
		}
		c.jrnl.Append(journal.KindAgentEvent, e.AgentID, ev)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (c *Core) handleHeartbeat(nodeID string, w http.ResponseWriter, r *http.Request) {
	c.touch(nodeID)
	writeJSON(w, map[string]bool{"ok": true})
}

func (c *Core) handlePush(nodeID string, w http.ResponseWriter, r *http.Request) {
	var req PushRequest
	if !readJSON(w, r, &req) {
		return
	}
	c.touch(nodeID)
	results := make([]SyncResult, 0, len(req.Changes))
	for _, ch := range req.Changes {
		res := c.hub.Push(nodeID, workspace.Change{
			Path:    ch.Path,
			Content: ch.Content,
			Mode:    fs.FileMode(ch.Mode),
			Deleted: ch.Deleted,
			Base:    ch.Base,
		})
		sr := SyncResult{Path: ch.Path, Applied: res.Applied, Version: res.Version}
		if res.Conflict != nil {
			sr.Conflict = true
			sr.HubVersion = res.Conflict.Hub.Version
			sr.HubContent = res.Conflict.Hub.Content
			c.jrnl.Append(journal.KindSystem, nodeID, map[string]any{"event": "sync_conflict", "path": ch.Path})
			// Route to the agent for reconciliation — once per hub version, not every tick.
			if c.OnConflict != nil && c.newConflict(nodeID, ch.Path, res.Conflict.Hub.Version) {
				c.OnConflict(nodeID, ch.Path, res.Conflict.Hub.Version, res.Conflict.Hub.Content, res.Conflict.Incoming.Content)
			}
		}
		results = append(results, sr)
	}
	writeJSON(w, PushResponse{Results: results})
}

func (c *Core) handlePull(nodeID string, w http.ResponseWriter, r *http.Request) {
	var req PullRequest
	if !readJSON(w, r, &req) {
		return
	}
	c.touch(nodeID)
	files := c.hub.Pull(req.Base)
	out := make([]PullFile, 0, len(files))
	for _, f := range files {
		out = append(out, PullFile{
			Path:    f.Path,
			Content: f.Content,
			Mode:    uint32(f.Mode),
			Version: f.Version,
			Deleted: f.Deleted,
		})
	}
	writeJSON(w, PullResponse{Files: out})
}

func (c *Core) auth(h func(nodeID string, w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		c.mu.Lock()
		nodeID, ok := c.sessions[tok]
		c.mu.Unlock()
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(nodeID, w, r)
	})
}

// newConflict reports whether (agent, path) hasn't already been notified at this hub version,
// recording it so repeated pushes of the same stale edit don't re-notify until the hub moves.
func (c *Core) newConflict(agentID, path string, hubVersion uint64) bool {
	key := agentID + "\x00" + path
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conflicts[key] == hubVersion {
		return false
	}
	c.conflicts[key] = hubVersion
	return true
}

func (c *Core) touch(nodeID string) {
	c.mu.Lock()
	if n := c.nodes[nodeID]; n != nil {
		n.LastSeen = time.Now()
	}
	c.mu.Unlock()
}

// RunLiveness marks nodes offline when their heartbeats lapse (and online again when they
// return), journaling each transition so the operator TUI reflects it. Blocks until ctx is
// cancelled. Node id == agent id, so an offline node shows its agent as offline in the fleet.
func (c *Core) RunLiveness(ctx context.Context) {
	tick := c.LivenessTimeout / 3
	if tick < time.Second {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweepLiveness()
		}
	}
}

// sweepLiveness flips each node's Live flag based on LastSeen and returns the transitions to
// journal (done outside the lock so journal subscribers can't deadlock on c.mu).
func (c *Core) sweepLiveness() {
	now := time.Now()
	type change struct {
		id   string
		live bool
	}
	var changes []change
	c.mu.Lock()
	for id, n := range c.nodes {
		live := now.Sub(n.LastSeen) < c.LivenessTimeout
		if live != n.Live {
			n.Live = live
			changes = append(changes, change{id, live})
		}
	}
	c.mu.Unlock()
	for _, ch := range changes {
		event := "node_offline"
		if ch.live {
			event = "node_online"
		}
		c.jrnl.Append(journal.KindSystem, ch.id, map[string]any{"event": event})
	}
}

func randToken() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
