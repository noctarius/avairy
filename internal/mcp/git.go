package mcp

import (
	"context"
	"encoding/json"

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
	s.registerWorktree()
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

func (s *Server) registerWorktree() {
	s.mcp.AddTool(mcpgo.NewTool("scratch_worktree",
		mcpgo.WithDescription("Manage a disposable checkout of the canonical repo at some ref, isolated from the live tree — for bisect / build / reproduce. Read-only w.r.t. history."),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Description("create | list | remove")),
		mcpgo.WithString("ref", mcpgo.Description("commit/branch to check out (create; default HEAD)")),
		mcpgo.WithString("id", mcpgo.Description("worktree id to remove")),
	), s.handleWorktree)
}

func (s *Server) handleWorktree(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.gitRepo == nil {
		return mcpgo.NewToolResultError("scratch_worktree: no canonical repo on core"), nil
	}
	switch req.GetString("action", "") {
	case "create":
		wt, err := s.gitRepo.AddWorktree(ctx, req.GetString("ref", ""))
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		s.jrnl.Append(journal.KindSystem, agentFromContext(ctx), map[string]any{"event": "worktree_create", "id": wt.ID, "ref": wt.Ref})
		return jsonResult(wt)
	case "list":
		return jsonResult(s.gitRepo.ListWorktrees())
	case "remove":
		id := req.GetString("id", "")
		if id == "" {
			return mcpgo.NewToolResultError("scratch_worktree: 'id' is required to remove"), nil
		}
		if err := s.gitRepo.RemoveWorktree(ctx, id); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		s.jrnl.Append(journal.KindSystem, agentFromContext(ctx), map[string]any{"event": "worktree_remove", "id": id})
		return mcpgo.NewToolResultText("removed " + id), nil
	default:
		return mcpgo.NewToolResultError("scratch_worktree: 'action' must be create|list|remove"), nil
	}
}

func jsonResult(v any) (*mcpgo.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
}
