package gating

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatedCommand(t *testing.T) {
	gated := []string{"rm -rf /", "sudo reboot", "git push origin main", "npm install left-pad", "go install ./..."}
	free := []string{"go test ./...", "go build", "ls -la", "cat file.go", "git status", "git diff"}
	for _, c := range gated {
		if !GatedCommand(c) {
			t.Errorf("%q should be gated", c)
		}
	}
	for _, c := range free {
		if GatedCommand(c) {
			t.Errorf("%q should be free", c)
		}
	}
}

// Edits are free by default but gated when GateEdits is set; reads are never gated.
func TestPolicyGateEdits(t *testing.T) {
	free := Policy{}
	if d, _ := free.Decide(context.Background(), Request{Kind: ActionFileWrite, Summary: "main.go"}); d != Allow {
		t.Fatalf("edits should be free by default, got %v", d)
	}

	var asked bool
	gated := Policy{GateEdits: true, Approve: func(_ context.Context, _ Request) (Decision, error) {
		asked = true
		return Allow, nil
	}}
	if d, _ := gated.Decide(context.Background(), Request{Kind: ActionFileWrite, Summary: "main.go"}); d != Allow || !asked {
		t.Fatalf("with GateEdits, an edit should go to the approver (asked=%v d=%v)", asked, d)
	}
	asked = false
	if d, _ := gated.Decide(context.Background(), Request{Kind: ActionRead, Summary: "cat x"}); d != Allow || asked {
		t.Fatalf("reads must never be gated (asked=%v d=%v)", asked, d)
	}
	if d, _ := (Policy{GateEdits: true}).Decide(context.Background(), Request{Kind: ActionFileWrite}); d != Deny {
		t.Fatalf("gated edit with no approver should deny, got %v", d)
	}
}

func TestPolicyFailsClosedWithoutApprover(t *testing.T) {
	p := Policy{} // no approver
	if d, _ := p.Decide(context.Background(), Request{Kind: ActionCommand, Summary: "go test"}); d != Allow {
		t.Fatalf("free command should be allowed, got %v", d)
	}
	if d, _ := p.Decide(context.Background(), Request{Kind: ActionCommand, Summary: "rm -rf /"}); d != Deny {
		t.Fatalf("gated command with no approver should be denied, got %v", d)
	}
	if d, _ := p.Decide(context.Background(), Request{Kind: ActionGitMutate, Summary: "commit"}); d != Deny {
		t.Fatalf("git mutation should be denied without approver, got %v", d)
	}
}

func TestPolicyConsultsApprover(t *testing.T) {
	var seen Request
	p := Policy{Approve: func(_ context.Context, r Request) (Decision, error) { seen = r; return Allow, nil }}
	d, _ := p.Decide(context.Background(), Request{Kind: ActionCommand, Summary: "git push"})
	if d != Allow || seen.Summary != "git push" {
		t.Fatalf("approver not consulted: d=%v seen=%+v", d, seen)
	}
}

func TestHookHandler(t *testing.T) {
	h := HookHandler(Policy{}.Decide) // fail-closed
	call := func(payload string) string {
		req := httptest.NewRequest("POST", "/hook", strings.NewReader(payload))
		w := httptest.NewRecorder()
		h(w, req)
		var resp struct {
			HookSpecificOutput struct {
				PermissionDecision string `json:"permissionDecision"`
			} `json:"hookSpecificOutput"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.HookSpecificOutput.PermissionDecision
	}
	if got := call(`{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`); got != "allow" {
		t.Fatalf("free command: %q", got)
	}
	if got := call(`{"tool_name":"Bash","tool_input":{"command":"rm -rf /"}}`); got != "deny" {
		t.Fatalf("destructive command: %q", got)
	}
	if got := call(`{"tool_name":"Edit","tool_input":{"file_path":"main.go"}}`); got != "allow" {
		t.Fatalf("local edit should be free: %q", got)
	}
}
