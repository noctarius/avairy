package control

import (
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

// NodeInfo is the core's record of an enrolled node.
type NodeInfo struct {
	ID       string
	AgentID  string
	OS       string
	Caps     map[string]string
	LastSeen time.Time
}

// Core is the server side of the node↔core channel: enrollment, node registry, and
// workspace sync against the canonical hub.
type Core struct {
	hub  *workspace.Hub
	jrnl journal.Log

	// OnEnroll, if set, runs when a node enrolls — used to register its agent on the bus.
	OnEnroll func(nodeID, agentID string, caps map[string]string)
	// InboxDrainer, if set, returns and clears bus messages buffered for an agent.
	InboxDrainer func(agentID string) []InboxMessage

	mu           sync.Mutex
	enrollTokens map[string]bool   // valid one-time enrollment tokens
	sessions     map[string]string // sessionToken -> nodeID
	nodes        map[string]*NodeInfo
}

// NewCore returns a Core backed by hub, journaling lifecycle events to jrnl.
func NewCore(hub *workspace.Hub, jrnl journal.Log) *Core {
	return &Core{
		hub:          hub,
		jrnl:         jrnl,
		enrollTokens: make(map[string]bool),
		sessions:     make(map[string]string),
		nodes:        make(map[string]*NodeInfo),
	}
}

// IssueEnrollToken mints a single-use enrollment token (shown in the TUI / seeded over SSH).
func (c *Core) IssueEnrollToken() string {
	tok := randToken()
	c.mu.Lock()
	c.enrollTokens[tok] = true
	c.mu.Unlock()
	return tok
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
	return mux
}

func (c *Core) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req EnrollRequest
	if !readJSON(w, r, &req) {
		return
	}
	c.mu.Lock()
	ok := c.enrollTokens[req.Token]
	if ok {
		delete(c.enrollTokens, req.Token) // single-use
	}
	c.mu.Unlock()
	if !ok || req.NodeID == "" {
		http.Error(w, "invalid enrollment token", http.StatusUnauthorized)
		return
	}

	session := randToken()
	c.mu.Lock()
	c.sessions[session] = req.NodeID
	c.nodes[req.NodeID] = &NodeInfo{ID: req.NodeID, AgentID: req.AgentID, OS: req.OS, Caps: req.Caps, LastSeen: time.Now()}
	c.mu.Unlock()

	c.jrnl.Append(journal.KindSystem, req.NodeID, map[string]any{"event": "node_enrolled", "os": req.OS, "caps": req.Caps})
	if c.OnEnroll != nil {
		c.OnEnroll(req.NodeID, req.AgentID, req.Caps)
	}
	writeJSON(w, EnrollResponse{SessionToken: session})
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
			ev.Tool = &agent.ToolCall{Name: e.Tool}
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

func (c *Core) touch(nodeID string) {
	c.mu.Lock()
	if n := c.nodes[nodeID]; n != nil {
		n.LastSeen = time.Now()
	}
	c.mu.Unlock()
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
