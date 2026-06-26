package board

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"avairy/internal/journal"
)

// Blackboard is avairy's durable shared memory (DESIGN.md §4/§8): keyed notes any agent (or the
// human) can write and read — context, decisions, findings — feeding task context and the
// fresh-look prompt. Latest write per key wins. Like the task board, every write is journaled,
// so it resumes across a core restart (Restore).
type Blackboard struct {
	jrnl journal.Log

	mu    sync.Mutex
	notes map[string]*Note
}

// Note is one blackboard entry.
type Note struct {
	Key    string
	Text   string
	Author string
}

// NewBlackboard returns an empty blackboard recording to jrnl.
func NewBlackboard(jrnl journal.Log) *Blackboard {
	return &Blackboard{jrnl: jrnl, notes: make(map[string]*Note)}
}

// Write sets the note at key (latest write wins) and journals it.
func (bb *Blackboard) Write(author, key, text string) Note {
	n := &Note{Key: key, Text: text, Author: author}
	bb.mu.Lock()
	bb.notes[key] = n
	out := *n
	bb.mu.Unlock()

	bb.jrnl.Append(journal.KindNote, author, out)
	return out
}

// Read returns notes whose key starts with prefix (all if prefix is empty), sorted by key.
func (bb *Blackboard) Read(prefix string) []Note {
	bb.mu.Lock()
	out := make([]Note, 0, len(bb.notes))
	for k, n := range bb.notes {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			out = append(out, *n)
		}
	}
	bb.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Restore rebuilds the blackboard from a persisted journal (last write per key wins). Does not
// re-journal. Call once on startup.
func (bb *Blackboard) Restore(records []journal.PersistedRecord) {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	for _, r := range records {
		if r.Kind != journal.KindNote {
			continue
		}
		var n Note
		if json.Unmarshal(r.Data, &n) == nil && n.Key != "" {
			restored := n
			bb.notes[n.Key] = &restored
		}
	}
}
