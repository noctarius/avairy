package main

import (
	"context"
	"path/filepath"
	"testing"

	"avairy/internal/adapter/mock"
	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/journal"
)

// nodeCaps reports the agent family and model only when set, so a proxy-only node (no --family) or
// an unpinned agent (no --model) doesn't advertise empty capabilities.
func TestNodeCaps(t *testing.T) {
	full := nodeCaps("linux", "claude", "opus", "high")
	if full["os"] != "linux" || full["family"] != "claude" || full["model"] != "opus" || full["effort"] != "high" {
		t.Fatalf("full caps = %v", full)
	}
	driven := nodeCaps("darwin", "codex", "", "") // agent, no pinned model or effort
	if driven["family"] != "codex" {
		t.Fatalf("want family codex, got %v", driven)
	}
	if _, ok := driven["model"]; ok {
		t.Fatalf("unpinned model should be omitted, got %v", driven)
	}
	if _, ok := driven["effort"]; ok {
		t.Fatalf("unset effort should be omitted, got %v", driven)
	}
	proxyOnly := nodeCaps("windows", "", "", "") // no agent
	if len(proxyOnly) != 1 || proxyOnly["os"] != "windows" {
		t.Fatalf("proxy-only node should report just os, got %v", proxyOnly)
	}
}

// oneShot runs an ephemeral turn and returns the assistant text — exercised here with the mock
// (which echoes the prompt) so it's deterministic and credit-free.
func TestOneShotEphemeral(t *testing.T) {
	got, err := oneShot(context.Background(), mock.New(), "role", "", t.TempDir(), "ping")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ping" {
		t.Fatalf("oneShot = %q, want echoed prompt", got)
	}
}

// decodeRecords round-trips each journaled kind back to its typed form (so the TUI can replay
// history after a restart). This also guards that bus.Message / agent.Event survive JSON.
func TestDecodeRecordsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	jf, err := journal.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	jf.Append(journal.KindMessage, "human", bus.Message{From: "human", To: bus.Agent("alice"), Body: "hi", ID: "m1"})
	jf.Append(journal.KindAgentEvent, "alice", agent.Event{Type: agent.EventToolUse, Tool: &agent.ToolCall{Name: "Bash", Input: map[string]any{"command": "go test"}}})
	jf.Append(journal.KindTask, "alice", board.Task{ID: "t1", Title: "build", State: board.TaskOpen})
	jf.Append(journal.KindSystem, "n1", map[string]any{"event": "report_status", "status": "working"})
	jf.Close()

	prs, err := journal.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	recs := decodeRecords(prs)
	if len(recs) != 4 {
		t.Fatalf("decoded %d records, want 4", len(recs))
	}
	if m, ok := recs[0].Data.(bus.Message); !ok || m.Body != "hi" || m.To.Value != "alice" {
		t.Fatalf("message did not round-trip: %#v", recs[0].Data)
	}
	if e, ok := recs[1].Data.(agent.Event); !ok || e.Tool == nil || e.Tool.Name != "Bash" {
		t.Fatalf("agent event did not round-trip: %#v", recs[1].Data)
	}
	if tk, ok := recs[2].Data.(board.Task); !ok || tk.ID != "t1" {
		t.Fatalf("task did not round-trip: %#v", recs[2].Data)
	}
	if sm, ok := recs[3].Data.(map[string]any); !ok || sm["event"] != "report_status" {
		t.Fatalf("system record did not round-trip: %#v", recs[3].Data)
	}
	for i, r := range recs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("seqs not contiguous: rec %d has seq %d", i, r.Seq)
		}
	}
}
