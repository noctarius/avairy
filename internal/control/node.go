package control

import (
	"bytes"
	"context"
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
	// ReenrollOnExpiry makes the node re-enroll automatically on a 401 (e.g. after a core
	// restart drops its session). Enable only for mTLS nodes: cert auth is stateless on core
	// (needs just the persisted CA), so re-enroll succeeds without the lost bound-token map.
	ReenrollOnExpiry bool

	mu          sync.Mutex
	session     string
	enrollOS    string
	enrollToken string
	enrollCaps  map[string]string
	base        map[string]uint64
	stamps      workspace.Stamps // last-synced file stamps, to skip unchanged files
	ignore      workspace.Ignore
	conflicts   map[string]bool // paths holding unresolved conflict markers (locked from sync)

	reMu sync.Mutex // serializes re-enrollment so concurrent 401s don't stampede
}

// NewNode returns a node client for the core control API at coreURL.
func NewNode(coreURL, id string) *Node {
	return &Node{
		CoreURL:   strings.TrimRight(coreURL, "/"),
		HTTP:      http.DefaultClient,
		ID:        id,
		base:      make(map[string]uint64),
		stamps:    make(workspace.Stamps),
		ignore:    workspace.DefaultIgnore(),
		conflicts: make(map[string]bool),
	}
}

// Enroll joins the core (one-time token, or an mTLS client cert presented by n.HTTP) and stores
// the session token. The credentials are remembered so the node can re-enroll later. The node
// id (n.ID) is also the agent's bus identity.
func (n *Node) Enroll(token, os string, caps map[string]string) error {
	n.mu.Lock()
	n.enrollToken, n.enrollOS, n.enrollCaps = token, os, caps
	n.mu.Unlock()
	return n.enroll(context.Background())
}

