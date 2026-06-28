package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/urfave/cli/v3"

	"avairy/internal/control"
	"avairy/internal/operator"
	"avairy/internal/tui"
)

// tuiCommand groups the remote operator-console subcommands.
func tuiCommand() *cli.Command {
	return &cli.Command{
		Name:  "tui",
		Usage: "attach the operator console to a remote core",
		Commands: []*cli.Command{{
			Name:  "connect",
			Usage: "attach the operator console (the same TUI) to a remote core",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "core", Usage: "core control API base URL (e.g. https://host:7700); or via --join"},
				&cli.StringFlag{Name: "token", Usage: "operator API bearer token (from core's startup output / TUI)"},
				&cli.StringFlag{Name: "ca", Usage: "PEM cert/CA to trust for an https core (self-signed/internal CA)"},
				&cli.BoolFlag{Name: "insecure", Usage: "skip TLS verification for an https core (DEV ONLY — exposes the channel to MITM)"},
				&cli.StringFlag{Name: "join", Usage: "operator-join string (core URL + CA + token or cert)"},
				&cli.StringFlag{Name: "join-file", Usage: "file with an operator-join (core's .avairy/operator-join, or one from `core add-operator`)"},
			},
			Action: runTUIConnect,
		}},
	}
}

func runTUIConnect(_ context.Context, cmd *cli.Command) error {
	core := cmd.String("core")
	token := cmd.String("token")
	caFile := cmd.String("ca")
	insecure := cmd.Bool("insecure")

	// A join supplies core URL + CA + credential. From `core add-operator` it carries an mTLS
	// client cert (the operator identity); the convenience .avairy/operator-join carries a token.
	var joinCA, clientCert, clientKey []byte
	if cmd.String("join") != "" || cmd.String("join-file") != "" {
		jb, err := control.ReadJoin(cmd.String("join"), cmd.String("join-file"))
		if err != nil {
			return err
		}
		core = jb.Core
		joinCA = jb.CA
		if jb.Token != "" {
			token = jb.Token
		}
		clientCert, clientKey = jb.ClientCert, jb.ClientKey
	}
	if core == "" {
		return fmt.Errorf("need --core (or --join/--join-file)")
	}

	// TLS trust + optional operator client cert (mTLS). A join's CA/cert take precedence, else
	// --ca / --insecure; a plain-http core needs none.
	httpClient := http.DefaultClient
	switch {
	case len(clientCert) > 0 || len(joinCA) > 0:
		c, err := control.TLSClientPEM(joinCA, insecure, clientCert, clientKey)
		if err != nil {
			return fmt.Errorf("tls: %w", err)
		}
		httpClient = c
	case caFile != "" || insecure:
		c, err := control.TLSClient(caFile, insecure)
		if err != nil {
			return fmt.Errorf("tls: %w", err)
		}
		httpClient = c
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := operator.Connect(ctx, core, token, httpClient)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return tui.Run(client.Deps())
}
