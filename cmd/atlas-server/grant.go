package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/hexane/atlas/internal/core/fleet"
	storagefleet "github.com/hexane/atlas/internal/storage/fleet"
)

// knownOperations is the set of AgentOps operations a grant may name. A
// typo'd --operation should fail locally rather than silently writing a
// grant nothing ever checks.
var knownOperations = map[string]bool{
	fleet.OperationContainerLogs: true,
}

func validateOperation(op string) error {
	if op == "" {
		return fmt.Errorf("--operation is required")
	}
	if !knownOperations[op] {
		return fmt.Errorf("unknown operation %q; want one of: container_logs", op)
	}
	return nil
}

// grantCommand authorizes or revokes an operator-granted AgentOps operation
// for a node, independent of how that node authenticates (HTTPS enrollment
// or libp2p peer authorization). See internal/core/fleet/grants.go.
func grantCommand(configPath string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: atlas-server grant <authorize|revoke> [flags]")
	}
	switch args[0] {
	case "authorize":
		return grantAuthorize(configPath, args[1:])
	case "revoke":
		return grantRevoke(configPath, args[1:])
	default:
		return fmt.Errorf("unknown grant command %q; want authorize or revoke", args[0])
	}
}

func grantAuthorize(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server grant authorize", flag.ContinueOnError)
	nodeID := flags.String("node-id", "", "the node id to authorize the operation for (required)")
	operation := flags.String("operation", "", "the AgentOps operation to authorize, e.g. container_logs (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *nodeID == "" {
		return fmt.Errorf("--node-id is required")
	}
	if err := validateOperation(*operation); err != nil {
		return err
	}

	return withRepository(configPath, func(ctx context.Context, repo *storagefleet.Repository) error {
		if err := repo.Grant(ctx, *nodeID, *operation, "operator", time.Now()); err != nil {
			return err
		}
		fmt.Printf("granted %s to node %s\n", *operation, *nodeID)
		return nil
	})
}

func grantRevoke(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server grant revoke", flag.ContinueOnError)
	nodeID := flags.String("node-id", "", "the node id to revoke the operation from (required)")
	operation := flags.String("operation", "", "the AgentOps operation to revoke, e.g. container_logs (required)")
	reason := flags.String("reason", "manual revocation", "reason recorded against the revocation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *nodeID == "" {
		return fmt.Errorf("--node-id is required")
	}
	if err := validateOperation(*operation); err != nil {
		return err
	}

	return withRepository(configPath, func(ctx context.Context, repo *storagefleet.Repository) error {
		if err := repo.RevokeGrant(ctx, *nodeID, *operation, *reason, time.Now()); err != nil {
			return err
		}
		fmt.Printf("revoked %s from node %s\n", *operation, *nodeID)
		return nil
	})
}
