package board_test

import (
	"path/filepath"
	"testing"

	"avairy/internal/board"
	"avairy/internal/journal"
)

func TestBlackboardWriteReadRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	jf, err := journal.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bb := board.NewBlackboard(jf)
	bb.Write("alice", "repro/linux", "panics on linux only")
	bb.Write("bob", "decision/db", "use postgres")
	bb.Write("alice", "repro/linux", "...also needs qemu") // latest write wins

	all := bb.Read("")
	if len(all) != 2 {
		t.Fatalf("want 2 notes, got %d: %+v", len(all), all)
	}
	repro := bb.Read("repro/")
	if len(repro) != 1 || repro[0].Text != "...also needs qemu" || repro[0].Author != "alice" {
		t.Fatalf("prefix read / latest-wins wrong: %+v", repro)
	}
	jf.Close()

	// Restart: replay the journal into a fresh blackboard.
	recs, err := journal.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bb2 := board.NewBlackboard(journal.NewMemory())
	bb2.Restore(recs)
	got := bb2.Read("")
	if len(got) != 2 {
		t.Fatalf("restore: want 2 notes, got %d", len(got))
	}
	if r := bb2.Read("repro/"); len(r) != 1 || r[0].Text != "...also needs qemu" {
		t.Fatalf("restore did not keep latest value: %+v", r)
	}
}
