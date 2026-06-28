package mcp

import (
	"context"
	"fmt"
	"strings"

	"avairy/internal/bus"
)

// addrString renders a bus address for the wire (read_inbox): "all"/"team" for the fan-out kinds,
// else "agent:<id>"/"role:<name>". Lets an agent see a message is a team request and claim it.
func addrString(a bus.Addr) string {
	switch a.Kind {
	case bus.ToBroadcast:
		return "all"
	case bus.ToTeam:
		return "team"
	default:
		return string(a.Kind) + ":" + a.Value
	}
}

// agentFromContext returns the caller agent id resolved by the HTTP transport.
func agentFromContext(ctx context.Context) string {
	v, _ := ctx.Value(agentKey).(string)
	return v
}

// parseAddr parses a bus address string: "broadcast"/"all", "team", "agent:<id>", or "role:<name>".
func parseAddr(to string) (bus.Addr, error) {
	switch to {
	case "broadcast", "all":
		return bus.Broadcast(), nil
	case "team":
		return bus.Team(), nil
	}
	kind, val, ok := strings.Cut(to, ":")
	if !ok || val == "" {
		return bus.Addr{}, fmt.Errorf("invalid address %q (want all | team | agent:<id> | role:<name>)", to)
	}
	switch kind {
	case "agent":
		return bus.Agent(val), nil
	case "role":
		return bus.Role(val), nil
	default:
		return bus.Addr{}, fmt.Errorf("unknown address kind %q", kind)
	}
}

// parseRequires turns ["os=linux","arch=arm64"] into a capability map.
func parseRequires(pairs []string) map[string]string {
	if len(pairs) == 0 {
		return nil
	}
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}
