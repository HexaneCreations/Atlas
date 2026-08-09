// Command atlas-relay is Atlas Relay: a minimal circuit-relay-v2 bootstrap
// that lets two NAT'd Atlas peers (a control plane and its agents) reach
// each other by Peer ID. See docs/adr/0012-connect-by-identity.md.
//
// It runs on a publicly reachable host and carries no Atlas business logic —
// no Postgres, no HTTP API, no mTLS termination. It forwards encrypted
// streams between peers it never authenticates or decrypts.
//
// Usage:
//
//	atlas-relay [-h|--help] [--version]
//
// Configuration is read entirely from ATLAS_RELAY_ environment variables.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hexane/atlas/internal/platform/build"
	"github.com/hexane/atlas/internal/relay"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "atlas-relay: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("atlas-relay", flag.ContinueOnError)
	showVersion := flags.Bool("version", false, "print build information and exit")
	flags.Usage = usage(flags)

	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Println(build.Current())
		return nil
	}

	cfg := relay.LoadConfig()

	level := slog.LevelInfo
	if cfg.LogLevel == "debug" {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	r, err := relay.New(cfg, logger)
	if err != nil {
		return fmt.Errorf("initialise relay: %w", err)
	}

	return r.Run(ctx)
}

func usage(flags *flag.FlagSet) func() {
	return func() {
		fmt.Fprint(flags.Output(), `atlas-relay — circuit-relay-v2 bootstrap for identity-based connectivity

Usage:
  atlas-relay [flags]

Run on a publicly reachable host. On start it prints its Peer ID and
dialable multiaddrs — hand these to Atlas Server as
ATLAS_FLEET_LIBP2P_RELAY_ADDR.

Environment variables:
  ATLAS_RELAY_DATA_DIR       Peer ID keypair storage (default /var/lib/atlas-relay)
  ATLAS_RELAY_LISTEN_ADDRS   Comma-separated multiaddrs to listen on
                              (default /ip4/0.0.0.0/tcp/4103)
  ATLAS_RELAY_LOG_LEVEL      info or debug (default info)

Flags:
`)
		flags.PrintDefaults()
	}
}
