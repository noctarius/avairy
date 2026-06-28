package mcp

import (
	"context"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"avairy/internal/journal"
)

// claimTTL is how long a response claim holds before a stalled/crashed claimant's lease expires and
// another agent may take the thread. Mirrors task-claim staleness.
const claimTTL = 5 * time.Minute

type claim struct {
	by string
	at time.Time
}

// registerClaimResponse lets an agent claim sole ownership of a team request (#team) so exactly one
// agent answers it. The first claimant wins; others are told to stand down.
func (s *Server) registerClaimResponse() {
	s.mcp.AddTool(mcpgo.NewTool("claim_response",
		mcpgo.WithDescription("Claim sole ownership of a team request (a message addressed to \"team\") before answering it. "+
			"Call this with the message's id BEFORE you reply. If it returns \"granted\" you own it — answer it. "+
			"If it returns \"denied\" another agent already took it — do NOT answer; stand down."),
		mcpgo.WithString("thread_id", mcpgo.Required(),
			mcpgo.Description("The id of the team message you intend to answer (the \"id\" field from read_inbox).")),
	), s.handleClaimResponse)
}

func (s *Server) handleClaimResponse(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	id := agentFromContext(ctx)
	if id == "" {
		return mcpgo.NewToolResultError("claim_response: caller identity not resolved"), nil
	}
	thread, err := req.RequireString("thread_id")
	if err != nil {
		return mcpgo.NewToolResultError("claim_response: 'thread_id' is required"), nil
	}

	now := s.now()
	s.claimMu.Lock()
	if c, ok := s.claims[thread]; ok && c.by != id && now.Sub(c.at) < claimTTL {
		s.claimMu.Unlock()
		return mcpgo.NewToolResultText("denied: " + c.by + " is handling " + thread + " — stand down, do not answer it"), nil
	}
	first := s.claims[thread].by != id // a fresh claim (new owner) vs the owner re-affirming
	s.claims[thread] = claim{by: id, at: now}
	s.claimMu.Unlock()

	if first {
		// Journal so the operator and the other agents see who took it (they stand down).
		s.jrnl.Append(journal.KindSystem, id, map[string]any{"event": "response_claimed", "thread": thread})
	}
	return mcpgo.NewToolResultText("granted: you own " + thread + " — answer it; the other agents will stand down"), nil
}