// enroll performs (or repeats) enrollment with the remembered credentials. It calls doPost
// directly so a 401 here doesn't recurse into re-enrollment.
func (n *Node) enroll(ctx context.Context) error {
	n.mu.Lock()
	req := EnrollRequest{Token: n.enrollToken, NodeID: n.ID, OS: n.enrollOS, Caps: n.enrollCaps}
	n.mu.Unlock()
	var resp EnrollResponse
	if _, err := n.doPost(ctx, PathEnroll, "", req, &resp); err != nil {
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
	changed, stampOf, seen, err := workspace.ScanChanges(dir, workspace.IgnoreFor(dir), n.stamps)
	if err != nil {
		return nil, err
	}
	wire := make([]SyncChange, 0, len(changed))
	changedSet := make(map[string]bool, len(changed))
	for _, c := range changed {
		changedSet[c.Path] = true
		// A path holding unresolved conflict markers is LOCKED: don't push it (that would land
		// the markers in the hub). When the agent edits it marker-free, it's resolved → unlock
		// and push from the adopted base so it lands as the next version.
		if workspace.HasConflictMarkers(c.Content) {
			n.conflicts[c.Path] = true
			n.stamps[c.Path] = stampOf[c.Path]
			continue
		}
		delete(n.conflicts, c.Path)
		wire = append(wire, SyncChange{Path: c.Path, Content: c.Content, Mode: uint32(c.Mode), Base: n.base[c.Path]})
	}
	// Files read but unchanged in content (metadata moved only): refresh their stamp now so we
	// don't re-read them — and never push identical content (no fsnotify ping-pong).
	for path, st := range stampOf {
		if !changedSet[path] {
			n.stamps[path] = st
		}
	}
	for path, b := range n.base {
		if !seen[path] && !n.conflicts[path] { // a conflicted (held) file isn't a deletion
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
			// Write 3-way markers into the local file (the agent's edit is the "ours" side, so
			// nothing is lost), adopt the hub version as base, and lock the path until resolved.
			n.markConflict(dir, r)
			conflicts = append(conflicts, r)
		}
	}
	return conflicts, nil
}

// markConflict writes git-style conflict markers into a rejected file so the agent resolves it
// in place, adopts the hub version as base, and locks the path from further sync until resolved.
func (n *Node) markConflict(dir string, r SyncResult) {
	full := filepath.Join(dir, filepath.FromSlash(r.Path))
	local, err := os.ReadFile(full)
	if err != nil {
		return // file vanished; nothing to mark
	}
	marked := workspace.MergeMarkers(local, r.HubContent, r.HubVersion)
	if err := os.WriteFile(full, marked, 0o644); err != nil {
		return
	}
	n.conflicts[r.Path] = true
	n.base[r.Path] = r.HubVersion // resolved edit will push from here → HubVersion+1
	if info, e := os.Stat(full); e == nil {
		n.stamps[r.Path] = workspace.FileStamp{Size: info.Size(), ModNano: info.ModTime().UnixNano(), Hash: workspace.HashContent(marked)}
	}
}

// ResumeFromHub primes the node's base versions against core before the first sync, so a node
// whose workspace is already populated (a pre-existing checkout, or a restart — base is in-memory
// and lost across restarts) doesn't push every file from base 0 and collide with the canonical hub.
// For each hub file that also exists locally it adopts the hub's version as the base: an unchanged
// local file then pushes idempotently (no conflict), a locally-edited one pushes from the correct
// base (a real edit, not a phantom new-file conflict), and hub files absent locally are left for
// SyncDown to fetch. Mirrors workspace.NodeView.ResumeFromHub (the core seed's resume). Call once
// after Enroll, before the sync loop.
func (n *Node) ResumeFromHub(dir string) error {
	var resp PullResponse
	if err := n.post(PathPull, n.sess(), PullRequest{Base: map[string]uint64{}}, &resp); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, f := range resp.Files {
		if f.Deleted {
			continue
		}
		full := filepath.Join(dir, filepath.FromSlash(f.Path))
		if _, err := os.Stat(full); err == nil {
			n.base[f.Path] = f.Version // adopt; don't write (SyncUp's push is idempotent for equal content)
		}
	}
	return nil
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
		if n.conflicts[f.Path] {
			continue // LOCKED: the agent is resolving conflict markers here — don't clobber it
		}
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
			// Hash too: a later touch trips the cheap gate, but the hash match then proves the
			// content is unchanged so we don't re-push our own write (no fsnotify ping-pong).
			n.stamps[f.Path] = workspace.FileStamp{Size: info.Size(), ModNano: info.ModTime().UnixNano(), Hash: workspace.HashContent(f.Content)}
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

// RequestApproval routes a gated action to the operator at core and blocks for the verdict.
// Core holds the request open until the human rules (or it times out), so this call can take
// a while — the caller's ctx (e.g. the hook's timeout) bounds the wait. Returns the decision
// string (DecisionAllow | DecisionDeny).
func (n *Node) RequestApproval(ctx context.Context, req ApprovalRequest) (string, error) {
	var resp ApprovalResponse
	if err := n.postCtx(ctx, PathApprove, n.sess(), req, &resp); err != nil {
		return "", err
	}
	return resp.Decision, nil
}

// PullBundle fetches an incremental git bundle of the canonical repo (raw bytes) for the node's
// read-only mirror, telling core which commit shas it already has. Returns (nil, nil) when the
// node is already current (HTTP 204), or an error if core has no repo (404) / the call fails.
func (n *Node) PullBundle(ctx context.Context, have []string) ([]byte, error) {
	body, err := json.Marshal(BundleRequest{Have: have})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.CoreURL+PathBundle, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s := n.sess(); s != "" {
		req.Header.Set("Authorization", "Bearer "+s)
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return io.ReadAll(resp.Body)
	case http.StatusNoContent:
		return nil, nil // already current
	default:
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("control %s: %s: %s", PathBundle, resp.Status, strings.TrimSpace(string(msg)))
	}
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
	// Reuse the node's TLS-trusting transport so an https core bus is verified against the same
	// CA as the control channel (from the join / -ca). nil transport → default (plain http).
	if n.HTTP != nil {
		rp.Transport = n.HTTP.Transport
	}
	return rp, nil
}

func (n *Node) sess() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.session
}

func (n *Node) post(path, session string, body, out any) error {
	return n.postCtx(context.Background(), path, session, body, out)
}

// postCtx posts to core and, on a 401 for an mTLS node (ReenrollOnExpiry), re-enrolls once and
// retries with the fresh session — so a core restart that dropped the session recovers without
// restarting the node. Enrollment itself never triggers this (guarded), so there's no loop.
func (n *Node) postCtx(ctx context.Context, path, session string, body, out any) error {
	status, err := n.doPost(ctx, path, session, body, out)
	if status == http.StatusUnauthorized && n.ReenrollOnExpiry && path != PathEnroll {
		if rerr := n.reenroll(ctx); rerr == nil {
			_, err = n.doPost(ctx, path, n.sess(), body, out)
		}
	}
	return err
}

// reenroll re-runs enrollment with the remembered credentials, serialized so concurrent 401s
// (heartbeat + sync) don't stampede core with redundant enrollments.
func (n *Node) reenroll(ctx context.Context) error {
	n.reMu.Lock()
	defer n.reMu.Unlock()
	return n.enroll(ctx)
}

// doPost performs one POST and returns the HTTP status alongside any error.
func (n *Node) doPost(ctx context.Context, path, session string, body, out any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.CoreURL+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("Authorization", "Bearer "+session)
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("control %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}
