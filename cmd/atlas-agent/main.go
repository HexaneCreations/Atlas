// Command atlas-agent collects the local host and pushes observations to an
// Atlas control plane over mTLS.
//
// Usage:
//
//	atlas-agent [-h|--help] [--version]
//	atlas-agent peer-id
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
	"github.com/hexane/atlas/internal/platform/lock"
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

	// peer-id prints this agent's persistent libp2p Peer ID and exits. It is
	// what an operator hands to `atlas-server peer authorize`; on the libp2p
	// transport that authorization, not an enrollment token, is what admits
	// this agent. Reads (or creates) the same identity file the agent itself
	// uses, so the value printed is the value it will dial with.
	if args := flags.Args(); len(args) > 0 && args[0] == "peer-id" {
		peerID, err := agent.PeerID(cfg.DataDir)
		if err != nil {
			return err
		}
		fmt.Println(peerID)
		return nil
	}

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

	// Acquired before any identity/certificate/spool/relationship state is
	// touched, and held for the process lifetime — see internal/platform/lock.
	agentLock, err := lock.Acquire(agent.LockPath(cfg.DataDir))
	if err != nil {
		return err
	}
	defer agentLock.Release()

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
  atlas-agent peer-id      print this host's libp2p Peer ID and exit

How the agent authenticates depends on its transport:

  libp2p   No token and no certificate. The agent's persistent Peer ID (see
           the peer-id command above) is its identity, proven by the libp2p
           Noise handshake. An operator authorizes it once, on the server:

             atlas-server peer authorize --peer-id <id> --node-id <node> \
               --environment <env>

  https    On first run, atlas-agent enrolls using ATLAS_AGENT_TOKEN and a
           locally generated keypair, then persists its certificate under
           ATLAS_AGENT_DATA_DIR. Every run after that reuses the persisted
           certificate and needs no token.

Environment variables:
  ATLAS_AGENT_CONTROL_PLANE_URL   Control plane base URL (default https://127.0.0.1:8443)
  ATLAS_AGENT_TOKEN               Enrollment token; https transport only, first run only.
                                  Not used, and not needed, on the libp2p transport.
  ATLAS_AGENT_DATA_DIR            Certificate and spool storage (default /var/lib/atlas-agent)
  ATLAS_AGENT_CA_BUNDLE           Path to a PEM CA certificate, for verified bootstrap.
  ATLAS_AGENT_INSECURE_BOOTSTRAP  Enroll with no CA bundle, trusting and pinning the
                                  certificate presented on first contact. Must be set
                                  explicitly; enrollment is otherwise refused when no
                                  CA bundle is configured and none is pinned yet.
  ATLAS_AGENT_NODE_ID             Pin the node id explicitly. Empty derives one
                                  from the OS machine id.
  ATLAS_AGENT_ENVIRONMENT         Operator-assigned environment tag
  ATLAS_AGENT_COLLECTION_INTERVAL Metric collection interval (default 15s)
  ATLAS_AGENT_COLLECTION_TIMEOUT  Per-collector timeout (default 10s)
  ATLAS_AGENT_INVENTORY_INTERVAL  Inventory push interval (default 60s)
  ATLAS_AGENT_SECRET_REDACTION_DISABLED
                                  Transmit process command lines and cron commands
                                  unredacted (default false — redaction is on).
  ATLAS_AGENT_LOG_LEVEL           info or debug (default info)

Multiple Control Planes (optional):
  The variables above always configure the implicit "default" relationship.
  To connect to additional control planes simultaneously (e.g. production and
  development), set:

  ATLAS_AGENT_RELATIONSHIPS      Comma-separated relationship ids, e.g.
                                  "production,development". "default" is
                                  reserved.

  For each id, the equivalent of every ATLAS_AGENT_* variable above is read
  from ATLAS_AGENT_RELATIONSHIP_<ID>_*, e.g.
  ATLAS_AGENT_RELATIONSHIP_PRODUCTION_CONTROL_PLANE_URL,
  ATLAS_AGENT_RELATIONSHIP_PRODUCTION_TOKEN (https only), and so on. These are
  consulted only until a relationship first bootstraps successfully — after
  that, its resolved configuration is persisted under its own data directory
  and is authoritative; the environment variables are ignored on later
  restarts.

Flags:
`)
		flags.PrintDefaults()
	}
}
