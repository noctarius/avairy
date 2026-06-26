package board_test

import (
	"path/filepath"
	"testing"

	"avairy/internal/board"
	"avairy/internal/journal"
)

// The board rebuilds from the persisted journal: final task states are recovered and new ids
// continue past the highest restored one.
func TestBoardRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	jf, err := journal.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b1 := board.New(jf)
	t1 := b1.Post("alice", "build", nil, nil)
	b1.Post("alice", "test", map[string]string{"os": "linux"}, nil) // t2, stays open
	if _, err := b1.Claim(t1.ID, "bob", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b1.SetState(t1.ID, board.TaskDone); err != nil {
		t.Fatal(err)
	}
	jf.Close()

	// Restart: fresh board, replay the persisted journal.
	recs, err := journal.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b2 := board.New(journal.NewMemory())
	b2.Restore(recs)

	got := b2.List()
	if len(got) != 2 {
		t.Fatalf("want 2 tasks restored, got %d: %+v", len(got), got)
	}
	if got[0].ID != "t1" || got[0].State != board.TaskDone || got[0].Claimant != "bob" {
		t.Fatalf("t1 not restored to its final state: %+v", got[0])
	}
	if got[1].ID != "t2" || got[1].State != board.TaskOpen || got[1].Requires["os"] != "linux" {
		t.Fatalf("t2 not restored: %+v", got[1])
	}

	// New tasks continue past the restored ids.
	if t3 := b2.Post("alice", "ship", nil, nil); t3.ID != "t3" {
		t.Fatalf("post after restore = %q, want t3", t3.ID)
	}
}

// Restore on an empty journal is a no-op (first run).
func TestBoardRestoreEmpty(t *testing.T) {
	b := board.New(journal.NewMemory())
	b.Restore(nil)
	if len(b.List()) != 0 {
		t.Fatal("empty restore should leave an empty board")
	}
	if id := b.Post("a", "x", nil, nil).ID; id != "t1" {
		t.Fatalf("first post after empty restore = %q, want t1", id)
	}
}
