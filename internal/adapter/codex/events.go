package codex

import (
	"encoding/json"

	"avairy/internal/agent"
)

// OnNotification maps a Codex app-server notification to normalized agent events.
func (s *session) OnNotification(method string, params json.RawMessage) {
	switch method {
	case "item/completed":
		var p struct {
			Item json.RawMessage `json:"item"`
		}
		if json.Unmarshal(params, &p) == nil {
			if ev, ok := itemToEvent(p.Item); ok {
				s.emit(ev)
			}
		}
	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(params, &p) == nil && p.Turn.ID != "" {
			s.mu.Lock()
			if s.activeTurn == "" {
				s.activeTurn = p.Turn.ID
			}
			s.mu.Unlock()
		}
	case "turn/completed":
		s.clearTurn()
		s.emit(agent.Event{Type: agent.EventTurnDone, Raw: cloneRaw(params)})
	case "error", "warning", "guardianWarning":
		var p struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(params, &p)
		s.emit(agent.Event{Type: agent.EventError, Text: p.Message, Raw: cloneRaw(params)})
	}
	// item/started, deltas, token-usage, and other notifications are ignored for now;
	// full text/tool events arrive on item/completed.
}

// itemToEvent maps a completed ThreadItem to an agent event, or (zero, false) to skip.
func itemToEvent(raw json.RawMessage) (agent.Event, bool) {
	var it struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &it) != nil {
		return agent.Event{}, false
	}
	switch it.Type {
	case "agentMessage":
		return agent.Event{Type: agent.EventText, Text: it.Text, Raw: cloneRaw(raw)}, true
	case "reasoning":
		return agent.Event{Type: agent.EventReasoning, Text: it.Text, Raw: cloneRaw(raw)}, true
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "webSearch":
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return agent.Event{
			Type: agent.EventToolUse,
			Tool: &agent.ToolCall{ID: it.ID, Name: it.Type, Input: m},
			Raw:  cloneRaw(raw),
		}, true
	default:
		return agent.Event{}, false
	}
}

func (s *session) emit(ev agent.Event) {
	select {
	case s.events <- ev:
	case <-s.peer.Done:
	}
}

func cloneRaw(b json.RawMessage) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
