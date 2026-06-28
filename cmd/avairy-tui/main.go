// Command avairy-tui is the operator console as a standalone client (DESIGN.md §3, item #18). It
// attaches to a remote core's operator API and renders the same TUI an attached `avairy` shows — so
// the operator can drive a headless core (`avairy -control-addr … -headless`) from another machine.
//
//	avairy-tui -join-file .avairy/operator-join     # one-arg attach (core URL + CA + token bundled)
//	avairy-tui -core https://core:7700 -token <tok> -ca core-ca.pem
//	avairy-tui -core http://localhost:7700 -token <tok>     # plain HTTP (dev)
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"avairy/internal/buildinfo"
	"avairy/internal/control"
	"avairy/internal/operator"
	"avairy/internal/tui"

	"flag"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(buildinfo.String())
		return
	}
	core := flag.String("core", "", "core control API base URL (e.g. https://host:7700); supplied by -join/-join-file")
	token := flag.String("token", "", "operator API bearer token (from core's startup output / TUI)")
	caFile := flag.String("ca", "", "PEM cert/CA to trust for an https core (self-signed/internal CA)")
	insecure := flag.Bool("insecure", false, "skip TLS verification for an https core (DEV ONLY — exposes the channel to MITM)")
	join := flag.String("join", "", "operator-join string from core (carries core URL + CA + token)")
	joinFile := flag.String("join-file", "", "file containing an operator-join string (e.g. core's .avairy/operator-join)")
	flag.Parse()

	// A join bundle supplies core URL + CA + token in one string, overriding the individual flags
	// (the same bundle machinery nodes use, written by core to .avairy/operator-join).
	var joinCA []byte
	if *join != "" || *joinFile != "" {
		raw := *join
		if raw == "" {
			b, err := os.ReadFile(*joinFile)
			if err != nil {
				fmt.Fprintln(os.Stderr, "avairy-tui: join-file:", err)
				os.Exit(1)
			}
			raw = strings.TrimSpace(string(b))
		}
		jb, err := control.DecodeJoin(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "avairy-tui:", err)
			os.Exit(1)
		}
		*core = jb.Core
		joinCA = jb.CA
		if jb.Token != "" {
			*token = jb.Token
		}
	}

	if *core == "" {
		fmt.Fprintln(os.Stderr, "avairy-tui: need -core (or -join/-join-file)")
		os.Exit(2)
	}

	// TLS trust: a join's CA takes precedence, else -ca / -insecure; a plain-http core needs none.
	httpClient := http.DefaultClient
	switch {
	case len(joinCA) > 0:
		c, err := control.TLSClientPEM(joinCA, *insecure, nil, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "avairy-tui: tls:", err)
			os.Exit(1)
		}
		httpClient = c
	case *caFile != "" || *insecure:
		c, err := control.TLSClient(*caFile, *insecure)
		if err != nil {
			fmt.Fprintln(os.Stderr, "avairy-tui: tls:", err)
			os.Exit(1)
		}
		httpClient = c
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := operator.Connect(ctx, *core, *token, httpClient)
	if err != nil {
		fmt.Fprintln(os.Stderr, "avairy-tui: connect:", err)
		os.Exit(1)
	}
	if err := tui.Run(client.Deps()); err != nil {
		fmt.Fprintln(os.Stderr, "avairy-tui:", err)
		os.Exit(1)
	}
}
