// Package mcp exposes the avairy bus as a Model Context Protocol server (DESIGN.md §4):
// every agent connects to this one HTTP (StreamableHTTP) endpoint and gets tools to message
// peers, read its inbox, and work the task board. On a remote node the daemon tunnels a
// localhost endpoint to this server, so an agent only ever sees a local MCP server.
//
// Convention mirrors simplyblock/postbrain: a Server wrapping the mcp-go server, a
// registerTools() that delegates to per-tool registerXxx() methods, one file per tool.
package mcp

import (
	"context"
	"net/http"
	"sync"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

// ctxKey carries the resolved caller agent id through the request context.
type ctxKey int

const agentKey ctxKey = 0

// Server wraps the mcp-go server with avairy bus/board/journal dependencies.
type Server struct {
	mcp   *mcpserver.MCPServer
	bus   *bus.Bus
	board *board.Board
	jrnl  journal.Log

	// resolve maps an HTTP request to the caller's agent id. The bus stamps sender identity
	// from this (no spoofing, DESIGN.md §4); the daemon wires real enrollment tokens here.
	resolve func(*http.Request) string

	mu     sync.Mutex
	agents map[string]*registered
}

type registered struct {
	roles  []string
	caps   map[string]string
	ch     <-chan bus.Message // the agent's bus inbox; read_inbox drains it
	cancel func()
}

// inboxMessage is the wire view of a bus message returned by read_inbox.
type inboxMessage struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	Body     string `json:"body"`
	Delivery string `json:"delivery"`
	Time     string `json:"time"`
}

// NewServer builds the MCP bus server backed by b/bd/j and registers all tools.
func NewServer(b *bus.Bus, bd *board.Board, j journal.Log) *Server {
	s := &Server{
		mcp:     mcpserver.NewMCPServer("avairy", "0.1.0"),
		bus:     b,
		board:   bd,
		jrnl:    j,
		agents:  make(map[string]*registered),
		resolve: func(r *http.Request) string { return r.Header.Get("X-Avairy-Agent") },
	}
	s.registerTools()
	return s
}

func (s *Server) registerTools() {
	s.registerSendMessage()
	s.registerReadInbox()
	s.registerPostTask()
	s.registerClaimTask()
	s.registerListTasks()
	s.registerReportStatus()
}

// MCP returns the underlying mcp-go server (for tests / extra transports).
func (s *Server) MCP() *mcpserver.MCPServer { return s.mcp }

// EndpointPath is where the StreamableHTTP MCP endpoint is served.
const EndpointPath = "/mcp"

// HTTPHandler returns the StreamableHTTP transport, resolving caller identity per request.
func (s *Server) HTTPHandler() http.Handler {
	return mcpserver.NewStreamableHTTPServer(s.mcp,
		mcpserver.WithEndpointPath(EndpointPath),
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return context.WithValue(ctx, agentKey, s.resolve(r))
		}),
	)
}

// RegisterAgent records an agent's roles/capabilities and subscribes it to the bus. The
// subscription's buffered channel holds messages until read_inbox drains them. Called by
// the daemon when an agent enrolls; in the single-machine case, by the harness.
func (s *Server) RegisterAgent(id string, roles []string, caps map[string]string) {
	ch, cancel := s.bus.Subscribe(id, roles...)
	reg := &registered{roles: roles, caps: caps, ch: ch, cancel: cancel}

	s.mu.Lock()
	if old := s.agents[id]; old != nil && old.cancel != nil {
		old.cancel()
	}
	s.agents[id] = reg
	s.mu.Unlock()
}

// DrainInbox non-blockingly returns and clears the bus messages buffered for an agent. Used
// by the control channel to deliver inbound messages to a remote agent's daemon.
func (s *Server) DrainInbox(agentID string) []bus.Message {
	reg := s.agent(agentID)
	if reg == nil {
		return nil
	}
	var out []bus.Message
	for {
		select {
		case m := <-reg.ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

// drainInbox non-blockingly pulls all currently-buffered messages for reg.
func drainInbox(reg *registered) []inboxMessage {
	var out []inboxMessage
	for {
		select {
		case m := <-reg.ch:
			out = append(out, inboxMessage{
				ID:       m.ID,
				From:     m.From,
				Body:     m.Body,
				Delivery: string(m.Delivery),
				Time:     m.Time.Format(time.RFC3339Nano),
			})
		default:
			return out
		}
	}
}

func (s *Server) agent(id string) *registered {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agents[id]
}

// AgentMeta is a registered agent's identity + capabilities (for facilitator matchmaking).
type AgentMeta struct {
	ID    string
	Roles []string
	Caps  map[string]string
}

// AgentList returns metadata for all registered agents (the roster).
func (s *Server) AgentList() []AgentMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AgentMeta, 0, len(s.agents))
	for id, reg := range s.agents {
		out = append(out, AgentMeta{ID: id, Roles: reg.roles, Caps: reg.caps})
	}
	return out
}
