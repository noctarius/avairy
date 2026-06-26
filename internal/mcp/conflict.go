package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"avairy/internal/journal"
)

// ConflictResolver applies an agent's reconciled content as the next hub version, clearing a
// conflict (DESIGN.md §9: divergent edits are surfaced for agent reconciliation, never silently
// merged). Returns the new hub version.
type ConflictResolver func(agentID, path string, content []byte) (uint64, error)

// EnableConflicts wires reconciliation: agents that get a CONFLICT notification merge the two
// sides and call resolve_conflict to commit the result. Call once when core has a hub.
func (s *Server) EnableConflicts(resolve ConflictResolver) {
	s.resolveConflict = resolve
	s.mcp.AddTool(mcpgo.NewTool("resolve_conflict",
		mcpgo.WithDescription("Resolve a sync CONFLICT you were notified about: submit the merged file content. It becomes the next canonical version."),
		mcpgo.WithString("path", mcpgo.Required(), mcpgo.Description("the conflicted file path (as given in the notification)")),
		mcpgo.WithString("content", mcpgo.Required(), mcpgo.Description("the full merged file content")),
	), s.handleResolveConflict)
}

func (s *Server) handleResolveConflict(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.resolveConflict == nil {
		return mcpgo.NewToolResultError("resolve_conflict: reconciliation not enabled"), nil
	}
	from := agentFromContext(ctx)
	path, err := req.RequireString("path")
	if err != nil {
		return mcpgo.NewToolResultError("resolve_conflict: 'path' is required"), nil
	}
	content, err := req.RequireString("content")
	if err != nil {
		return mcpgo.NewToolResultError("resolve_conflict: 'content' is required"), nil
	}
	version, err := s.resolveConflict(from, path, []byte(content))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	s.jrnl.Append(journal.KindSystem, from, map[string]any{"event": "conflict_resolved", "path": path, "version": version})
	return mcpgo.NewToolResultText("resolved " + path), nil
}
