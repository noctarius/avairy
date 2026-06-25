package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"avairy/internal/workspace"
)

// Node is the client side of the channel: it enrolls with core, syncs a workspace directory,
// heartbeats, and proxies a local MCP endpoint to the core bus.
type Node struct {
	CoreURL string
	HTTP    *http.Client
	ID      string

	mu      sync.Mutex
	session string
	base    map[string]uint64
	stamps  workspace.Stamps // last-synced file stamps, to skip unchanged files
	ignore  workspace.Ignore
}

// NewNode returns a node client for the core control API at coreURL.
func NewNode(coreURL, id string) *Node {
	return &Node{
		CoreURL: strings.TrimRight(coreURL, "/"),
		HTTP:    http.DefaultClient,
		ID:      id,
		base:    make(map[string]uint64),
		stamps:  make(workspace.Stamps),
		ignore:  workspace.DefaultIgnore(),
	}
}

// Enroll joins the core using a one-time token and stores the session token. The node id
// (n.ID) is also the agent's bus identity.
func (n *Node) Enroll(token, os string, caps map[string]string) error {
	var resp EnrollResponse
	if err := n.post(PathEnroll, "", EnrollRequest{Token: token, NodeID: n.ID, OS: os, Caps: caps}, &resp); err != nil {
		return err
	}
	if resp.SessionToken == "" {
		return errors.New("control: enrollment returned no session token")
	}
	n.mu.Lock()
	n.session = resp.SessionToken
	n.mu.Unlock()
	return nil
}

// Heartbeat marks the node live at core.
func (n *Node) Heartbeat() error {
	return n.post(PathHeartbeat, n.sess(), HeartbeatRequest{NodeID: n.ID}, nil)
}

// SyncUp scans dir and pushes changes (and deletions) to the hub, advancing local base for
// accepted paths and returning any conflicts for reconciliation.
func (n *Node) SyncUp(dir string) ([]SyncResult, error) {
	// Only read/push files that actually changed since last sync (stat-based) — avoids
	// re-reading and re-uploading the whole tree every tick.
	changed, stampOf, seen, err := workspace.ScanChanges(dir, n.ignore, n.stamps)
	if err != nil {
		return nil, err
	}
	wire := make([]SyncChange, 0, len(changed))
	for _, c := range changed {
		wire = append(wire, SyncChange{Path: c.Path, Content: c.Content, Mode: uint32(c.Mode), Base: n.base[c.Path]})
	}
	for path, b := range n.base {
		if !seen[path] {
			wire = append(wire, SyncChange{Path: path, Deleted: true, Base: b})
		}
	}
	if len(wire) == 0 {
		return nil, nil // nothing changed → no round-trip
	}

	var resp PushResponse
	if err := n.post(PathPush, n.sess(), PushRequest{Changes: wire}, &resp); err != nil {
		return nil, err
	}
	var conflicts []SyncResult
	for _, r := range resp.Results {
		switch {
		case r.Applied:
			n.base[r.Path] = r.Version
			if st, ok := stampOf[r.Path]; ok {
				n.stamps[r.Path] = st
			} else {
				delete(n.stamps, r.Path) // a deletion
			}
		case r.Conflict:
			conflicts = append(conflicts, r)
		}
	}
	return conflicts, nil
}

// SyncDown pulls updates the node hasn't seen and applies them to dir.
func (n *Node) SyncDown(dir string) error {
	n.mu.Lock()
	base := make(map[string]uint64, len(n.base))
	for k, v := range n.base {
		base[k] = v
	}
	n.mu.Unlock()

	var resp PullResponse
	if err := n.post(PathPull, n.sess(), PullRequest{Base: base}, &resp); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, f := range resp.Files {
		if err := workspace.ApplyFile(dir, workspace.FileState{
			Path: f.Path, Content: f.Content, Mode: fs.FileMode(f.Mode), Version: f.Version, Deleted: f.Deleted,
		}); err != nil {
			return err
		}
		n.base[f.Path] = f.Version
		// Record the written file's stamp so the next SyncUp won't re-read/re-push it.
		if f.Deleted {
			delete(n.stamps, f.Path)
		} else if info, e := os.Stat(filepath.Join(dir, filepath.FromSlash(f.Path))); e == nil {
			n.stamps[f.Path] = workspace.FileStamp{Size: info.Size(), ModNano: info.ModTime().UnixNano()}
		}
	}
	return nil
}

// PullInbox fetches and clears bus messages buffered at core for agentID.
func (n *Node) PullInbox(agentID string) ([]InboxMessage, error) {
	var resp InboxPullResponse
	if err := n.post(PathInbox, n.sess(), InboxPullRequest{AgentID: agentID}, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// PostEvents ships an agent's stream events to the core journal (so they show in the TUI).
func (n *Node) PostEvents(events []AgentEventReport) error {
	if len(events) == 0 {
		return nil
	}
	return n.post(PathEvents, n.sess(), EventsRequest{Events: events}, nil)
}

// MCPProxy returns a handler that reverse-proxies the local MCP endpoint to the core bus,
// stamping the agent's bus identity. Agents on the node connect here (localhost); the core
// never sees the network topology (DESIGN.md §4).
func (n *Node) MCPProxy(coreBaseURL, agentID string) (http.Handler, error) {
	target, err := url.Parse(strings.TrimRight(coreBaseURL, "/"))
	if err != nil {
		return nil, err
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Header.Set("X-Avairy-Agent", agentID)
		},
	}
	return rp, nil
}

func (n *Node) sess() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.session
}

func (n *Node) post(path, session string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, n.CoreURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("Authorization", "Bearer "+session)
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("control %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
