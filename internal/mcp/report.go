package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"avairy/internal/journal"
)

// blockedReport is the journal payload for a self-declared stuck signal; the coordinator's
// stuck-detection (DESIGN.md §5) consumes these to decide whether to wake the facilitator.
type blockedReport struct {
	Agent  string `json:"agent"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

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
	s.jrnl.Append(journal.KindSystem, from, blockedReport{
		Agent:  from,
		Status: status,
		Detail: req.GetString("detail", ""),
	})
	return mcpgo.NewToolResultText("noted"), nil
}
