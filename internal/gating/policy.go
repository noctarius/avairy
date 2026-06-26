package gating

import (
	"context"
	"strings"
)

// Approver is consulted for gated actions — the human (TUI approvals view) or facilitator.
type Approver func(ctx context.Context, req Request) (Decision, error)

// Policy is the §7 enforcement decision: free actions are allowed; gated actions go to the
// Approver (and are denied if there is none). Its Decide method satisfies Decider, so it can
// be handed straight to a Backend or an adapter's approval hook.
type Policy struct {
	Approve Approver
	// GateEdits opts into approving file edits too (ActionFileWrite, otherwise free) — per-edit
	// human approval (§7); allow-for-session in the broker keeps it from being one-prompt-per-diff.
	GateEdits bool
}

// Decide implements Decider.
func (p Policy) Decide(ctx context.Context, req Request) (Decision, error) {
	if !p.gated(req) {
		return Allow, nil
	}
	if p.Approve == nil {
		return Deny, nil // no approver wired → fail closed
	}
	return p.Approve(ctx, req)
}

// gated is Gated plus the policy's opt-in edit gating.
func (p Policy) gated(req Request) bool {
	if p.GateEdits && req.Kind == ActionFileWrite {
		return true
	}
	return Gated(req)
}

// Gated reports whether a request needs approval per the §7 policy (edits are free here; opt in
// to gating them via Policy.GateEdits).
func Gated(req Request) bool {
	switch req.Kind {
	case ActionGitMutate, ActionCrossNode, ActionInstall:
		return true
	case ActionFileWrite, ActionRead:
		return false // local edits and reads are free
	default: // ActionCommand and anything else: inspect the command text
		return GatedCommand(req.Summary)
	}
}

var (
	destructive  = []string{"rm -rf", "rm -fr", "rm -r ", "mkfs", "dd if=", ":(){", "> /dev/sd", "shutdown", "reboot", "chmod -r 777 /"}
	gitMutations = []string{"git push", "git commit", "git tag", "git reset --hard", "git clean -"}
	installs     = []string{"sudo ", "apt install", "apt-get install", "brew install", "npm install", "npm i ", "pip install", "go install", "yum install", "dnf install", "cargo install"}
)

// GatedCommand reports whether a shell command is gated (destructive, a git history
// mutation, or a privileged/package install) per §7.
func GatedCommand(cmd string) bool {
	c := strings.ToLower(strings.TrimSpace(cmd))
	for _, set := range [][]string{destructive, gitMutations, installs} {
		for _, p := range set {
			if strings.Contains(c, p) {
				return true
			}
		}
	}
	return false
}
