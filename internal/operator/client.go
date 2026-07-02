package operator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"avairy/internal/board"
	"avairy/internal/journal"
	"avairy/internal/tui"
)

// Client attaches to a remote core's operator API and presents the same surface the in-process
// Services does, so tui.Run(client.Deps()) renders an identical UI from another machine (item #18).
// It keeps a local journal (fed by the SSE stream) and a small state cache (tasks/approvals/
// conflicts/roster/control) refreshed whenever a relevant record arrives.
type Client struct {
	base  string
	token string
	http  *http.Client
	jrnl  *journal.Memory

	mu    sync.Mutex
	state State
	seen  map[uint64]bool // wire seqs already applied (dedup backfill/live overlap)
	ready bool
}

// Connect dials core, replays the journal backfill into a local log, fetches the initial state, and
// leaves a goroutine streaming live updates until ctx is cancelled. httpClient carries the TLS trust
// (build it from a join bundle or -ca); token authenticates (empty if the API is open).
func Connect(ctx context.Context, coreURL, token string, httpClient *http.Client) (*Client, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	c := &Client{
		base:  strings.TrimRight(coreURL, "/"),
		token: token,
		http:  httpClient,
		jrnl:  journal.NewMemory(),
		seen:  make(map[uint64]bool),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+PathStream, nil)
	if err != nil {
		return nil, err
	}
	c.authHeader(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("operator: connect %s: %w", c.base, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("operator: stream returned %s", resp.Status)
	}

	readyCh := make(chan struct{})
	go c.streamLoop(resp.Body, readyCh)
	select {
	case <-readyCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(15 * time.Second):
		return nil, errors.New("operator: timed out waiting for journal backfill")
	}
	c.refreshState() // initial snapshot, now consistent with the replayed journal
	return c, nil
}

func (c *Client) authHeader(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// streamLoop parses the SSE stream: it applies each record (deduped on wire Seq) to the local log,
// closes readyCh once the backfill-complete sentinel arrives, and thereafter refreshes the state
// cache on records that can change it. Returns when the body closes (ctx cancel / core gone).
func (c *Client) streamLoop(body io.ReadCloser, readyCh chan struct{}) {
	defer body.Close()
	rd := bufio.NewReaderSize(body, 1<<20)
	var data strings.Builder
	var event string
	var readyOnce sync.Once
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if event == readyEvent {
				c.mu.Lock()
				c.ready = true
				c.mu.Unlock()
				readyOnce.Do(func() { close(readyCh) })
			} else if data.Len() > 0 {
				c.applyWire(data.String())
			}
			data.Reset()
			event = ""
		case strings.HasPrefix(line, "data: "):
			data.WriteString(line[len("data: "):])
		case strings.HasPrefix(line, "event: "):
			event = line[len("event: "):]
		}
	}
}

// applyWire decodes one streamed record and feeds it to the local log. After backfill, a record
// that can change cached state triggers a synchronous refresh first, so the cache is fresh by the
// time the appended record wakes the TUI to re-render.
func (c *Client) applyWire(s string) {
	var pr journal.PersistedRecord
	if json.Unmarshal([]byte(s), &pr) != nil {
		return
	}
	c.mu.Lock()
	if c.seen[pr.Seq] {
		c.mu.Unlock()
		return
	}
	c.seen[pr.Seq] = true
	ready := c.ready
	c.mu.Unlock()

	rec, ok := decodeRecord(pr)
	if !ok {
		return
	}
	if ready && stateRelevant(pr.Kind) {
		c.refreshState()
	}
	c.jrnl.Append(rec.Kind, rec.Actor, rec.Data)
}

// stateRelevant reports whether a record kind can change the cached snapshot (tasks/approvals/
// conflicts/roster). Messages and agent events only flow through the journal.
func stateRelevant(k journal.Kind) bool {
	switch k {
	case journal.KindSystem, journal.KindTask, journal.KindHandover, journal.KindApproval:
		return true
	default:
		return false
	}
}

func (c *Client) refreshState() {
	var st State
	if err := c.get(PathState, &st); err != nil {
		return // keep the last good cache
	}
	c.mu.Lock()
	c.state = st
	c.mu.Unlock()
}

// --- tui.Deps surface ---

