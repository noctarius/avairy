package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"avairy/internal/gating"
	"avairy/internal/git"
	"avairy/internal/journal"
)

// EnableGit wires the canonical core repo into the bus so agents can read history (any agent)
// and request commits (gated, signed, executed here — DESIGN.md §9). approve routes a commit
// request to the operator; nil means commits are refused (no approver = fail closed). Call
// once, before serving, when core has a workspace that is a git repo.
func (s *Server) EnableGit(repo *git.Repo, approve gating.Decider) {
	s.gitRepo = repo
	s.gitApprove = approve
	s.registerGitHistory()
	s.registerRequestCommit()
}

func (s *Server) registerGitHistory() {
	s.mcp.AddTool(mcpgo.NewTool("git_history",
		mcpgo.WithDescription("Read the canonical repo's history for root-cause analysis: log, show a commit, diff, or blame a file. Read-only."),
		mcpgo.WithString("mode", mcpgo.Required(), mcpgo.Description("log | show | diff | blame")),
		mcpgo.WithString("ref", mcpgo.Description("commit/branch/range (e.g. HEAD~5..HEAD); must not start with '-'")),
		mcpgo.WithString("path", mcpgo.Description("limit to this file/dir (required for blame)")),
		mcpgo.WithNumber("limit", mcpgo.Description("max log entries (default 50, max 500)")),
	), s.handleGitHistory)
}

func (s *Server) handleGitHistory(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.gitRepo == nil {
		return mcpgo.NewToolResultError("git_history: no canonical repo on core"), nil
	}
	mode, err := req.RequireString("mode")
	if err != nil {
		return mcpgo.NewToolResultError("git_history: 'mode' is required (log|show|diff|blame)"), nil
	}
	out, err := s.gitRepo.History(ctx, git.HistoryMode(mode),
		req.GetString("ref", ""), req.GetString("path", ""), int(req.GetFloat("limit", 0)))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if out == "" {
		out = "(no output)"
	}
	return mcpgo.NewToolResultText(out), nil
}

func (s *Server) registerRequestCommit() {
	s.mcp.AddTool(mcpgo.NewTool("request_commit",
		mcpgo.WithDescription("Request a signed commit of the canonical repo. Gated: the operator must approve. Executed on core (signing keys never leave it)."),
		mcpgo.WithString("message", mcpgo.Required(), mcpgo.Description("commit message")),
		mcpgo.WithArray("paths", mcpgo.Description("files to stage; omit to stage all changes"),
			mcpgo.Items(map[string]any{"type": "string"})),
	), s.handleRequestCommit)
}

func (s *Server) handleRequestCommit(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.gitRepo == nil {
		return mcpgo.NewToolResultError("request_commit: no canonical repo on core"), nil
	}
	from := agentFromContext(ctx)
	message, err := req.RequireString("message")
	if err != nil {
		return mcpgo.NewToolResultError("request_commit: 'message' is required"), nil
	}
	paths := req.GetStringSlice("paths", nil)

	// A commit is a §7 history mutation: route it to the operator and bail if not allowed.
	gateReq := gating.Request{AgentID: from, Kind: gating.ActionGitMutate, Summary: "commit: " + message}
	if s.gitApprove == nil {
		return mcpgo.NewToolResultError("request_commit: commits are not enabled (no approver)"), nil
	}
	d, err := s.gitApprove(ctx, gateReq)
	if err != nil || (d != gating.Allow && d != gating.AllowForSession) {
		s.jrnl.Append(journal.KindSystem, from, map[string]any{"event": "commit_denied", "message": message})
		return mcpgo.NewToolResultError("request_commit: denied by operator"), nil
	}

	hash, err := s.gitRepo.Commit(ctx, paths, message)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	s.jrnl.Append(journal.KindSystem, from, map[string]any{"event": "git_commit", "hash": hash, "message": message})
	return mcpgo.NewToolResultText("committed " + hash), nil
}
