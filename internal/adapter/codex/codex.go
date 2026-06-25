// Package codex implements an agent.Adapter that drives the Codex CLI via
// `codex app-server --stdio` — a long-lived JSON-RPC peer (v2 thread/turn protocol), not
// `codex exec` (which cannot route approvals back). Protocol verified against the schema
// emitted by `codex app-server generate-json-schema` for 0.142.1 (see ADAPTERS.md):
//
//   - handshake: initialize → thread/start (stores threadId)
//   - per message: turn/start (new turn) or turn/steer (inject into the active turn)
//   - interrupt: turn/interrupt
//   - notifications (item/completed, turn/completed, item/agentMessage/delta, error) → agent.Event
//   - server requests (item/.../requestApproval, exec/applyPatch) → answered (must always reply)
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"avairy/internal/agent"
)

// ApprovalDecider decides a Codex approval server-request. It returns a v2 decision
// ("accept" | "acceptForSession" | "decline") or v1 ("approved" | "denied") as appropriate
// for the method. Default approves; the gating EnforcementBackend wires the real policy.
type ApprovalDecider func(method string, params json.RawMessage) string

// Adapter drives the Codex app-server.
type Adapter struct {
	Bin            string // codex binary; defaults to "codex"
	ExtraArgs      []string
	ApprovalPolicy string // thread/start approvalPolicy: untrusted|on-request|never (default never)
	Sandbox        string // thread/start sandbox: read-only|workspace-write|danger-full-access
	Approve        ApprovalDecider
}

// New returns an Adapter using the `codex` binary from PATH.
func New() *Adapter { return &Adapter{Bin: "codex"} }

func (a *Adapter) Family() agent.Family { return agent.FamilyCodex }

func (a *Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsInterrupt: true, // turn/steer injects mid-turn; turn/interrupt cancels
		SupportsSteer:     true,
		SupportsResume:    true,
		MCPClient:         true,
		Enforcement:       agent.EnforcementHooked, // in-protocol approval requests
	}
}

func (a *Adapter) bin() string {
	if a.Bin == "" {
		return "codex"
	}
	return a.Bin
}

// Start spawns the app-server, performs the initialize + thread/start handshake, and returns
// a ready session.
func (a *Adapter) Start(ctx context.Context, cfg agent.SessionConfig) (agent.Session, error) {
	args := []string{"app-server", "--stdio"}
	args = append(args, mcpConfigArgs(cfg)...)
	args = append(args, a.ExtraArgs...)

	cmd := exec.CommandContext(ctx, a.bin(), args...)
	cmd.Dir = cfg.Workspace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex: start %s: %w", a.bin(), err)
	}

	approve := a.Approve
	if approve == nil {
		approve = defaultApprove
	}
	s := &session{
		cmd:     cmd,
		stdin:   stdin,
		approve: approve,
		events:  make(chan agent.Event, 64),
		pending: make(map[int64]chan rpcResult),
		done:    make(chan struct{}),
	}
	go s.readLoop(stdout)

	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := s.call(hctx, "initialize", initializeParams{ClientInfo: clientInfo{Name: "avairy", Version: "0.1.0"}}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("codex: initialize: %w", err)
	}
	tsRes, err := s.call(hctx, "thread/start", threadStartParams{
		Cwd:                   nonEmpty(cfg.Workspace),
		Model:                 nonEmpty(cfg.Model),
		DeveloperInstructions: nonEmpty(cfg.Role),
		ApprovalPolicy:        orDefault(a.ApprovalPolicy, "never"),
		Sandbox:               orDefault(a.Sandbox, "workspace-write"),
	})
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("codex: thread/start: %w", err)
	}
	var ts struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(tsRes, &ts); err != nil || ts.Thread.ID == "" {
		_ = s.Close()
		return nil, fmt.Errorf("codex: thread/start gave no thread id (%w)", err)
	}
	s.mu.Lock()
	s.threadID = ts.Thread.ID
	s.mu.Unlock()
	return s, nil
}

// --- JSON-RPC plumbing ---

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result any             `json:"result"`
}

