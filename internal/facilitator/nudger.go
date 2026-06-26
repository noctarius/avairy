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
		// 1) Capability matchmaking: if the blocker text refers to a capability some other agent
		//    has (and the blocked one lacks), suggest handing off (the "better positioned" nudge).
		if peer, caps := bestPeer(roster, t.Agent, t.Detail); peer != "" {
			return []Nudge{{
				Kind: NudgeConsult,
				To:   t.Agent,
				Body: fmt.Sprintf("%s looks better positioned for this (%s) — consider handing "+
					"off the repro or asking them to try it.", peer, capStr(caps)),
			}}
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

// bestPeer finds the peer best positioned for a blocker: the one with the most capabilities the
// blocker text refers to (by value, synonyms, or key) that the blocked agent itself lacks. It's
// roster-driven — any declared cap (arch, qemu, gpu, docker, …), not just OS. Returns the peer id
// and the matched caps (for the nudge), or "" if none stands out.
func bestPeer(roster []Agent, blockedID, detail string) (string, map[string]string) {
	d := strings.ToLower(detail)
	var blocked map[string]string
	for _, a := range roster {
		if a.ID == blockedID {
			blocked = a.Caps
		}
	}
	bestID, bestScore := "", 0
	var bestCaps map[string]string
	for _, a := range sortedByID(roster) {
		if a.ID == blockedID {
			continue
		}
		matched := map[string]string{}
		for k, v := range a.Caps {
			if blocked[k] == v { // both have it → not differentiating
				continue
			}
			if capMentioned(d, k, v) {
				matched[k] = v
			}
		}
		if len(matched) > bestScore {
			bestID, bestScore, bestCaps = a.ID, len(matched), matched
		}
	}
	return bestID, bestCaps
}

// capMentioned reports whether lowercased blocker text refers to capability k=v — by the value,
// a common synonym, or the key name. Short/boolean values match only via the key (so "gpu=true"
// matches "needs a GPU", not the literal "true"); keys under 3 chars (e.g. "os") are ignored.
func capMentioned(detail, k, v string) bool {
	vl := strings.ToLower(v)
	terms := capSynonyms(vl)
	if len(vl) >= 3 && vl != "true" && vl != "false" && vl != "yes" && vl != "no" {
		terms = append(terms, vl)
	}
	terms = append(terms, strings.ToLower(k))
	for _, t := range terms {
		if len(t) >= 3 && strings.Contains(detail, t) {
			return true
		}
	}
	return false
}

// capSynonyms gives common alternate spellings for a capability value.
func capSynonyms(v string) []string {
	switch v {
	case "darwin":
		return []string{"darwin", "macos", "mac os", "osx"}
	case "windows":
		return []string{"windows", "win32"}
	case "arm64":
		return []string{"arm64", "aarch64", "apple silicon"}
	case "amd64":
		return []string{"amd64", "x86_64", "x86-64"}
	}
	return nil
}

func firstPeer(roster []Agent, exclude string) string {
	for _, a := range sortedByID(roster) {
		if a.ID != exclude {
			return a.ID
		}
	}
	return ""
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
