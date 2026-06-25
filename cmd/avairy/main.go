// Command avairy is, for now, a headless demo of the single-machine collaboration loop:
// it wires a (mock) agent to the bus + journal, posts a task, sends the agent a message,
// and prints the resulting event-sourced journal. No agent credits are used.
//
// The real entrypoint (core + TUI) lands in a later milestone.
package main

import (
	"context"
	"fmt"
	"time"

	"avairy/internal/adapter/mock"
	"avairy/internal/agent"
	"avairy/internal/board"
	"avairy/internal/bus"
	"avairy/internal/journal"
	"avairy/internal/runner"
)

func main() {
	jrnl := journal.NewMemory()
	b := bus.New(jrnl)
	bd := board.New(jrnl)

	// Seed a task that requires a linux node (DESIGN.md §4 capability matchmaking).
	bd.Post("human", "reproduce the linux-only panic", map[string]string{"os": "linux"}, nil)

	// Start one mock agent on the bus.
	ad := mock.New()
	sess, err := ad.Start(context.Background(), agent.SessionConfig{AgentID: "alice", Role: "backend dev"})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := runner.New(runner.Agent{ID: "alice", Roles: []string{"backend"}}, sess, b, jrnl)
	go r.Run(ctx)

	// The human injects a message; the agent (mock) echoes it.
	b.Publish("human", bus.Agent("alice"), "alice, can you reproduce the panic on linux?", agent.DeliverySteer)

	time.Sleep(200 * time.Millisecond)
	cancel()
	_ = sess.Close()

	fmt.Println("=== tasks ===")
	for _, t := range bd.List() {
		fmt.Printf("  %s [%s] %q requires=%v\n", t.ID, t.State, t.Title, t.Requires)
	}
	fmt.Println("=== journal ===")
	for _, rec := range jrnl.Records() {
		fmt.Printf("  #%d %-12s actor=%-10s %s\n", rec.Seq, rec.Kind, rec.Actor, summarize(rec.Data))
	}
}

func summarize(data any) string {
	switch v := data.(type) {
	case bus.Message:
		return fmt.Sprintf("%s -> %s/%s: %q", v.From, v.To.Kind, v.To.Value, v.Body)
	case agent.Event:
		if v.Tool != nil {
			return fmt.Sprintf("%s tool=%s", v.Type, v.Tool.Name)
		}
		return fmt.Sprintf("%s %q", v.Type, v.Text)
	case board.Task:
		return fmt.Sprintf("task %s [%s] %q", v.ID, v.State, v.Title)
	default:
		return fmt.Sprintf("%v", data)
	}
}
