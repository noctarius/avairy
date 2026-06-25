package mcp

import (
	"context"
	"fmt"
	"strings"

	"avairy/internal/bus"
)

// agentFromContext returns the caller agent id resolved by the HTTP transport.
func agentFromContext(ctx context.Context) string {
	v, _ := ctx.Value(agentKey).(string)
	return v
}

// parseAddr parses a bus address string: "broadcast", "agent:<id>", or "role:<name>".
func parseAddr(to string) (bus.Addr, error) {
	if to == "broadcast" {
		return bus.Broadcast(), nil
	}
	kind, val, ok := strings.Cut(to, ":")
	if !ok || val == "" {
		return bus.Addr{}, fmt.Errorf("invalid address %q (want broadcast | agent:<id> | role:<name>)", to)
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
