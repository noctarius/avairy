package claudecode

import (
	"encoding/json"

	"avairy/internal/agent"
)

// rawLine is the discriminated-union envelope for a single stream-json output line.
// Field set verified against Claude Code 2.1.176 (see ADAPTERS.md).
type rawLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init
	SessionID string `json:"session_id"`
	Model     string `json:"model"`

	// assistant / user
	Message *rawMessage `json:"message"`

	// result (turn done)
	IsError      bool      `json:"is_error"`
	Result       string    `json:"result"`
	StopReason   string    `json:"stop_reason"`
	TotalCostUSD float64   `json:"total_cost_usd"`
	Usage        *rawUsage `json:"usage"`
}

type rawMessage struct {
	Role    string     `json:"role"`
	Content []rawBlock `json:"content"`
	Usage   *rawUsage  `json:"usage"`
}

type rawBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text"`

	// thinking (extended thinking blocks; redacted_thinking carries no readable text)
	Thinking string `json:"thinking"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type rawUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// parseLine maps one stream-json output line to normalized events. It also returns the
// session id when the line carries one (system/init), so the Session can record it. Lines
// that yield no events (e.g. rate_limit_event) return a nil slice.
func parseLine(line []byte) (events []agent.Event, sessionID string) {
	var r rawLine
	if err := json.Unmarshal(line, &r); err != nil {
		return []agent.Event{{Type: agent.EventError, Text: "unparseable stream line: " + err.Error(), Raw: clone(line)}}, ""
	}

	switch r.Type {
	case "system":
		// init carries the session id; emit no event but surface the id.
		return nil, r.SessionID

	case "assistant":
		if r.Message == nil {
			return nil, ""
		}
		for _, b := range r.Message.Content {
			switch b.Type {
			case "text":
				events = append(events, agent.Event{Type: agent.EventText, Text: b.Text, Raw: clone(line)})
			case "thinking":
				events = append(events, agent.Event{Type: agent.EventReasoning, Text: b.Thinking, Raw: clone(line)})
			case "redacted_thinking":
				// Encrypted reasoning the model emitted but we can't read — record that it happened.
				events = append(events, agent.Event{Type: agent.EventReasoning, Text: "[redacted thinking]", Raw: clone(line)})
			case "tool_use":
				events = append(events, agent.Event{
					Type: agent.EventToolUse,
					Tool: &agent.ToolCall{ID: b.ID, Name: b.Name, Input: decodeInput(b.Input)},
					Raw:  clone(line),
				})
			}
		}
		return events, ""

	case "user":
		if r.Message == nil {
			return nil, ""
		}
		for _, b := range r.Message.Content {
			if b.Type == "tool_result" {
				events = append(events, agent.Event{
					Type: agent.EventToolResult,
					Tool: &agent.ToolCall{ID: b.ToolUseID, Result: string(b.Content)},
					Raw:  clone(line),
				})
			}
		}
		return events, ""

	case "result":
		ev := agent.Event{Type: agent.EventTurnDone, Raw: clone(line)}
		if r.Usage != nil {
			ev.Usage = &agent.Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens, CostUSD: r.TotalCostUSD}
		} else if r.TotalCostUSD != 0 {
			ev.Usage = &agent.Usage{CostUSD: r.TotalCostUSD}
		}
		if r.IsError || r.Subtype == "error" {
			return []agent.Event{{Type: agent.EventError, Text: r.Result, Raw: clone(line)}, ev}, ""
		}
		return []agent.Event{ev}, ""

	default:
		// rate_limit_event and any future/unknown types are dropped (not surfaced as events).
		return nil, ""
	}
}

func decodeInput(b json.RawMessage) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

func clone(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