type rpcMessage struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
	Result json.RawMessage  `json:"result"`
	Error  *rpcError        `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type session struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	approve ApprovalDecider
	events  chan agent.Event
	done    chan struct{}

	encMu sync.Mutex // serializes writes to stdin

	mu         sync.Mutex
	nextID     int64
	pending    map[int64]chan rpcResult
	threadID   string
	activeTurn string
	closed     bool
}

func (s *session) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.encMu.Lock()
	defer s.encMu.Unlock()
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

func (s *session) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&s.nextID, 1)
	ch := make(chan rpcResult, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()

	if err := s.write(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, errors.New("codex: session closed")
	}
}

func (s *session) readLoop(r io.Reader) {
	defer close(s.done)
	defer close(s.events)
	dec := json.NewDecoder(r)
	for {
		var m rpcMessage
		if err := dec.Decode(&m); err != nil {
			return
		}
		switch {
		case m.Method != "" && m.ID != nil:
			s.handleServerRequest(*m.ID, m.Method, m.Params)
		case m.Method != "":
			s.handleNotification(m.Method, m.Params)
		case m.ID != nil:
			id, _ := strconv.ParseInt(string(*m.ID), 10, 64)
			s.mu.Lock()
			ch := s.pending[id]
			delete(s.pending, id)
			s.mu.Unlock()
			if ch != nil {
				if m.Error != nil {
					ch <- rpcResult{err: fmt.Errorf("codex rpc error %d: %s", m.Error.Code, m.Error.Message)}
				} else {
					ch <- rpcResult{result: m.Result}
				}
			}
		}
	}
}

// handleServerRequest answers an approval request. It MUST always reply or the turn hangs.
func (s *session) handleServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	decision := s.approve(method, params)
	_ = s.write(rpcResponse{ID: id, Result: map[string]string{"decision": decision}})
}

// --- agent.Session ---

func (s *session) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadID
}

type userInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *session) Send(ctx context.Context, text string, d agent.Delivery) error {
	s.mu.Lock()
	tid, turn := s.threadID, s.activeTurn
	s.mu.Unlock()

	input := []userInput{{Type: "text", Text: text}}
	if turn != "" {
		// A turn is in flight: steer injects this message mid-turn (mid-reasoning).
		_, err := s.call(ctx, "turn/steer", steerParams{ThreadID: tid, ExpectedTurnID: turn, Input: input})
		return err
	}
	res, err := s.call(ctx, "turn/start", turnStartParams{ThreadID: tid, Input: input})
	if err != nil {
		return err
	}
	var r struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(res, &r) == nil && r.Turn.ID != "" {
		s.setTurn(r.Turn.ID)
	}
	return nil
}

func (s *session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	tid, turn := s.threadID, s.activeTurn
	s.mu.Unlock()
	if turn == "" {
		return nil
	}
	_, err := s.call(ctx, "turn/interrupt", interruptParams{ThreadID: tid, TurnID: turn})
	return err
}

func (s *session) Events() <-chan agent.Event { return s.events }

func (s *session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.stdin.Close()
	return s.cmd.Wait()
}

func (s *session) setTurn(id string) {
	s.mu.Lock()
	s.activeTurn = id
	s.mu.Unlock()
}

func (s *session) clearTurn() {
	s.mu.Lock()
	s.activeTurn = ""
	s.mu.Unlock()
}

// --- request param types ---

type initializeParams struct {
	ClientInfo clientInfo `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type threadStartParams struct {
	Cwd                   string `json:"cwd,omitempty"`
	Model                 string `json:"model,omitempty"`
	DeveloperInstructions string `json:"developerInstructions,omitempty"`
	ApprovalPolicy        string `json:"approvalPolicy,omitempty"`
	Sandbox               string `json:"sandbox,omitempty"`
}

type turnStartParams struct {
	ThreadID string      `json:"threadId"`
	Input    []userInput `json:"input"`
	Model    string      `json:"model,omitempty"`
}

type steerParams struct {
	ThreadID       string      `json:"threadId"`
	ExpectedTurnID string      `json:"expectedTurnId"`
	Input          []userInput `json:"input"`
}

type interruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// --- helpers ---

func defaultApprove(method string, _ json.RawMessage) string {
	// v1 methods use approved/denied; v2 uses accept/decline. Default: allow.
	switch method {
	case "execCommandApproval", "applyPatchApproval":
		return "approved"
	default:
		return "accept"
	}
}

func nonEmpty(s string) string { return s }

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// mcpConfigArgs renders cfg.MCP into `-c` config overrides the app-server accepts. Codex
// configures MCP via config.toml [mcp_servers]; http servers use url + http_headers.
func mcpConfigArgs(cfg agent.SessionConfig) []string {
	var args []string
	for _, srv := range cfg.MCP {
		if srv.Type != "http" || srv.URL == "" {
			continue
		}
		args = append(args, "-c", fmt.Sprintf("mcp_servers.%s.url=%q", srv.Name, srv.URL))
		// Auto-approve this server's tools (AppToolApproval::Approve). Without this, MCP tool
		// calls hit the approval path, which approvalPolicy=never force-denies ("user rejected
		// MCP tool call"). Real gating is the EnforcementBackend milestone.
		args = append(args, "-c", fmt.Sprintf("mcp_servers.%s.default_tools_approval_mode=%q", srv.Name, "approve"))
		for k, v := range srv.Headers {
			args = append(args, "-c", fmt.Sprintf("mcp_servers.%s.http_headers.%s=%q", srv.Name, k, v))
		}
	}
	return args
}
