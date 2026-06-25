package codex

import (
	"encoding/json"
	"testing"

	"avairy/internal/gating"
)

func TestApproverFromDecider_FailClosedPolicy(t *testing.T) {
	approve := ApproverFromDecider(gating.Policy{}.Decide) // no approver → gated actions denied

	// A free command is accepted.
	if got := approve("item/commandExecution/requestApproval", json.RawMessage(`{"command":["go","test"]}`)); got != "accept" {
		t.Fatalf("free command: %q", got)
	}
	// A destructive command is declined.
	if got := approve("item/commandExecution/requestApproval", json.RawMessage(`{"command":["rm","-rf","/"]}`)); got != "decline" {
		t.Fatalf("destructive command: %q", got)
	}
	// v1 method uses approved/denied vocabulary.
	if got := approve("execCommandApproval", json.RawMessage(`{"command":["sudo","reboot"]}`)); got != "denied" {
		t.Fatalf("v1 destructive: %q", got)
	}
	if got := approve("execCommandApproval", json.RawMessage(`{"command":["ls"]}`)); got != "approved" {
		t.Fatalf("v1 free: %q", got)
	}
}