// Deps builds TUI deps backed by this client.
func (c *Client) Deps() tui.Deps {
	d := tui.Deps{
		Journal:          c.jrnl,
		Roster:           c.roster,
		Tasks:            c.tasks,
		Notes:            c.notes,
		Inject:           c.inject,
		Interrupt:        c.interrupt,
		React:            c.react,
		PendingApprovals: c.pendingApprovals,
		ResolveApproval:  c.resolveApproval,
		PendingConflicts: c.pendingConflicts,
		ResolveConflict:  c.resolveConflict,
		Consult:          c.consult,
		CloseConsult:     c.closeConsult,
		Reconfigure:      c.reconfigure,
		Configs:          c.configs,
		Commit:           c.commit,
	}
	c.mu.Lock()
	cs := c.state.Control
	c.mu.Unlock()
	if cs != nil {
		d.Control = &tui.ControlInfo{
			ControlURL:   cs.ControlURL,
			BusBase:      cs.BusBase,
			Warn:         cs.Warn,
			JoinFile:     cs.JoinFile,
			OperatorJoin: cs.OperatorJoin,
			WebURL:       cs.WebURL,
			MTLSOnly:     cs.MTLSOnly,
			CurrentToken: c.currentToken,
			NewToken:     c.newToken,
		}
	}
	return d
}

func (c *Client) roster() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.state.Roster...)
}

func (c *Client) tasks() []board.Task {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]board.Task(nil), c.state.Tasks...)
}

func (c *Client) notes() []board.Note {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]board.Note(nil), c.state.Notes...)
}

func (c *Client) pendingApprovals() []tui.ApprovalItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]tui.ApprovalItem, 0, len(c.state.Approvals))
	for _, a := range c.state.Approvals {
		out = append(out, tui.ApprovalItem{ID: a.ID, AgentID: a.AgentID, Kind: a.Kind, Summary: a.Summary, Reason: a.Reason, Diff: a.Diff})
	}
	return out
}

func (c *Client) pendingConflicts() []tui.ConflictItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]tui.ConflictItem, 0, len(c.state.Conflicts))
	for _, cf := range c.state.Conflicts {
		out = append(out, tui.ConflictItem{ID: cf.ID, Path: cf.Path, HubVersion: cf.HubVersion, Source: cf.Source, Detail: cf.Detail})
	}
	return out
}

func (c *Client) configs() []tui.AgentConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]tui.AgentConfig, 0, len(c.state.Configs))
	for _, cf := range c.state.Configs {
		out = append(out, tui.AgentConfig{
			Agent: cf.Agent, ModelMode: cf.ModelMode, EffortMode: cf.EffortMode,
			Efforts: cf.Efforts, Models: cf.Models, CurrentModel: cf.CurrentModel, CurrentEffort: cf.CurrentEffort,
		})
	}
	return out
}

func (c *Client) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Control != nil {
		return c.state.Control.Token
	}
	return ""
}

func (c *Client) inject(target, body string) {
	_ = c.post(PathInject, injectRequest{Target: target, Body: body}, nil)
}
func (c *Client) interrupt() { _ = c.post(PathInterrupt, struct{}{}, nil) }
func (c *Client) react(seq uint64, kind string) {
	_ = c.post(PathReact, reactRequest{Seq: seq, Kind: kind}, nil)
}
func (c *Client) resolveApproval(id, decision string) {
	_ = c.post(PathApproval, approvalDecision{ID: id, Decision: decision}, nil)
}

func (c *Client) resolveConflict(id, decision, target string) {
	_ = c.post(PathConflict, conflictDecision{ID: id, Decision: decision, Target: target}, nil)
}

func (c *Client) consult(target, family string) (string, error) {
	var resp consultResponse
	if err := c.post(PathConsult, consultRequest{Target: target, Family: family}, &resp); err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	return resp.ID, nil
}

func (c *Client) closeConsult(id string) bool {
	return c.post(PathClose, closeRequest{ID: id}, nil) == nil
}

func (c *Client) reconfigure(agent, model, effort string) {
	_ = c.post(PathReconfigure, reconfigureRequest{Agent: agent, Model: model, Effort: effort}, nil)
}

func (c *Client) commit(message string) (string, error) {
	var resp commitResponse
	if err := c.post(PathCommit, commitRequest{Message: message}, &resp); err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	return resp.Hash, nil
}

func (c *Client) newToken() string {
	var resp tokenResponse
	if err := c.post(PathToken, struct{}{}, &resp); err != nil {
		return c.currentToken()
	}
	c.mu.Lock()
	if c.state.Control != nil {
		c.state.Control.Token = resp.Token
	}
	c.mu.Unlock()
	return resp.Token
}

// --- HTTP helpers ---

func (c *Client) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	c.authHeader(req)
	return c.do(req, out)
}

func (c *Client) post(path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authHeader(req)
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("operator %s: %s: %s", req.URL.Path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil && resp.StatusCode == http.StatusOK {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
