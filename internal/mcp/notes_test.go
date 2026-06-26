package mcp

import (
	"encoding/json"
	"testing"

	"avairy/internal/board"
)

// note/read_notes go through the shared blackboard: alice writes, bob reads, latest-wins per key,
// and prefix filtering works.
func TestNoteAndReadNotes(t *testing.T) {
	s, _ := newTestServer(t)
	s.RegisterAgent("alice", nil, nil)
	s.RegisterAgent("bob", nil, nil)

	res, err := s.handleNote(asAgent("alice"), call(map[string]any{"key": "repro/linux", "text": "panics on linux only"}))
	if err != nil {
		t.Fatal(err)
	}
	mustText(t, res)
	s.handleNote(asAgent("bob"), call(map[string]any{"key": "decision/db", "text": "use postgres"}))
	s.handleNote(asAgent("alice"), call(map[string]any{"key": "repro/linux", "text": "...also needs qemu"})) // latest wins

	// bob reads everything back.
	res, _ = s.handleReadNotes(asAgent("bob"), call(nil))
	var all []board.Note
	if err := json.Unmarshal([]byte(mustText(t, res)), &all); err != nil {
		t.Fatalf("read_notes json: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 notes, got %d: %+v", len(all), all)
	}

	// prefix filter + latest-wins.
	res, _ = s.handleReadNotes(asAgent("bob"), call(map[string]any{"prefix": "repro/"}))
	var repro []board.Note
	if err := json.Unmarshal([]byte(mustText(t, res)), &repro); err != nil {
		t.Fatalf("read_notes prefix json: %v", err)
	}
	if len(repro) != 1 || repro[0].Text != "...also needs qemu" || repro[0].Author != "alice" {
		t.Fatalf("prefix read / latest-wins wrong: %+v", repro)
	}
}

// note requires both key and text.
func TestNoteRequiresKeyAndText(t *testing.T) {
	s, _ := newTestServer(t)
	s.RegisterAgent("alice", nil, nil)

	res, _ := s.handleNote(asAgent("alice"), call(map[string]any{"text": "no key"}))
	if !res.IsError {
		t.Fatal("expected error for missing key")
	}
	res, _ = s.handleNote(asAgent("alice"), call(map[string]any{"key": "k"}))
	if !res.IsError {
		t.Fatal("expected error for missing text")
	}
}
