// Package board is avairy's shared task board + blackboard (DESIGN.md §4/§8), a
// materialized view over the journal. Tasks carry capability requirements used for
// node/agent matchmaking and for gating claims; the blackboard is durable shared memory.
package board

import (
	"errors"
	"maps"
	"sort"
	"strconv"
	"sync"

	"avairy/internal/journal"
)

// TaskState is a task's lifecycle state.
type TaskState string

const (
	TaskOpen       TaskState = "open"
	TaskClaimed    TaskState = "claimed"
	TaskInProgress TaskState = "in_progress"
	TaskBlocked    TaskState = "blocked"
	TaskDone       TaskState = "done"
	TaskFailed     TaskState = "failed"
)

// Task is a unit of work (DESIGN.md §4).
type Task struct {
	ID        string
	Title     string
	State     TaskState
	Requires  map[string]string // capability constraints, e.g. {"os":"linux"}
	Deps      []string
	Claimant  string
	CreatedBy string
}

func (t Task) clone() Task {
	c := t
	if t.Requires != nil {
		c.Requires = make(map[string]string, len(t.Requires))
		maps.Copy(c.Requires, t.Requires)
	}
	if t.Deps != nil {
		c.Deps = append([]string(nil), t.Deps...)
	}
	return c
}

var (
	ErrNotFound     = errors.New("board: task not found")
	ErrNotClaimable = errors.New("board: task not open")
	ErrCapMismatch  = errors.New("board: node does not meet task requirements")
)

// Board holds tasks. All mutations append to the journal.
type Board struct {
	jrnl journal.Log

	mu    sync.Mutex
	seq   uint64
	tasks map[string]*Task
}

// New returns an empty board recording to jrnl.
func New(jrnl journal.Log) *Board {
	return &Board{jrnl: jrnl, tasks: make(map[string]*Task)}
}

// Post adds a new open task.
func (b *Board) Post(createdBy, title string, requires map[string]string, deps []string) Task {
	b.mu.Lock()
	b.seq++
	t := &Task{
		ID:        "t" + strconv.FormatUint(b.seq, 10),
		Title:     title,
		State:     TaskOpen,
		Requires:  requires,
		Deps:      deps,
		CreatedBy: createdBy,
	}
	b.tasks[t.ID] = t
	out := t.clone()
	b.mu.Unlock()

	b.jrnl.Append(journal.KindTask, createdBy, out)
	return out
}

// Claim atomically assigns an open task to agentID, but only if caps satisfy Requires.
func (b *Board) Claim(taskID, agentID string, caps map[string]string) (Task, error) {
	b.mu.Lock()
	t, ok := b.tasks[taskID]
	if !ok {
		b.mu.Unlock()
		return Task{}, ErrNotFound
	}
	if t.State != TaskOpen {
		b.mu.Unlock()
		return Task{}, ErrNotClaimable
	}
	if !satisfies(caps, t.Requires) {
		b.mu.Unlock()
		return Task{}, ErrCapMismatch
	}
	t.State = TaskClaimed
	t.Claimant = agentID
	out := t.clone()
	b.mu.Unlock()

	// A claim is a handover (work changing hands) — recorded for the TUI timeline.
	b.jrnl.Append(journal.KindHandover, agentID, out)
	return out, nil
}

// SetState updates a task's state.
func (b *Board) SetState(taskID string, s TaskState) (Task, error) {
	b.mu.Lock()
	t, ok := b.tasks[taskID]
	if !ok {
		b.mu.Unlock()
		return Task{}, ErrNotFound
	}
	t.State = s
	out := t.clone()
	b.mu.Unlock()

	b.jrnl.Append(journal.KindTask, t.Claimant, out)
	return out, nil
}

// Get returns a task by id.
func (b *Board) Get(id string) (Task, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.tasks[id]; ok {
		return t.clone(), true
	}
	return Task{}, false
}

// List returns all tasks sorted by id.
func (b *Board) List() []Task {
	b.mu.Lock()
	out := make([]Task, 0, len(b.tasks))
	for _, t := range b.tasks {
		out = append(out, t.clone())
	}
	b.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// satisfies reports whether caps meets every constraint in requires.
func satisfies(caps, requires map[string]string) bool {
	for k, v := range requires {
		if caps[k] != v {
			return false
		}
	}
	return true
}
