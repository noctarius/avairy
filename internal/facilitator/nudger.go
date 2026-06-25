package facilitator

import (
	"fmt"
	"sort"
	"strings"
)

// RuleNudger is the default, zero-LLM nudger. It implements the facilitator's core moves
// deterministically; an LLM-backed Nudger can replace it without touching trigger detection.
type RuleNudger struct{}

func (RuleNudger) Decide(t Trigger, roster []Agent) []Nudge {
	switch t.Kind {
	case TriggerLoop:
		return []Nudge{{
			Kind: NudgeRemind,
			To:   t.Agent,
			Body: "You seem to be repeating the same step. Try a fresh perspective — start an " +
				"ephemeral session for a clean look, or ask a peer for their opinion.",
		}}

	case TriggerBlocked:
		// 1) Capability matchmaking: if the blocker mentions an environment another agent
		//    has, suggest handing off to that agent (the flagship "better positioned" nudge).
		if need := neededCap(t.Detail); need != nil {
			if peer := firstCapable(roster, t.Agent, need); peer != "" {
				return []Nudge{{
					Kind: NudgeConsult,
					To:   t.Agent,
					Body: fmt.Sprintf("%s looks better positioned for this (%s) — consider handing "+
						"off the repro or asking them to try it.", peer, capStr(need)),
				}}
			}
		}
		// 2) Otherwise suggest consulting any available peer.
		if peer := firstPeer(roster, t.Agent); peer != "" {
			return []Nudge{{
				Kind: NudgeRemind,
				To:   t.Agent,
				Body: fmt.Sprintf("You reported being blocked. Consider asking %s for their opinion.", peer),
			}}
		}
		// 3) No peer can help → escalate to the human.
		return []Nudge{{Kind: NudgeEscalate, Body: "an agent is blocked and no peer can help: " + t.Detail}}
	}
	return nil
}

// neededCap maps free-text blocker detail to a required capability (OS keywords for now).
func neededCap(detail string) map[string]string {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "linux"):
		return map[string]string{"os": "linux"}
	case strings.Contains(d, "windows"):
		return map[string]string{"os": "windows"}
	case strings.Contains(d, "macos"), strings.Contains(d, "darwin"), strings.Contains(d, " mac"):
		return map[string]string{"os": "darwin"}
	}
	return nil
}

func firstCapable(roster []Agent, exclude string, need map[string]string) string {
	for _, a := range sortedByID(roster) {
		if a.ID == exclude {
			continue
		}
		if capMatch(a.Caps, need) {
			return a.ID
		}
	}
	return ""
}

func firstPeer(roster []Agent, exclude string) string {
	for _, a := range sortedByID(roster) {
		if a.ID != exclude {
			return a.ID
		}
	}
	return ""
}

func capMatch(have, need map[string]string) bool {
	for k, v := range need {
		if have[k] != v {
			return false
		}
	}
	return true
}

func capStr(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func sortedByID(roster []Agent) []Agent {
	out := append([]Agent(nil), roster...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
