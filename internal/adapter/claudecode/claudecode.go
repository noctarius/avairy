// Package claudecode implements an agent.Adapter that drives Claude Code as a persistent
// streaming session via `claude -p --input-format stream-json --output-format stream-json`.
// Schema verified against 2.1.176 (see ADAPTERS.md).
package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"avairy/internal/agent"
)

// maxLine bounds a single stream-json line; the system/init line alone is several KB and
// tool results can be large.
const maxLine = 8 << 20 // 8 MiB

// Adapter drives the Claude Code CLI.
type Adapter struct {
	Bin       string   // claude binary; defaults to "claude"
	ExtraArgs []string // appended to every invocation
}

// New returns an Adapter using the `claude` binary from PATH.
func New() *Adapter { return &Adapter{Bin: "claude"} }

func (a *Adapter) Family() agent.Family { return agent.FamilyClaudeCode }

func (a *Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		// CLI interrupt-via-control-message is unverified; treat as steer-only for now.
		SupportsInterrupt: false,
		SupportsSteer:     true,
		SupportsResume:    true,
		MCPClient:         true,
		Enforcement:       agent.EnforcementHooked, // PreToolUse hook (wired by gating backend)
	}
}

func (a *Adapter) bin() string {
	if a.Bin == "" {
		return "claude"
	}
	return a.Bin
}

// Start launches a streaming Claude Code session. Worker agents are launched lean (explicit
// role via --append-system-prompt) — see the cost note in ADAPTERS.md.
func (a *Adapter) Start(ctx context.Context, cfg agent.SessionConfig) (agent.Session, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if cfg.Role != "" {
		args = append(args, "--append-system-prompt", cfg.Role)
	}
	if cfg.ResumeID != "" {
		args = append(args, "--resume", cfg.ResumeID)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if mcp := mcpConfigJSON(cfg.MCP); mcp != "" {
		args = append(args, "--mcp-config", mcp, "--strict-mcp-config")
	}
	args = append(args, a.ExtraArgs...)

	cmd := exec.CommandContext(ctx, a.bin(), args...)
	cmd.Dir = cfg.Workspace

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("claudecode: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claudecode: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claudecode: start %s: %w", a.bin(), err)
	}

	s := &session{
		cmd:    cmd,
		stdin:  stdin,
		enc:    json.NewEncoder(stdin),
		events: make(chan agent.Event, 64),
	}
	go s.readLoop(stdout)
	return s, nil
}

// session is one running Claude Code conversation.
type session struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	enc   *json.Encoder

	events chan agent.Event

	mu sync.RWMutex
	id string
}

func (s *session) ID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.id
}

func (s *session) setID(id string) {
	s.mu.Lock()
	if s.id == "" {
		s.id = id
	}
	s.mu.Unlock()
}

// inputMessage is the stream-json input envelope. Content is sent as a plain string, which
// Claude Code accepts (verified against 2.1.176).
type inputMessage struct {
	Type    string           `json:"type"`
	Message inputMessageBody `json:"message"`
}

type inputMessageBody struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *session) Send(ctx context.Context, text string, d agent.Delivery) error {
	if d == agent.DeliveryInterrupt {
		return errors.New("claudecode: interrupt delivery not supported; use steer")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(inputMessage{
		Type:    "user",
		Message: inputMessageBody{Role: "user", Content: text},
	})
}

func (s *session) Events() <-chan agent.Event { return s.events }

func (s *session) Interrupt(ctx context.Context) error {
	return errors.New("claudecode: interrupt not supported")
}

func (s *session) Close() error {
	_ = s.stdin.Close()
	return s.cmd.Wait()
}

func (s *session) readLoop(r io.Reader) {
	defer close(s.events)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		evs, sid := parseLine(line)
		if sid != "" {
			s.setID(sid)
		}
		for _, e := range evs {
			s.events <- e
		}
	}
	if err := sc.Err(); err != nil {
		s.events <- agent.Event{Type: agent.EventError, Text: "stream read error: " + err.Error()}
	}
}

// mcpConfigJSON renders the MCP servers as the inline JSON --mcp-config accepts. Returns ""
// when there are none.
func mcpConfigJSON(servers []agent.MCPServer) string {
	if len(servers) == 0 {
		return ""
	}
	type stdioCfg struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args,omitempty"`
	}
	type httpCfg struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	out := map[string]map[string]any{"mcpServers": {}}
	for _, srv := range servers {
		switch srv.Type {
		case "http":
			out["mcpServers"][srv.Name] = httpCfg{Type: "http", URL: srv.URL}
		default:
			out["mcpServers"][srv.Name] = stdioCfg{Type: "stdio", Command: srv.Command, Args: srv.Args}
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}
