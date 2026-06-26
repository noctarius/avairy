package mcp

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) registerNote() {
	s.mcp.AddTool(mcpgo.NewTool("note",
		mcpgo.WithDescription("Write a durable note to the shared blackboard under a key — shared memory for context, decisions, and findings other agents (and the operator) can read. Latest write per key wins."),
		mcpgo.WithString("key", mcpgo.Required(), mcpgo.Description("note key, e.g. \"repro/linux-panic\" or \"decision/db\"")),
		mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("the note content")),
	), s.handleNote)
}

func (s *Server) handleNote(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	key, err := req.RequireString("key")
	if err != nil {
		return mcpgo.NewToolResultError("note: 'key' is required"), nil
	}
	text, err := req.RequireString("text")
	if err != nil {
		return mcpgo.NewToolResultError("note: 'text' is required"), nil
	}
	n := s.blackboard.Write(agentFromContext(ctx), key, text)
	return mcpgo.NewToolResultText("noted " + n.Key), nil
}

func (s *Server) registerReadNotes() {
	s.mcp.AddTool(mcpgo.NewTool("read_notes",
		mcpgo.WithDescription("Read shared blackboard notes — all, or those whose key starts with 'prefix'."),
		mcpgo.WithString("prefix", mcpgo.Description("only notes whose key starts with this (omit for all)")),
	), s.handleReadNotes)
}

func (s *Server) handleReadNotes(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	notes := s.blackboard.Read(req.GetString("prefix", ""))
	b, err := json.Marshal(notes)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
}
