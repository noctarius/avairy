package mcp

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// registerListAgents exposes the roster so an agent can discover peers unprompted (#24) — find who
// to send_message instead of guessing ids (e.g. "who's on linux?" via caps.os).
func (s *Server) registerListAgents() {
	s.mcp.AddTool(mcpgo.NewTool("list_agents",
		mcpgo.WithDescription("List the OTHER agents currently on the bus — their id, roles, and capabilities (e.g. os). Use this to find the right peer to send_message (e.g. one running on a specific OS) instead of guessing ids."),
	), s.handleListAgents)
}

func (s *Server) handleListAgents(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	self := agentFromContext(ctx)
	type peer struct {
		ID    string            `json:"id"`
		Roles []string          `json:"roles,omitempty"`
		Caps  map[string]string `json:"caps,omitempty"`
	}
	out := make([]peer, 0)
	for _, m := range s.AgentList() {
		if m.ID == self {
			continue // the caller doesn't need to list itself
		}
		out = append(out, peer{ID: m.ID, Roles: m.Roles, Caps: m.Caps})
	}
	if len(out) == 0 {
		return mcpgo.NewToolResultText("no other agents on the bus"), nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
}
