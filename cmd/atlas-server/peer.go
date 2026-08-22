package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hexane/atlas/internal/app"
	"github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/platform/postgres"
	storagefleet "github.com/hexane/atlas/internal/storage/fleet"
)

// peerCommand authorizes a libp2p Peer ID to act as a node, lists existing
// authorizations, or revokes one. See docs/adr/0012-connect-by-identity.md
// and migrations/0011_agent_peers.sql: on the libp2p transport this replaces
// the enrollment token entirely, and unlike a token it is not a secret, does
// not expire, and is revocable in place.
func peerCommand(configPath string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: atlas-server peer <authorize|list|revoke> [flags]")
	}
	switch args[0] {
	case "authorize":
		return peerAuthorize(configPath, args[1:])
	case "list":
		return peerList(configPath)
	case "revoke":
		return peerRevoke(configPath, args[1:])
	default:
		return fmt.Errorf("unknown peer command %q; want authorize, list or revoke", args[0])
	}
}

// withRepository opens the same database the server itself uses (same config
// resolution, same env vars) and hands the caller a repository over it.
func withRepository(configPath string, fn func(context.Context, *storagefleet.Repository) error) error {
	cfg, logger, err := app.LoadConfigAndLogger(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool := postgres.NewPool(cfg.Database, logger)
	if err := pool.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = pool.Stop(context.WithoutCancel(ctx)) }()

	return fn(ctx, storagefleet.NewRepository(pool.DB()))
}

func peerAuthorize(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server peer authorize", flag.ContinueOnError)
	peerID := flags.String("peer-id", "", "the agent's libp2p Peer ID, from its startup log or `atlas-agent peer-id` (required)")
	nodeID := flags.String("node-id", "", "the machine identity this peer speaks for (required)")
	environment := flags.String("environment", "", "environment this authorization is scoped to (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	spec := fleet.PeerSpec{
		PeerID: *peerID, NodeID: *nodeID, Environment: *environment, Role: fleet.PeerRoleAgent,
	}
	// Validated before a database connection is opened: a mistyped Peer ID
	// should fail immediately and locally, not after a connection attempt.
	if err := spec.Validate(); err != nil {
		return err
	}

	return withRepository(configPath, func(ctx context.Context, repo *storagefleet.Repository) error {
		if err := repo.RegisterPeer(ctx, spec); err != nil {
			return err
		}
		fmt.Printf("authorized peer %s as node %s in environment %s\n", spec.PeerID, spec.NodeID, spec.Environment)
		return nil
	})
}

func peerRevoke(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server peer revoke", flag.ContinueOnError)
	peerID := flags.String("peer-id", "", "the libp2p Peer ID to revoke (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *peerID == "" {
		return fmt.Errorf("--peer-id is required")
	}

	return withRepository(configPath, func(ctx context.Context, repo *storagefleet.Repository) error {
		if err := repo.RevokePeer(ctx, *peerID); err != nil {
			return err
		}
		fmt.Printf("revoked peer %s\n", *peerID)
		return nil
	})
}

func peerList(configPath string) error {
	return withRepository(configPath, func(ctx context.Context, repo *storagefleet.Repository) error {
		peers, err := repo.ListPeers(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "PEER ID\tNODE ID\tENVIRONMENT\tROLE\tSTATUS\tCREATED")
		for _, p := range peers {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				p.PeerID, p.NodeID, p.Environment, p.Role, p.Status, p.CreatedAt.Format(time.RFC3339))
		}
		return w.Flush()
	})
}
