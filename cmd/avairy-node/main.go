// Command avairy-node is the avairy node daemon: a single cross-platform binary that
// enrolls with core, serves a local MCP proxy for agents on this machine, continuously
// syncs a workspace directory to/from the canonical hub, and heartbeats. It dials core
// (node→core outbound, NAT-friendly); the channel is HTTP here and TLS in production.
//
//	avairy-node -core http://core:7700 -core-mcp http://core:7701 -token <T> \
//	            -id linux-box -agent alice -workspace ./repo -proxy 127.0.0.1:7800
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"avairy/internal/control"
)

func main() {
	core := flag.String("core", "", "core control API base URL (required)")
	coreMCP := flag.String("core-mcp", "", "core MCP bus base URL for the local proxy")
	token := flag.String("token", "", "one-time enrollment token (required)")
	id := flag.String("id", "", "node id (required)")
	agentID := flag.String("agent", "", "agent id this node hosts (for the MCP proxy identity)")
	osName := flag.String("os", runtime.GOOS, "node OS capability")
	ws := flag.String("workspace", "", "workspace directory to sync (optional)")
	proxy := flag.String("proxy", "127.0.0.1:7800", "local MCP proxy listen address")
	interval := flag.Duration("interval", 2*time.Second, "sync/heartbeat interval")
	flag.Parse()

	if *core == "" || *token == "" || *id == "" {
		fmt.Fprintln(os.Stderr, "avairy-node: -core, -token and -id are required")
		os.Exit(2)
	}

	// The local workspace is this node's synced copy; create it if absent (it gets populated
	// by SyncDown from the canonical hub).
	if *ws != "" {
		if err := os.MkdirAll(*ws, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node: workspace:", err)
			os.Exit(1)
		}
	}

	n := control.NewNode(*core, *id)
	if err := n.Enroll(*token, *osName, map[string]string{"os": *osName}); err != nil {
		fmt.Fprintln(os.Stderr, "avairy-node: enroll:", err)
		os.Exit(1)
	}
	fmt.Printf("enrolled node %q (os=%s) with core %s\n", *id, *osName, *core)

	// Local MCP proxy → core bus, stamping this node's agent identity.
	if *coreMCP != "" && *agentID != "" {
		h, err := n.MCPProxy(*coreMCP, *agentID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "avairy-node: proxy:", err)
			os.Exit(1)
		}
		go func() {
			fmt.Printf("MCP proxy for agent %q at http://%s/mcp → %s\n", *agentID, *proxy, *coreMCP)
			if err := http.ListenAndServe(*proxy, h); err != nil {
				fmt.Fprintln(os.Stderr, "avairy-node: proxy server:", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("avairy-node: shutting down")
			return
		case <-ticker.C:
			if err := n.Heartbeat(); err != nil {
				fmt.Fprintln(os.Stderr, "heartbeat:", err)
			}
			if *ws == "" {
				continue
			}
			conflicts, err := n.SyncUp(*ws)
			if err != nil {
				fmt.Fprintln(os.Stderr, "syncUp:", err)
			}
			for _, c := range conflicts {
				fmt.Printf("CONFLICT %s (hub v%d) — needs reconciliation\n", c.Path, c.HubVersion)
			}
			if err := n.SyncDown(*ws); err != nil {
				fmt.Fprintln(os.Stderr, "syncDown:", err)
			}
		}
	}
}
