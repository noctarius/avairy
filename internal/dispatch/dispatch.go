// Package dispatch holds the @facilitator routing decision (DESIGN.md §5): given the worker roster
// and an optional LLM picker, decide which single agent (or the whole team) should own an operator
// request. Pure logic so it's testable without a bus or a live model; the caller publishes the
// resulting address and journals the rule.
package dispatch

import "avairy/internal/bus"

// Decision is where a request should go and why (the rule is journaled for the operator).
type Decision struct {
	To   bus.Addr
	Rule string // "no-agents" | "sole-candidate" | "matched" | "team"
}

// Decide runs the cascade (rules → LLM fallback):
//   - no workers              → no target (Rule "no-agents").
//   - exactly one worker      → assign it (Rule "sole-candidate"), no LLM.
//   - several workers + pick  → if pick returns a known worker id, assign it ("matched");
//     otherwise (pick nil, "team", empty, or an unknown id) open a @team claim ("team").
//
// pick is the (LLM) chooser; nil when no model is available, in which case several workers always
// fall through to @team. ok is false only when there is nobody to route to.
func Decide(workers []string, pick func() string) (d Decision, ok bool) {
	switch len(workers) {
	case 0:
		return Decision{Rule: "no-agents"}, false
	case 1:
		return Decision{To: bus.Agent(workers[0]), Rule: "sole-candidate"}, true
	}
	if pick != nil {
		choice := pick()
		for _, w := range workers {
			if w == choice {
				return Decision{To: bus.Agent(choice), Rule: "matched"}, true
			}
		}
	}
	return Decision{To: bus.Team(), Rule: "team"}, true
}
