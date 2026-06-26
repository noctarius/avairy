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
