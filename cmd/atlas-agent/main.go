// Command atlas-agent collects the local host and pushes observations to an
// Atlas control plane over mTLS.
//
// Usage:
//
//	atlas-agent [-h|--help] [--version]
//
// Configuration is read entirely from ATLAS_AGENT_ environment variables; see
// -h for the full list. On first run it enrolls using ATLAS_AGENT_TOKEN and
// persists its certificate under ATLAS_AGENT_DATA_DIR for every run after.
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

	"github.com/hexane/atlas/internal/agent"
	"github.com/hexane/atlas/internal/platform/build"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "atlas-agent: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("atlas-agent", flag.ContinueOnError)
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

	cfg := agent.LoadConfig()

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

	a, err := agent.New(ctx, cfg, logger)
	if err != nil {
		return fmt.Errorf("initialise agent: %w", err)
	}

	return a.Run(ctx)
}

func usage(flags *flag.FlagSet) func() {
	return func() {
		fmt.Fprint(flags.Output(), `atlas-agent — collects this host and pushes to an Atlas control plane

Usage:
  atlas-agent [flags]

On first run, atlas-agent enrolls with the control plane using
ATLAS_AGENT_TOKEN and a locally generated keypair, then persists its
certificate under ATLAS_AGENT_DATA_DIR. Every run after that reuses the
persisted certificate and needs no token.

Environment variables:
  ATLAS_AGENT_CONTROL_PLANE_URL   Control plane base URL (default https://127.0.0.1:8443)
  ATLAS_AGENT_TOKEN               Enrollment token; required only on first run
  ATLAS_AGENT_DATA_DIR            Certificate and spool storage (default /var/lib/atlas-agent)
  ATLAS_AGENT_CA_BUNDLE           Path to a PEM CA certificate, for verified bootstrap.
                                  Omit to trust-on-first-use and pin the CA the
                                  control plane returns during enrollment.
  ATLAS_AGENT_NODE_ID             Pin the node id explicitly. Empty derives one
                                  from the OS machine id.
  ATLAS_AGENT_ENVIRONMENT         Operator-assigned environment tag
  ATLAS_AGENT_COLLECTION_INTERVAL Metric collection interval (default 15s)
  ATLAS_AGENT_COLLECTION_TIMEOUT  Per-collector timeout (default 10s)
  ATLAS_AGENT_INVENTORY_INTERVAL  Inventory push interval (default 60s)
  ATLAS_AGENT_LOG_LEVEL           info or debug (default info)

Flags:
`)
		flags.PrintDefaults()
	}
}
