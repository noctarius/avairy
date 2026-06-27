package mcp

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"avairy/internal/agent"
	"avairy/internal/bus"
)

func (s *Server) registerSendMessage() {
	s.mcp.AddTool(mcpgo.NewTool("send_message",
		mcpgo.WithDescription("Send a message to another agent, a role, or everyone, over the avairy bus."),
		mcpgo.WithString("to", mcpgo.Required(),
			mcpgo.Description("Recipient: \"broadcast\", \"agent:<id>\" (use an id from list_agents), or \"role:<name>\". "+
				"To reach a specific agent prefer agent:<id>; an agent is also addressable as role:<its id> and role:<its os>.")),
		mcpgo.WithString("body", mcpgo.Required(), mcpgo.Description("Message text")),
		mcpgo.WithString("delivery",
			mcpgo.Description("\"steer\" (default; deliver at next turn boundary) or \"interrupt\" (mid-reasoning)")),
	), s.handleSendMessage)
}

func (s *Server) handleSendMessage(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	from := agentFromContext(ctx)
	if from == "" {
		return mcpgo.NewToolResultError("send_message: caller identity not resolved"), nil
	}
	to, err := req.RequireString("to")
	if err != nil {
		return mcpgo.NewToolResultError("send_message: 'to' is required"), nil
	}
	body, err := req.RequireString("body")
	if err != nil {
		return mcpgo.NewToolResultError("send_message: 'body' is required"), nil
	}
	addr, err := parseAddr(to)
	if err != nil {
		return mcpgo.NewToolResultError("send_message: " + err.Error()), nil
	}
	// Reject a directed message that reaches no one, so the sender learns its address was wrong
	// instead of getting a false "sent" (a silent drop is what hid the role:macos bug). Broadcast
	// is a fan-out, not a targeted address, so it's always allowed.
	if addr.Kind != bus.ToBroadcast && !s.hasRecipient(from, addr) {
		return mcpgo.NewToolResultError("send_message: no agent matches " + to + " — call list_agents to see who's reachable (address a specific peer as agent:<id>)"), nil
	}
	delivery := agent.DeliverySteer
	if req.GetString("delivery", "steer") == string(agent.DeliveryInterrupt) {
		delivery = agent.DeliveryInterrupt
	}
	msg := s.bus.Publish(from, addr, body, delivery)
	return mcpgo.NewToolResultText("sent " + msg.ID), nil
}

func (s *Server) registerReadInbox() {
	s.mcp.AddTool(mcpgo.NewTool("read_inbox",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("Read and clear messages addressed to you since your last read."),
	), s.handleReadInbox)
}

func (s *Server) handleReadInbox(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	id := agentFromContext(ctx)
	reg := s.agent(id)
	if reg == nil {
		return mcpgo.NewToolResultError("read_inbox: agent not registered"), nil
	}
	msgs := drainInbox(reg)
	if len(msgs) == 0 {
		return mcpgo.NewToolResultText("[]"), nil
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("read_inbox: marshal", err), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
}
