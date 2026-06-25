package mcp

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) registerPostTask() {
	s.mcp.AddTool(mcpgo.NewTool("post_task",
		mcpgo.WithDescription("Post a task to the shared board for any capable agent to claim."),
		mcpgo.WithString("title", mcpgo.Required(), mcpgo.Description("What needs doing")),
		mcpgo.WithArray("requires", mcpgo.WithStringItems(),
			mcpgo.Description("Capability constraints as key=value, e.g. [\"os=linux\",\"qemu=true\"]")),
		mcpgo.WithArray("deps", mcpgo.WithStringItems(),
			mcpgo.Description("Task ids this task depends on")),
	), s.handlePostTask)
}

func (s *Server) handlePostTask(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	from := agentFromContext(ctx)
	title, err := req.RequireString("title")
	if err != nil {
		return mcpgo.NewToolResultError("post_task: 'title' is required"), nil
	}
	requires := parseRequires(req.GetStringSlice("requires", nil))
	deps := req.GetStringSlice("deps", nil)
	t := s.board.Post(from, title, requires, deps)
	return mcpgo.NewToolResultText("posted " + t.ID), nil
}

func (s *Server) registerClaimTask() {
	s.mcp.AddTool(mcpgo.NewTool("claim_task",
		mcpgo.WithDescription("Claim an open task. Succeeds only if your node meets the task's requirements."),
		mcpgo.WithString("task_id", mcpgo.Required(), mcpgo.Description("Task id to claim")),
	), s.handleClaimTask)
}

func (s *Server) handleClaimTask(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	id := agentFromContext(ctx)
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcpgo.NewToolResultError("claim_task: 'task_id' is required"), nil
	}
	var caps map[string]string
	if reg := s.agent(id); reg != nil {
		caps = reg.caps
	}
	t, err := s.board.Claim(taskID, id, caps)
	if err != nil {
		return mcpgo.NewToolResultError("claim_task: " + err.Error()), nil
	}
	return mcpgo.NewToolResultText("claimed " + t.ID), nil
}

func (s *Server) registerListTasks() {
	s.mcp.AddTool(mcpgo.NewTool("list_tasks",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("List all tasks on the board with state, requirements, and claimant."),
	), s.handleListTasks)
}

func (s *Server) handleListTasks(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	b, err := json.Marshal(s.board.List())
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("list_tasks: marshal", err), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
}
