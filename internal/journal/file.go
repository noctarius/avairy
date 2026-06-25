package journal

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PersistedRecord is the on-disk form of a Record (Data as raw JSON for audit/replay).
type PersistedRecord struct {
	Seq   uint64          `json:"seq"`
	Time  time.Time       `json:"time"`
	Kind  Kind            `json:"kind"`
	Actor string          `json:"actor"`
	Data  json.RawMessage `json:"data"`
}

// File is a Log that also appends every record to an append-only JSONL file (DESIGN.md §10
// durability + audit trail). Live reads keep typed Data via the embedded Memory; the file is
// the durable artifact (read back with ReadFile). Typed state-resume on restart is a planned
// follow-up.
type File struct {
	*Memory
	mu sync.Mutex
	w  *os.File
}

// OpenFile opens (creating parent dirs) an append-only journal file.
func OpenFile(path string) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	w, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &File{Memory: NewMemory(), w: w}, nil
}

// Append records in memory and durably appends a JSONL line.
func (f *File) Append(kind Kind, actor string, data any) Record {
	rec := f.Memory.Append(kind, actor, data)
	raw, err := json.Marshal(data)
	if err != nil {
		raw = []byte("null")
	}
	line, _ := json.Marshal(PersistedRecord{Seq: rec.Seq, Time: rec.Time, Kind: rec.Kind, Actor: rec.Actor, Data: raw})
	f.mu.Lock()
	_, _ = f.w.Write(append(line, '\n'))
	f.mu.Unlock()
	return rec
}

// Close closes the underlying file.
func (f *File) Close() error { return f.w.Close() }

// ReadFile reads a persisted journal back in order (Data as raw JSON).
func ReadFile(path string) ([]PersistedRecord, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var out []PersistedRecord
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var pr PersistedRecord
		if err := json.Unmarshal(sc.Bytes(), &pr); err != nil {
			return out, err
		}
		out = append(out, pr)
	}
	return out, sc.Err()
}
