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
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"avairy/internal/adapter/jsonrpc"
	"avairy/internal/agent"
	"avairy/internal/gating"
)

// Profile is the per-agent seam: how to launch an ACP agent. Everything else is generic.
// Args is a builder (not a static slice) because per-session flags — model, reasoning effort —
// map to family-specific flag names AND positions (e.g. grok wants them on the `agent` subcommand,
// before `stdio`), which a generic append couldn't place. ACP's session/new carries no such fields,
// so the CLI flags are the only lever.
type Profile struct {
	Family  agent.Family
	Command string
	Args    func(cfg agent.SessionConfig) []string
	Env     []string
	Efforts []string // accepted reasoning-effort levels (family-specific; copilot adds "none")
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
		SupportsResume:    true,  // session/load by id (Copilot & Grok both advertise loadSession)
		MCPClient:         true,  // mcpServers in session/new (http verified)
		Enforcement:       agent.EnforcementHooked,
		ReasoningEfforts:  a.Profile.Efforts,
	}
}

// Start spawns the agent, performs the initialize + session/new handshake (wiring cfg.MCP as
// ACP mcpServers), and returns a ready session.
func (a *Adapter) Start(ctx context.Context, cfg agent.SessionConfig) (agent.Session, error) {
	cmd := exec.CommandContext(ctx, a.Profile.Command, a.Profile.Args(cfg)...)
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
		cmd:    cmd,
		peer:   jsonrpc.NewPeer("acp", stdin),
		decide: a.Decide,
		events: make(chan agent.Event, 64),
	}
	go func() { s.peer.Run(stdout, s); close(s.events) }()

	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := s.peer.Call(hctx, "initialize", initializeParams{
		ProtocolVersion:    1,
		ClientCapabilities: clientCapabilities{Fs: fsCapabilities{}},
	}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("acp: initialize: %w", err)
	}
	// Resume a prior session by id (both Copilot and Grok advertise loadSession). session/load
	// replays history via session/update — suppressed (already in our journal). Fall back to a
	// fresh session if the id can't be loaded, so respawn never hard-fails.
	if cfg.ResumeID != "" && cfg.Mode != agent.SessionEphemeral {
		s.loading.Store(true)
		_, lerr := s.peer.Call(hctx, "session/load", sessionLoadParams{
			SessionID:  cfg.ResumeID,
			Cwd:        cfg.Workspace,
			McpServers: mcpServers(cfg.MCP),
		})
		s.loading.Store(false)
		if lerr == nil {
			s.id = cfg.ResumeID
			return s, nil
		}
	}
	nsRes, err := s.peer.Call(hctx, "session/new", sessionNewParams{
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

// rpcResponse answers a server-initiated request. ACP includes the jsonrpc field on responses, so
// this stays local rather than living in the shared peer.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type session struct {
	cmd    *exec.Cmd
	peer   *jsonrpc.Peer
	decide gating.Decider
	events chan agent.Event

	id      string
	loading atomic.Bool // true while session/load replays history — suppress those as live events

	promptMu sync.Mutex // serializes session/prompt turns

	msgMu  sync.Mutex
	msgBuf string // accumulates agent_message_chunk text until flushed
}

// --- agent.Session ---

func (s *session) ID() string { return s.id }

func (s *session) Send(ctx context.Context, text string, d agent.Delivery) error {
	// One prompt (turn) at a time; ACP runs a single prompt per session.
	s.promptMu.Lock()
	defer s.promptMu.Unlock()

	res, err := s.peer.Call(ctx, "session/prompt", promptParams{
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
	// session/cancel is fire-and-forget; the in-flight session/prompt returns stopReason=cancelled.
	return s.peer.Send("session/cancel", cancelParams{SessionID: s.id})
}

func (s *session) Events() <-chan agent.Event { return s.events }

func (s *session) Close() error {
	_ = s.peer.Close()
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

// sessionLoadParams resumes a previously-created session by id (ACP loadSession capability).
type sessionLoadParams struct {
	SessionID  string      `json:"sessionId"`
	Cwd        string      `json:"cwd"`
	McpServers []mcpServer `json:"mcpServers"`
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
