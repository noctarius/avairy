package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"avairy/internal/journal"
)

func (s *Server) registerReportStatus() {
	s.mcp.AddTool(mcpgo.NewTool("report_status",
		mcpgo.WithDescription("Report your status so the facilitator can help if you're stuck."),
		mcpgo.WithString("status", mcpgo.Required(),
			mcpgo.Description("One of: working | blocked | low_confidence | done")),
		mcpgo.WithString("detail", mcpgo.Description("What you're working on or stuck on")),
	), s.handleReportStatus)
}

func (s *Server) handleReportStatus(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	from := agentFromContext(ctx)
	status, err := req.RequireString("status")
	if err != nil {
		return mcpgo.NewToolResultError("report_status: 'status' is required"), nil
	}
	// Decodable payload so the coordinator's stuck-detection (DESIGN.md §5) can consume it.
	s.jrnl.Append(journal.KindSystem, from, map[string]any{
		"event":  "report_status",
		"agent":  from,
		"status": status,
		"detail": req.GetString("detail", ""),
	})
	return mcpgo.NewToolResultText("noted"), nil
}
