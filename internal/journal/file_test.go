package journal

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestFilePersistAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Append(KindMessage, "human", map[string]any{"body": "hello"})
	f.Append(KindAgentEvent, "alice", map[string]any{"text": "hi"})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// In-memory view is intact.
	if len(f.Records()) != 2 {
		t.Fatalf("in-memory records = %d", len(f.Records()))
	}

	// Durable file replays in order with decodable data.
	recs, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Seq != 1 || recs[0].Kind != KindMessage || recs[1].Actor != "alice" {
		t.Fatalf("read back: %+v", recs)
	}
	var data struct {
		Body string `json:"body"`
	}
	json.Unmarshal(recs[0].Data, &data)
	if data.Body != "hello" {
		t.Fatalf("data not preserved: %q", data.Body)
	}
}
