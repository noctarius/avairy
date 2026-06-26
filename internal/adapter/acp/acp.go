// Package acp is a generic Agent Client Protocol (ACP) adapter engine — the JSON-RPC peer
// and agent.Adapter/Session mapping shared by every ACP-speaking agent. ACP is Zed's open
// protocol (JSON-RPC 2.0 over NDJSON), the same shape as Codex's app-server, so the peer
// mirrors internal/adapter/codex.
//
// Per-agent specifics (launch command, quirks) live in a small Profile; concrete families
// compose this engine behind their own constructor (e.g. copilot.New()). Verified handshake:
// initialize → {protocolVersion, agentCapabilities{mcpCapabilities.http}}. session methods
// follow the ACP v1 spec (session/new, session/prompt, session/update, session/cancel,
// session/request_permission).
package acp

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
	"avairy/internal/gating"
)

// Profile is the per-agent seam: how to launch an ACP agent. Everything else is generic.
type Profile struct {
	Family  agent.Family
	Command string
	Args    []string
	Env     []string
}

// Adapter drives an ACP agent described by Profile.
type Adapter struct {
	Profile Profile
	// Decide gates tool execution via session/request_permission. nil = allow all.
	Decide gating.Decider
}

// New returns an Adapter for the given profile.
func New(p Profile) *Adapter { return &Adapter{Profile: p} }

func (a *Adapter) Family() agent.Family { return a.Profile.Family }

func (a *Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsInterrupt: true,  // session/cancel
		SupportsSteer:     false, // no native mid-turn inject; a new prompt starts after the turn
		SupportsResume:    false, // protocol has session/load, but Start doesn't honor ResumeID yet
		MCPClient:         true,  // mcpServers in session/new (http verified)
		Enforcement:       agent.EnforcementHooked,
	}
}

// Start spawns the agent, performs the initialize + session/new handshake (wiring cfg.MCP as
// ACP mcpServers), and returns a ready session.
func (a *Adapter) Start(ctx context.Context, cfg agent.SessionConfig) (agent.Session, error) {
	cmd := exec.CommandContext(ctx, a.Profile.Command, a.Profile.Args...)
	cmd.Dir = cfg.Workspace
	cmd.Env = append(cmd.Environ(), a.Profile.Env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: start %s: %w", a.Profile.Command, err)
	}

	s := &session{
		cmd:     cmd,
		stdin:   stdin,
		decide:  a.Decide,
		events:  make(chan agent.Event, 64),
		pending: make(map[int64]chan rpcResult),
		done:    make(chan struct{}),
	}
	go s.readLoop(stdout)

	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := s.call(hctx, "initialize", initializeParams{
		ProtocolVersion:    1,
		ClientCapabilities: clientCapabilities{Fs: fsCapabilities{}},
	}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("acp: initialize: %w", err)
	}
	nsRes, err := s.call(hctx, "session/new", sessionNewParams{
		Cwd:        cfg.Workspace,
		McpServers: mcpServers(cfg.MCP),
	})
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("acp: session/new (is the agent logged in?): %w", err)
	}
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(nsRes, &ns); err != nil || ns.SessionID == "" {
		_ = s.Close()
		return nil, fmt.Errorf("acp: session/new returned no sessionId (%w)", err)
	}
	s.id = ns.SessionID
	return s, nil
}

// --- JSON-RPC plumbing (mirrors internal/adapter/codex) ---

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
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
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	decide gating.Decider
	events chan agent.Event
	done   chan struct{}

	id string

	encMu    sync.Mutex
	promptMu sync.Mutex // serializes session/prompt turns

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResult

	msgMu  sync.Mutex
	msgBuf string // accumulates agent_message_chunk text until flushed
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
		return nil, errors.New("acp: session closed")
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
					ch <- rpcResult{err: fmt.Errorf("acp rpc error %d: %s", m.Error.Code, m.Error.Message)}
				} else {
					ch <- rpcResult{result: m.Result}
				}
			}
		}
	}
}

// --- agent.Session ---

func (s *session) ID() string { return s.id }

func (s *session) Send(ctx context.Context, text string, d agent.Delivery) error {
	// One prompt (turn) at a time; ACP runs a single prompt per session.
	s.promptMu.Lock()
	defer s.promptMu.Unlock()

	res, err := s.call(ctx, "session/prompt", promptParams{
		SessionID: s.id,
		Prompt:    []contentBlock{{Type: "text", Text: text}},
	})
	s.flushText() // emit any buffered assistant text before the turn boundary
	if err != nil {
		return err
	}
	var pr struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(res, &pr)
	s.emit(agent.Event{Type: agent.EventTurnDone, Interrupted: pr.StopReason == "cancelled", Raw: cloneRaw(res)})
	return nil
}

func (s *session) Interrupt(ctx context.Context) error {
	// session/cancel is a notification; the in-flight session/prompt returns stopReason=cancelled.
	return s.write(rpcRequest{JSONRPC: "2.0", ID: atomic.AddInt64(&s.nextID, 1), Method: "session/cancel", Params: cancelParams{SessionID: s.id}})
}

func (s *session) Events() <-chan agent.Event { return s.events }

func (s *session) Close() error {
	_ = s.stdin.Close()
	return s.cmd.Wait()
}

// --- request param types ---

type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
}

type clientCapabilities struct {
	Fs fsCapabilities `json:"fs"`
}

type fsCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type sessionNewParams struct {
	Cwd        string      `json:"cwd"`
	McpServers []mcpServer `json:"mcpServers"`
}

type mcpServer struct {
	Type    string      `json:"type,omitempty"` // "http" for remote
	Name    string      `json:"name"`
	URL     string      `json:"url,omitempty"`
	Headers []mcpHeader `json:"headers,omitempty"`
	Command string      `json:"command,omitempty"`
	Args    []string    `json:"args,omitempty"`
}

type mcpHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type cancelParams struct {
	SessionID string `json:"sessionId"`
}

// mcpServers maps avairy MCP config to ACP's mcpServers shape (http with header list).
func mcpServers(servers []agent.MCPServer) []mcpServer {
	out := make([]mcpServer, 0, len(servers))
	for _, srv := range servers {
		switch srv.Type {
		case "http":
			m := mcpServer{Type: "http", Name: srv.Name, URL: srv.URL}
			for k, v := range srv.Headers {
				m.Headers = append(m.Headers, mcpHeader{Name: k, Value: v})
			}
			out = append(out, m)
		default:
			out = append(out, mcpServer{Name: srv.Name, Command: srv.Command, Args: srv.Args})
		}
	}
	return out
}

func cloneRaw(b json.RawMessage) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
