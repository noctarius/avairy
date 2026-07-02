// Package agent defines the family-agnostic contract for driving an AI coding agent
// (Claude Code, Codex, ...) as a long-lived, interruptible, gated worker.
//
// The two families we target have fundamentally different control surfaces (see
// ADAPTERS.md): Claude Code is driven via the stream-json CLI or the Agent SDK with gating
// through a PreToolUse hook; Codex is driven via the app-server JSON-RPC protocol with
// gating through in-protocol approval requests. This package normalizes both behind one
// interface; the gating package (internal/gating) normalizes their enforcement.
package agent

import "context"

// Family identifies an agent family.
type Family string

const (
	FamilyClaudeCode Family = "claude-code"
	FamilyCodex      Family = "codex"
	FamilyCopilot    Family = "copilot"
	FamilyGrok       Family = "grok"
)

// EnforcementLevel declares how strongly a node/adapter can gate an agent's actions
// (DESIGN.md §7). Surfaced in the TUI so the operator always sees the containment level.
type EnforcementLevel string

const (
	// EnforcementSandboxed: OS-layer confinement (future drop-in; out of scope for v1).
	EnforcementSandboxed EnforcementLevel = "sandboxed"
	// EnforcementHooked: native permission hook / in-protocol approval routes to avairy.
	EnforcementHooked EnforcementLevel = "hooked"
	// EnforcementAdvisory: allow + log + stream only (no real block).
	EnforcementAdvisory EnforcementLevel = "advisory"
)

// SessionMode selects context continuity (DESIGN.md §8). Role is always persistent; the
// session is chosen per request.
type SessionMode string

const (
	// SessionPersistent: long-lived project session; accumulates context, compacted.
	SessionPersistent SessionMode = "persistent"
	// SessionEphemeral: same role, clean context from the blackboard, no history — a fresh look.
	SessionEphemeral SessionMode = "ephemeral"
)

// Delivery selects how an injected message reaches a running agent (DESIGN.md §6).
type Delivery string

const (
	// DeliveryInterrupt: cancel current generation, inject, resume (true mid-reasoning).
	// Requires Capabilities().SupportsInterrupt.
	DeliveryInterrupt Delivery = "interrupt"
	// DeliverySteer: queue, deliver at the next turn/tool boundary. Always available.
	DeliverySteer Delivery = "steer"
)

// Capabilities describes what a concrete adapter supports. The coordinator and TUI use it
// to choose a delivery mode and display enforcement strength.
type Capabilities struct {
	SupportsInterrupt bool // true mid-generation cancel (Claude Agent SDK / Codex app-server)
	SupportsSteer     bool // mid-turn input injection
	SupportsResume    bool // session resume
	MCPClient         bool // can connect to avairy's MCP server (the bus)
	Enforcement       EnforcementLevel
}

// MCPServer points an agent at an MCP endpoint. For the avairy bus (DESIGN.md §4) every
// agent is handed a localhost endpoint; the node daemon tunnels it to the core bus.
type MCPServer struct {
	Name    string
	Type    string            // "stdio" | "http"
	URL     string            // for http
	Headers map[string]string // for http (e.g. X-Avairy-Agent for bus identity)
	Command string            // for stdio
	Args    []string          // for stdio
}

// SessionConfig configures a new agent session.
type SessionConfig struct {
	AgentID   string // stable bus identity; role is a non-unique label (DESIGN.md §4)
	Role      string // persistent system prompt / persona (DESIGN.md §8)
	Mode      SessionMode
	Workspace string      // working directory on the node
	ResumeID  string      // non-empty to resume a prior persistent session
	MCP       []MCPServer // the bus, plus any extra servers
	Model     string      // optional family-specific model id
	Effort    string      // optional reasoning-effort level (family-specific: e.g. claude low|medium|high|xhigh|max, codex model_reasoning_effort)
}

// Adapter is the per-family driver (DESIGN.md §3). One Adapter instance can start many
// Sessions.
type Adapter interface {
	Family() Family
	Capabilities() Capabilities
	Start(ctx context.Context, cfg SessionConfig) (Session, error)
}

// Session is one running agent conversation.
type Session interface {
	ID() string
	// Send delivers a message (from a peer, the facilitator, or the human) using the given
	// delivery mode. Steer is always honored; Interrupt requires SupportsInterrupt.
	Send(ctx context.Context, text string, d Delivery) error
	// Events streams normalized events until the session closes.
	Events() <-chan Event
	// Interrupt cancels in-flight generation. Returns an error if unsupported.
	Interrupt(ctx context.Context) error
	// Close ends the session; a persistent session can later be resumed via SessionConfig.ResumeID.
	Close() error
}

// EventType enumerates normalized cross-family stream events. Claude Code emits
// stream_event/message_stop; Codex emits item.*/turn.completed — both map here.
type EventType string

const (
	EventTurnStart  EventType = "turn_start"
	EventText       EventType = "text" // assistant text (delta or full message)
	EventReasoning  EventType = "reasoning"
	EventToolUse    EventType = "tool_use" // agent invoked a tool/command
	EventToolResult EventType = "tool_result"
	EventTurnDone   EventType = "turn_done" // turn complete (message_stop end_turn / turn.completed)
	EventUsage      EventType = "usage"
	EventError      EventType = "error"
)

// Event is a normalized stream event from a Session.
type Event struct {
	Type        EventType
	Text        string    // text / reasoning / error
	Tool        *ToolCall // tool_use / tool_result
	Usage       *Usage    // usage / turn_done
	Interrupted bool      // turn_done: the turn ended via interrupt rather than completing
	Raw         []byte    // original family event, for audit/debug
}

// ToolCall describes an agent tool/command invocation and (when known) its result.
type ToolCall struct {
	ID     string
	Name   string
	Input  map[string]any
	Result string // populated on tool_result
}

// Usage reports token/cost accounting for a turn (DESIGN.md §10, surfaced in the TUI).
type Usage struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}
