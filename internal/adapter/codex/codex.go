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
	"fmt"
	"os/exec"
	"sync"
	"time"

	"avairy/internal/adapter/jsonrpc"
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
		// turn/start takes model + effort overrides ("for this turn and subsequent turns") — both live.
		ReconfigureModel:  agent.ReconfigureLive,
		ReconfigureEffort: agent.ReconfigureLive,
		SupportsInterrupt: true, // turn/steer injects mid-turn; turn/interrupt cancels
		SupportsSteer:     true,
		SupportsResume:    true, // thread/resume by threadId (verified against the app-server schema)
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
	args = append(args, reasoningArgs(cfg.Effort)...)
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
		peer:    jsonrpc.NewPeer("codex", stdin),
		approve: approve,
		events:  make(chan agent.Event, 64),
		model:   cfg.Model,
		effort:  cfg.Effort,
	}
	go func() { s.peer.Run(stdout, s); close(s.events) }()

	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := s.peer.Call(hctx, "initialize", initializeParams{ClientInfo: clientInfo{Name: "avairy", Version: "0.1.0"}}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("codex: initialize: %w", err)
	}
	approval, sandbox := orDefault(a.ApprovalPolicy, "never"), orDefault(a.Sandbox, "workspace-write")
	var tsRes json.RawMessage
	if cfg.ResumeID != "" && cfg.Mode != agent.SessionEphemeral {
		// Resume the prior thread by id; fall back to a fresh thread if it can't be loaded
		// (e.g. the app-server's store was cleared), so respawn never hard-fails.
		// Discard the resume error deliberately: tsRes stays nil and we fall back to thread/start.
		tsRes, _ = s.peer.Call(hctx, "thread/resume", threadResumeParams{
			ThreadID: cfg.ResumeID, Cwd: cfg.Workspace, ApprovalPolicy: approval, Sandbox: sandbox,
		})
	}
	if tsRes == nil {
		tsRes, err = s.peer.Call(hctx, "thread/start", threadStartParams{
			Cwd:                   cfg.Workspace,
			Model:                 cfg.Model,
			DeveloperInstructions: cfg.Role,
			ApprovalPolicy:        approval,
			Sandbox:               sandbox,
		})
	}
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

// rpcResponse answers a server-initiated request (the app-server's approval prompt). Codex omits
// the jsonrpc field on responses, so this stays local rather than living in the shared peer.
type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result any             `json:"result"`
}

type session struct {
	cmd     *exec.Cmd
	peer    *jsonrpc.Peer
	approve ApprovalDecider
	events  chan agent.Event

	mu         sync.Mutex // guards threadID / activeTurn / closed / model / effort
	threadID   string
	activeTurn string
	closed     bool
	model      string // current model/effort, sent as turn/start overrides; Reconfigure updates them
	effort     string
}

// OnServerRequest answers the app-server's approval request. It MUST always reply or the turn hangs.
func (s *session) OnServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	decision := s.approve(method, params)
	_ = s.peer.Write(rpcResponse{ID: id, Result: map[string]string{"decision": decision}})
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
		_, err := s.peer.Call(ctx, "turn/steer", steerParams{ThreadID: tid, ExpectedTurnID: turn, Input: input})
		return err
	}
	s.mu.Lock()
	model, effort := s.model, s.effort
	s.mu.Unlock()
	// model/effort are turn/start overrides "for this turn and subsequent turns" — carrying the
	// current values here is what makes Reconfigure take effect on the next turn (no respawn).
	res, err := s.peer.Call(ctx, "turn/start", turnStartParams{ThreadID: tid, Input: input, Model: model, Effort: effort})
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
	_, err := s.peer.Call(ctx, "turn/interrupt", interruptParams{ThreadID: tid, TurnID: turn})
	return err
}

// Reconfigure changes the model and/or effort in place; the new values ride the next turn/start as
// overrides ("for this turn and subsequent turns"), so no respawn is needed.
func (s *session) Reconfigure(ctx context.Context, model, effort string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if model != "" {
		s.model = model
	}
	if effort != "" {
		s.effort = effort
	}
	return nil
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
	_ = s.peer.Close()
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

// threadResumeParams resumes a prior thread by id (loaded from the app-server's on-disk store).
type threadResumeParams struct {
	ThreadID       string `json:"threadId"`
	Cwd            string `json:"cwd,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
}

type turnStartParams struct {
	ThreadID string      `json:"threadId"`
	Input    []userInput `json:"input"`
	Model    string      `json:"model,omitempty"`
	Effort   string      `json:"effort,omitempty"`
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

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// reasoningArgs pins codex's reasoning effort via a `-c model_reasoning_effort` override when an
// effort level is set (empty = leave the model/profile default).
func reasoningArgs(effort string) []string {
	if effort == "" {
		return nil
	}
	return []string{"-c", fmt.Sprintf("model_reasoning_effort=%q", effort)}
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
