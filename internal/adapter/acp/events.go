package acp

import (
	"context"
	"encoding/json"
	"strings"

	"avairy/internal/agent"
	"avairy/internal/gating"
)

// handleNotification maps an ACP session/update to normalized agent events.
func (s *session) handleNotification(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var p struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			ToolCallID string         `json:"toolCallId"`
			Title      string         `json:"title"`
			Kind       string         `json:"kind"`
			Status     string         `json:"status"`
			RawInput   map[string]any `json:"rawInput"` // agent-specific args (command, path, …)
			Locations  []struct {
				Path string `json:"path"`
			} `json:"locations"` // files the call touches
		} `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	u := p.Update
	switch u.SessionUpdate {
	case "agent_message_chunk":
		s.appendText(u.Content.Text)
	case "agent_thought_chunk":
		if u.Content.Text != "" {
			s.emit(agent.Event{Type: agent.EventReasoning, Text: u.Content.Text})
		}
	case "tool_call":
		s.flushText() // close out any assistant text before the tool event
		name := u.Title
		if name == "" {
			name = u.Kind
		}
		// Carry the call's args so the TUI shows what it's doing and loop detection can tell
		// distinct actions apart (ACP previously emitted the name only). rawInput when present,
		// else the first touched file path — agents vary in which they send.
		input := u.RawInput
		if len(u.Locations) > 0 && u.Locations[0].Path != "" {
			if input == nil {
				input = map[string]any{}
			}
			if _, ok := input["path"]; !ok {
				input["path"] = u.Locations[0].Path
			}
		}
		s.emit(agent.Event{Type: agent.EventToolUse, Tool: &agent.ToolCall{ID: u.ToolCallID, Name: name, Input: input}, Raw: cloneRaw(params)})
	case "tool_call_update":
		if u.Status == "completed" || u.Status == "failed" {
			s.emit(agent.Event{Type: agent.EventToolResult, Tool: &agent.ToolCall{ID: u.ToolCallID, Result: u.Status}, Raw: cloneRaw(params)})
		}
	}
}

// handleServerRequest answers agent→client requests. The only one we act on is permission;
// fs/* are refused (we advertised no fs capability) but must still be answered.
func (s *session) handleServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	if method == "session/request_permission" {
		s.handlePermission(id, params)
		return
	}
	_ = s.write(rpcResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{}})
}

type permOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // allow_once | allow_always | reject_once | reject_always
}

func (s *session) handlePermission(id json.RawMessage, params json.RawMessage) {
	var p struct {
		ToolCall struct {
			Title    string         `json:"title"`
			Kind     string         `json:"kind"`
			RawInput map[string]any `json:"rawInput"`
		} `json:"toolCall"`
		Options []permOption `json:"options"`
	}
	_ = json.Unmarshal(params, &p)

	decision := gating.Allow
	if s.decide != nil {
		d, err := s.decide(context.Background(), permToRequest(p.ToolCall.Kind, p.ToolCall.Title, p.ToolCall.RawInput))
		if err != nil {
			d = gating.Deny
		}
		decision = d
	}

	var outcome map[string]any
	if optID := pickOption(p.Options, decision); optID != "" {
		outcome = map[string]any{"outcome": "selected", "optionId": optID}
	} else {
		outcome = map[string]any{"outcome": "cancelled"}
	}
	_ = s.write(rpcResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{"outcome": outcome}})
}

// permToRequest maps an ACP tool-call kind to a gating Request for the §7 policy.
func permToRequest(kind, title string, rawInput map[string]any) gating.Request {
	switch kind {
	case "execute":
		cmd, _ := rawInput["command"].(string)
		if cmd == "" {
			cmd = title
		}
		return gating.Request{Kind: gating.ActionCommand, Summary: cmd}
	case "delete":
		return gating.Request{Kind: gating.ActionGitMutate, Summary: title} // force-gated
	default: // read, edit, move, search, fetch, think, other → treated as free
		return gating.Request{Kind: gating.ActionFileWrite, Summary: title}
	}
}

// pickOption selects the option id whose kind matches the decision.
func pickOption(opts []permOption, d gating.Decision) string {
	var want []string
	prefix := "allow"
	switch d {
	case gating.Allow:
		want = []string{"allow_once", "allow_always"}
	case gating.AllowForSession:
		want = []string{"allow_always", "allow_once"}
	default:
		want, prefix = []string{"reject_once", "reject_always"}, "reject"
	}
	for _, w := range want {
		for _, o := range opts {
			if o.Kind == w {
				return o.OptionID
			}
		}
	}
	for _, o := range opts {
		if strings.HasPrefix(o.Kind, prefix) {
			return o.OptionID
		}
	}
	return ""
}

func (s *session) appendText(t string) {
	s.msgMu.Lock()
	s.msgBuf += t
	s.msgMu.Unlock()
}

func (s *session) flushText() {
	s.msgMu.Lock()
	t := s.msgBuf
	s.msgBuf = ""
	s.msgMu.Unlock()
	if t != "" {
		s.emit(agent.Event{Type: agent.EventText, Text: t})
	}
}

func (s *session) emit(ev agent.Event) {
	select {
	case s.events <- ev:
	case <-s.done:
	}
}
