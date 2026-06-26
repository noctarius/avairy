package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"avairy/internal/journal"
)

// FreshLookFunc runs a question through an ephemeral, same-role session with NO prior
// conversation context (DESIGN.md §8) and returns the answer. It's the deliberate "fresh look"
// — the facilitator's primary tool against anchoring and loops.
type FreshLookFunc func(ctx context.Context, question string) (string, error)

// EnableFreshLook registers the fresh_look tool. Call once when an ephemeral runner is wired.
func (s *Server) EnableFreshLook(fn FreshLookFunc) {
	s.freshLook = fn
	s.mcp.AddTool(mcpgo.NewTool("fresh_look",
		mcpgo.WithDescription("Get an independent second opinion from a fresh session with NO prior context or conversation history — a deliberate clean look, e.g. when you're stuck, looping, or want to check an assumption."),
		mcpgo.WithString("question", mcpgo.Required(), mcpgo.Description("the question to put to the fresh session")),
	), s.handleFreshLook)
}

func (s *Server) handleFreshLook(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.freshLook == nil {
		return mcpgo.NewToolResultError("fresh_look: not enabled"), nil
	}
	question, err := req.RequireString("question")
	if err != nil {
		return mcpgo.NewToolResultError("fresh_look: 'question' is required"), nil
	}
	answer, err := s.freshLook(ctx, question)
	if err != nil {
		return mcpgo.NewToolResultError("fresh_look: " + err.Error()), nil
	}
	s.jrnl.Append(journal.KindSystem, agentFromContext(ctx), map[string]any{"event": "fresh_look", "question": question})
	return mcpgo.NewToolResultText(answer), nil
}
