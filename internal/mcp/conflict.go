package mcp

import (
	"context"
	"encoding/json"

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

// EnableConflictList wires list_conflicts: lister returns the calling agent's currently-conflicted
// paths (authoritative, from the node's tracked set) so the agent never greps for markers (#22).
func (s *Server) EnableConflictList(lister func(agentID string) []string) {
	s.conflictList = lister
	s.mcp.AddTool(mcpgo.NewTool("list_conflicts",
		mcpgo.WithDescription("List YOUR files that currently have unresolved sync conflicts (git-style markers). Authoritative — use this instead of grepping for <<<<<<< markers. Resolve each by editing it marker-free (or resolve_conflict)."),
	), s.handleListConflicts)
}

func (s *Server) handleListConflicts(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	var paths []string
	if s.conflictList != nil {
		paths = s.conflictList(agentFromContext(ctx))
	}
	if len(paths) == 0 {
		return mcpgo.NewToolResultText("no conflicts"), nil
	}
	b, err := json.Marshal(paths)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
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
