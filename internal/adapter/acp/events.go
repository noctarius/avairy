package acp

import (
	"context"
	"encoding/json"
	"strings"

	"avairy/internal/agent"
	"avairy/internal/gating"
)

// OnNotification maps an ACP session/update to normalized agent events.
func (s *session) OnNotification(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var p struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Content       json.RawMessage `json:"content"` // a ContentBlock (chunks) or ToolCallContent[] (tool calls)
			ToolCallID    string          `json:"toolCallId"`
			Title         string          `json:"title"`
			Kind          string          `json:"kind"`
			Status        string          `json:"status"`
			RawInput      map[string]any  `json:"rawInput"` // agent-specific args (command, path, …)
			Locations     []struct {
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
		s.appendText(acpText(u.Content))
	case "agent_thought_chunk":
		if t := acpText(u.Content); t != "" {
			s.emit(agent.Event{Type: agent.EventReasoning, Text: t})
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
		// An edit's diff rides the tool call's content ({type:"diff", oldText, newText}); fold it in
		// as edit fields so the transcript's diff control (agent.ToolDiff) can render it.
		if df := acpDiffInput(u.Content); df != nil {
			if input == nil {
				input = map[string]any{}
			}
			for k, v := range df {
				input[k] = v
			}
		}
		s.emit(agent.Event{Type: agent.EventToolUse, Tool: &agent.ToolCall{ID: u.ToolCallID, Name: name, Input: input}, Raw: cloneRaw(params)})
	case "tool_call_update":
		if u.Status == "completed" || u.Status == "failed" {
			s.emit(agent.Event{Type: agent.EventToolResult, Tool: &agent.ToolCall{ID: u.ToolCallID, Result: u.Status}, Raw: cloneRaw(params)})
		}
	}
}

// OnServerRequest answers agent→client requests. The only one we act on is permission;
// fs/* are refused (we advertised no fs capability) but must still be answered.
func (s *session) OnServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	if method == "session/request_permission" {
		s.handlePermission(id, params)
		return
	}
	_ = s.peer.Write(rpcResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{}})
}

type permOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // allow_once | allow_always | reject_once | reject_always
}

func (s *session) handlePermission(id json.RawMessage, params json.RawMessage) {
	var p struct {
		ToolCall struct {
			Title    string          `json:"title"`
			Kind     string          `json:"kind"`
			RawInput map[string]any  `json:"rawInput"`
			Content  json.RawMessage `json:"content"` // ToolCallContent[]; an edit carries a diff block
		} `json:"toolCall"`
		Options []permOption `json:"options"`
	}
	_ = json.Unmarshal(params, &p)

	// Fold an edit's content diff into rawInput so the approval carries a reviewable diff (#7).
	rawInput := p.ToolCall.RawInput
	if df := acpDiffInput(p.ToolCall.Content); df != nil {
		if rawInput == nil {
			rawInput = map[string]any{}
		}
		for k, v := range df {
			rawInput[k] = v
		}
	}

	decision := gating.Allow
	if s.decide != nil {
		d, err := s.decide(context.Background(), permToRequest(p.ToolCall.Kind, p.ToolCall.Title, rawInput))
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
	_ = s.peer.Write(rpcResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{"outcome": outcome}})
}

// acpText pulls the text out of a single ACP ContentBlock ({type:"text", text}). Returns "" for a
// non-text/array content (e.g. a tool call's ToolCallContent[]), which is harmless here.
func acpText(raw json.RawMessage) string {
	var c struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &c)
	return c.Text
}

// acpDiffInput extracts an ACP diff ToolCallContent ({type:"diff", path, oldText, newText}) from a
// tool call's content array, as edit fields (old_string/new_string/file_path) that PatchPreview and
// ToolDiff understand. nil when the content carries no diff block.
func acpDiffInput(raw json.RawMessage) map[string]any {
	var blocks []struct {
		Type    string `json:"type"`
		Path    string `json:"path"`
		OldText string `json:"oldText"`
		NewText string `json:"newText"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	for _, b := range blocks {
		if b.Type == "diff" {
			m := map[string]any{"old_string": b.OldText, "new_string": b.NewText}
			if b.Path != "" {
				m["file_path"] = b.Path
			}
			return m
		}
	}
	return nil
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
	case "edit", "move":
		// gated only with -gate-edits; surface the diff for the operator when rawInput carries one.
		return gating.Request{Kind: gating.ActionFileWrite, Summary: title, Diff: agent.PatchPreview("edit", rawInput)}
	default: // read, search, fetch, think, other → read-only, never gated
		return gating.Request{Kind: gating.ActionRead, Summary: title}
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
	if s.loading.Load() {
		return // history replayed during session/load — already in our journal, don't re-emit
	}
	select {
	case s.events <- ev:
	case <-s.peer.Done:
	}
}
