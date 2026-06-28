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
	"avairy/internal/gating"
	"avairy/internal/git"
	"avairy/internal/journal"
)

// ctxKey carries the resolved caller agent id through the request context.
type ctxKey int

const agentKey ctxKey = 0

// Server wraps the mcp-go server with avairy bus/board/journal dependencies.
type Server struct {
	mcp        *mcpserver.MCPServer
	bus        *bus.Bus
	board      *board.Board
	blackboard *board.Blackboard
	jrnl       journal.Log

	// Canonical git repo on core (DESIGN.md §9), wired by EnableGit. nil until enabled.
	gitRepo    *git.Repo
	gitApprove gating.Decider

	// resolveConflict applies an agent's reconciled file as the next hub version (EnableConflicts).
	resolveConflict ConflictResolver
	// conflictList returns the caller's currently-conflicted paths (EnableConflicts) — backs
	// list_conflicts so the agent doesn't grep for markers (#22). nil → tool reports none.
	conflictList func(agentID string) []string
	// freshLook runs a question through an ephemeral clean-context session (EnableFreshLook).
	freshLook FreshLookFunc

	// resolve maps an HTTP request to the caller's agent id. The bus stamps sender identity
	// from this (no spoofing, DESIGN.md §4); the daemon wires real enrollment tokens here.
	resolve func(*http.Request) string

	mu     sync.Mutex
	agents map[string]*registered

	// claims arbitrates @team request ownership: thread id -> who claimed it + when (claim_response).
	claimMu sync.Mutex
	claims  map[string]claim
	now     func() time.Time // injectable clock (claim TTL); defaults to time.Now
}

type registered struct {
	roles  []string
	caps   map[string]string
	ch     <-chan bus.Message // the agent's bus inbox; read_inbox drains it
	cancel func()
	// wakeCh is a SECOND, independent subscription drained by the node daemon's PullInbox to decide
	// what to wake the agent on. It must be separate from ch so the daemon's drain (which discards
	// context-only messages it won't wake on, #25) doesn't empty the agent's read_inbox. This mirrors
	// core-local agents, where the runner and read_inbox already use distinct subscriptions.
	wakeCh     <-chan bus.Message
	wakeCancel func()
}

// inboxMessage is the wire view of a bus message returned by read_inbox.
type inboxMessage struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	To       string `json:"to"` // "all" | "team" | "agent:<id>" | "role:<name>" — "team" means claim_response first
	Body     string `json:"body"`
	Delivery string `json:"delivery"`
	Time     string `json:"time"`
}

// NewServer builds the MCP bus server backed by b/bd/j and registers all tools.
func NewServer(b *bus.Bus, bd *board.Board, j journal.Log) *Server {
	s := &Server{
		mcp:        mcpserver.NewMCPServer("avairy", "0.1.0"),
		bus:        b,
		board:      bd,
		blackboard: board.NewBlackboard(j),
		jrnl:       j,
		agents:     make(map[string]*registered),
		claims:     make(map[string]claim),
		now:        time.Now,
		resolve:    func(r *http.Request) string { return r.Header.Get("X-Avairy-Agent") },
	}
	s.registerTools()
	return s
}

// Blackboard returns the shared blackboard (for the operator to restore it from the journal on
// startup, and for fresh_look to curate context from).
func (s *Server) Blackboard() *board.Blackboard { return s.blackboard }

func (s *Server) registerTools() {
	s.registerSendMessage()
	s.registerReadInbox()
	s.registerListAgents()
	s.registerNote()
	s.registerReadNotes()
	s.registerPostTask()
	s.registerClaimTask()
	s.registerListTasks()
	s.registerReportStatus()
	s.registerClaimResponse()
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
	wakeCh, wakeCancel := s.bus.Subscribe(id, roles...) // independent stream for the node's wake loop
	reg := &registered{roles: roles, caps: caps, ch: ch, cancel: cancel, wakeCh: wakeCh, wakeCancel: wakeCancel}

	s.mu.Lock()
	if old := s.agents[id]; old != nil {
		if old.cancel != nil {
			old.cancel()
		}
		if old.wakeCancel != nil {
			old.wakeCancel()
		}
	}
	s.agents[id] = reg
	s.mu.Unlock()
}

// Unregister removes an agent from the bus (unsubscribes + drops its inbox). Used to tear down an
// ephemeral consult agent (#24) when the operator closes it — so it stops receiving messages and
// vanishes from the roster.
func (s *Server) Unregister(id string) {
	s.mu.Lock()
	reg := s.agents[id]
	delete(s.agents, id)
	s.mu.Unlock()
	if reg != nil {
		if reg.cancel != nil {
			reg.cancel()
		}
		if reg.wakeCancel != nil {
			reg.wakeCancel()
		}
	}
}

// DrainInbox non-blockingly returns and clears the WAKE-queue messages buffered for an agent. Used
// by the control channel for a node daemon's PullInbox loop: the daemon decides what to wake the
// agent on. It drains the wake queue, NOT the read_inbox buffer (reg.ch) — so the daemon discarding
// context-only messages it won't wake on doesn't empty the agent's read_inbox.
func (s *Server) DrainInbox(agentID string) []bus.Message {
	reg := s.agent(agentID)
	if reg == nil {
		return nil
	}
	var out []bus.Message
	for {
		select {
		case m := <-reg.wakeCh:
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
				To:       addrString(m.To),
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
