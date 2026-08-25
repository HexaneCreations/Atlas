package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hexane/atlas/internal/app"
	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/postgres"
	storageuser "github.com/hexane/atlas/internal/storage/user"
)

// userCommand creates or lists human-user accounts, and grants or revokes
// their node-scoped roles. See docs/adr/0011-deferred-rbac.md and
// internal/core/user: this is the human-user identity domain, entirely
// separate from `atlas-server peer` (libp2p Agent identity) and
// `atlas-server grant` (agent_operation_grants).
func userCommand(configPath string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: atlas-server user <create|list|grant|revoke-grant|grants> [flags]")
	}
	switch args[0] {
	case "create":
		return userCreate(configPath, args[1:])
	case "list":
		return userList(configPath)
	case "grant":
		return userGrant(configPath, args[1:])
	case "revoke-grant":
		return userRevokeGrant(configPath, args[1:])
	case "grants":
		return userGrants(configPath, args[1:])
	default:
		return fmt.Errorf("unknown user command %q; want create, list, grant, revoke-grant or grants", args[0])
	}
}

// withUserRepository opens the same database the server itself uses and
// hands the caller a repository over it, the same pattern [withRepository]
// uses for the fleet/peer/grant commands.
func withUserRepository(configPath string, fn func(context.Context, *storageuser.Repository) error) error {
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

	return fn(ctx, storageuser.NewRepository(pool.DB()))
}

// generatePassword returns a random password for an operator who does not
// supply one explicitly. It is shown exactly once, the same "no reveal API"
// rule [fleet.GeneratedToken] documents for enrollment tokens.
func generatePassword() (string, error) {
	buf := make([]byte, 15) // 20 base64 characters, well over minPasswordLength
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func userCreate(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server user create", flag.ContinueOnError)
	username := flags.String("username", "", "login name for the new user (required)")
	password := flags.String("password", "", "password for the new user; omit to have one generated and printed once")
	email := flags.String("email", "", "optional contact address, never used to log in")
	if err := flags.Parse(args); err != nil {
		return err
	}

	pass := *password
	generated := false
	if pass == "" {
		var err error
		pass, err = generatePassword()
		if err != nil {
			return err
		}
		generated = true
	}

	spec := coreuser.CreateSpec{Username: *username, Password: pass, Email: *email}
	// Validated before a database connection is opened, the same convention
	// [fleet.TokenSpec.Validate] and [fleet.PeerSpec.Validate] follow.
	if err := spec.Validate(); err != nil {
		return err
	}

	hash, err := coreuser.HashPassword(pass)
	if err != nil {
		return err
	}

	return withUserRepository(configPath, func(ctx context.Context, repo *storageuser.Repository) error {
		if err := repo.CreateUser(ctx, coreuser.User{Username: spec.Username, PasswordHash: hash, Email: spec.Email}); err != nil {
			return err
		}
		fmt.Printf("created user %s\n", spec.Username)
		if generated {
			fmt.Fprintf(os.Stderr, "Generated password: %s\nIt will not be shown again.\n", pass)
		}
		return nil
	})
}

func userList(configPath string) error {
	return withUserRepository(configPath, func(ctx context.Context, repo *storageuser.Repository) error {
		users, err := repo.ListUsers(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tUSERNAME\tEMAIL\tDISABLED\tCREATED")
		for _, u := range users {
			email := u.Email
			if email == "" {
				email = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%s\n", u.ID, u.Username, email, u.Disabled(), u.CreatedAt.Format(time.RFC3339))
		}
		return w.Flush()
	})
}

// validateGrantScope enforces that an operator chooses explicitly between a
// specific node and every node — there is no default between them. An empty
// --node-id must never silently become the broadest possible grant; see
// [coreuser.GrantSpec]'s doc.
func validateGrantScope(nodeID string, fleetWide bool) error {
	switch {
	case nodeID == "" && !fleetWide:
		return fmt.Errorf("specify either --node-id or --fleet-wide; there is no default between them")
	case nodeID != "" && fleetWide:
		return fmt.Errorf("--node-id and --fleet-wide cannot both be set")
	}
	return nil
}

func userGrant(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server user grant", flag.ContinueOnError)
	userID := flags.String("user-id", "", "the user id to grant a role to (required; see `user list`)")
	role := flags.String("role", "", "the role to grant: viewer, operator, or admin (required)")
	nodeID := flags.String("node-id", "", "the node this grant is scoped to")
	fleetWide := flags.Bool("fleet-wide", false, "grant the role across every node instead of one")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if err := validateGrantScope(*nodeID, *fleetWide); err != nil {
		return err
	}

	spec := coreuser.GrantSpec{UserID: *userID, NodeID: *nodeID, FleetWide: *fleetWide, Role: *role, GrantedBy: "operator"}
	if err := spec.Validate(); err != nil {
		return err
	}

	return withUserRepository(configPath, func(ctx context.Context, repo *storageuser.Repository) error {
		if err := repo.Grant(ctx, spec, time.Now()); err != nil {
			return err
		}
		if spec.FleetWide {
			fmt.Printf("granted %s to user %s, fleet-wide\n", spec.Role, spec.UserID)
		} else {
			fmt.Printf("granted %s to user %s for node %s\n", spec.Role, spec.UserID, spec.NodeID)
		}
		return nil
	})
}

func userRevokeGrant(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server user revoke-grant", flag.ContinueOnError)
	grantID := flags.String("grant-id", "", "the grant id to revoke (required; see `user grants`)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *grantID == "" {
		return fmt.Errorf("--grant-id is required")
	}

	return withUserRepository(configPath, func(ctx context.Context, repo *storageuser.Repository) error {
		if err := repo.RevokeGrant(ctx, *grantID, "operator", time.Now()); err != nil {
			return err
		}
		fmt.Printf("revoked grant %s\n", *grantID)
		return nil
	})
}

func userGrants(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server user grants", flag.ContinueOnError)
	userID := flags.String("user-id", "", "the user id to list grants for (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *userID == "" {
		return fmt.Errorf("--user-id is required")
	}

	return withUserRepository(configPath, func(ctx context.Context, repo *storageuser.Repository) error {
		grants, err := repo.ListGrants(ctx, *userID)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNODE\tROLE\tGRANTED\tREVOKED")
		for _, g := range grants {
			node := "fleet-wide"
			if g.NodeID != nil {
				node = *g.NodeID
			}
			revoked := "-"
			if g.RevokedAt != nil {
				revoked = g.RevokedAt.Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", g.ID, node, g.Role, g.GrantedAt.Format(time.RFC3339), revoked)
		}
		return w.Flush()
	})
}
