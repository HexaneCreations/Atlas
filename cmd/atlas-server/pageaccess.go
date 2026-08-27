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
	corepageauthz "github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/platform/postgres"
	storagepageauthz "github.com/hexane/atlas/internal/storage/pageauthz"
)

// pageAccessCommand manages RoleAccess bundle *definitions* — creating a
// named, reusable set of pages and choosing its membership. This is
// deliberately CLI-only: bundle definitions are an infrequent, structural
// decision (deciding what "container-related" even means), unlike
// *assigning* an existing bundle to a user or granting a page directly,
// which are routine admin actions exposed through the HTTP API instead
// (POST /users/{id}/role-access, POST /users/{id}/page-access) — the same
// "CLI for bootstrap, UI for routine work" split
// docs/adr/0011-deferred-rbac.md's own CLI already draws for `user grant`.
func pageAccessCommand(configPath string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: atlas-server page-access <create-bundle|add-page|remove-page|list-bundles|grant|revoke|grants> [flags]")
	}
	switch args[0] {
	case "create-bundle":
		return pageAccessCreateBundle(configPath, args[1:])
	case "add-page":
		return pageAccessAddPage(configPath, args[1:])
	case "remove-page":
		return pageAccessRemovePage(configPath, args[1:])
	case "list-bundles":
		return pageAccessListBundles(configPath)
	// grant/revoke/grants manage direct UserAccess page grants — the exact
	// bootstrap escape hatch migrations/0016_page_access_bootstrap.sql's own
	// doc names: every /users/*/page-access HTTP endpoint requires
	// PermissionUserManage AND PageUsers (see
	// internal/api/v1/pageaccess.go), so without these, an operator with no
	// existing PageUsers grant — the state every environment starts in —
	// would have no way to grant it to anyone at all, through the API or
	// otherwise. These call the exact same core.Store methods the HTTP
	// handlers do (same conflict check, same audit write), never a second
	// implementation.
	case "grant":
		return pageAccessGrant(configPath, args[1:])
	case "revoke":
		return pageAccessRevoke(configPath, args[1:])
	case "grants":
		return pageAccessGrants(configPath, args[1:])
	default:
		return fmt.Errorf("unknown page-access command %q; want create-bundle, add-page, remove-page, list-bundles, grant, revoke or grants", args[0])
	}
}

// roleAccessCommand manages RoleAccess *assignments* — a user holding an
// existing bundle, scoped to a node or fleet-wide. Distinct from
// page-access's create-bundle/add-page/remove-page, which manage the
// bundle *definitions* themselves.
func roleAccessCommand(configPath string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: atlas-server role-access <assign|unassign|assignments> [flags]")
	}
	switch args[0] {
	case "assign":
		return roleAccessAssign(configPath, args[1:])
	case "unassign":
		return roleAccessUnassign(configPath, args[1:])
	case "assignments":
		return roleAccessAssignments(configPath, args[1:])
	default:
		return fmt.Errorf("unknown role-access command %q; want assign, unassign or assignments", args[0])
	}
}

// withPageAccessRepository opens the same database the server itself uses
// and hands the caller a repository over it, the same pattern
// [withUserRepository] uses.
func withPageAccessRepository(configPath string, fn func(context.Context, *storagepageauthz.Repository) error) error {
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

	return fn(ctx, storagepageauthz.NewRepository(pool.DB()))
}

func pageAccessCreateBundle(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server page-access create-bundle", flag.ContinueOnError)
	name := flags.String("name", "", "the bundle's name (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--name is required")
	}
	return withPageAccessRepository(configPath, func(ctx context.Context, repo *storagepageauthz.Repository) error {
		if err := repo.CreateRoleAccess(ctx, *name, "operator", time.Now()); err != nil {
			return err
		}
		fmt.Printf("created role-access bundle %s\n", *name)
		return nil
	})
}

func pageAccessAddPage(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server page-access add-page", flag.ContinueOnError)
	bundle := flags.String("bundle", "", "the bundle's name (required; see page-access list-bundles)")
	page := flags.String("page", "", "the page to add, e.g. containers (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *bundle == "" || *page == "" {
		return fmt.Errorf("--bundle and --page are required")
	}
	return withPageAccessRepository(configPath, func(ctx context.Context, repo *storagepageauthz.Repository) error {
		if err := repo.AddPageToRoleAccess(ctx, *bundle, corepageauthz.Page(*page)); err != nil {
			return err
		}
		fmt.Printf("added %s to bundle %s\n", *page, *bundle)
		return nil
	})
}

func pageAccessRemovePage(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server page-access remove-page", flag.ContinueOnError)
	bundle := flags.String("bundle", "", "the bundle's name (required)")
	page := flags.String("page", "", "the page to remove (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *bundle == "" || *page == "" {
		return fmt.Errorf("--bundle and --page are required")
	}
	return withPageAccessRepository(configPath, func(ctx context.Context, repo *storagepageauthz.Repository) error {
		if err := repo.RemovePageFromRoleAccess(ctx, *bundle, corepageauthz.Page(*page)); err != nil {
			return err
		}
		fmt.Printf("removed %s from bundle %s\n", *page, *bundle)
		return nil
	})
}

func pageAccessGrant(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server page-access grant", flag.ContinueOnError)
	userID := flags.String("user-id", "", "the user id to grant page access to (required; see `user list`)")
	page := flags.String("page", "", "the page to grant, e.g. containers or users (required)")
	nodeID := flags.String("node-id", "", "the node this grant is scoped to")
	fleetWide := flags.Bool("fleet-wide", false, "grant the page across every node instead of one")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *userID == "" || *page == "" {
		return fmt.Errorf("--user-id and --page are required")
	}
	if err := validateGrantScope(*nodeID, *fleetWide); err != nil {
		return err
	}

	spec := corepageauthz.PageGrantSpec{
		UserID: *userID, Page: corepageauthz.Page(*page),
		NodeID: *nodeID, FleetWide: *fleetWide, GrantedBy: "operator",
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	return withPageAccessRepository(configPath, func(ctx context.Context, repo *storagepageauthz.Repository) error {
		if err := repo.GrantPageAccess(ctx, spec, time.Now()); err != nil {
			return err
		}
		if spec.FleetWide {
			fmt.Printf("granted %s to user %s, fleet-wide\n", spec.Page, spec.UserID)
		} else {
			fmt.Printf("granted %s to user %s for node %s\n", spec.Page, spec.UserID, spec.NodeID)
		}
		return nil
	})
}

func pageAccessRevoke(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server page-access revoke", flag.ContinueOnError)
	grantID := flags.String("grant-id", "", "the grant id to revoke (required; see `page-access grants`)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *grantID == "" {
		return fmt.Errorf("--grant-id is required")
	}
	return withPageAccessRepository(configPath, func(ctx context.Context, repo *storagepageauthz.Repository) error {
		if err := repo.RevokePageAccess(ctx, *grantID, "operator", time.Now()); err != nil {
			return err
		}
		fmt.Printf("revoked page grant %s\n", *grantID)
		return nil
	})
}

func pageAccessGrants(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server page-access grants", flag.ContinueOnError)
	userID := flags.String("user-id", "", "the user id to list page grants for (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *userID == "" {
		return fmt.Errorf("--user-id is required")
	}
	return withPageAccessRepository(configPath, func(ctx context.Context, repo *storagepageauthz.Repository) error {
		grants, err := repo.ListPageAccessGrants(ctx, *userID)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tPAGE\tNODE\tGRANTED\tREVOKED")
		for _, g := range grants {
			node := "fleet-wide"
			if g.NodeID != nil {
				node = *g.NodeID
			}
			revoked := "-"
			if g.RevokedAt != nil {
				revoked = g.RevokedAt.Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", g.ID, g.Page, node, g.GrantedAt.Format(time.RFC3339), revoked)
		}
		return w.Flush()
	})
}

func roleAccessAssign(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server role-access assign", flag.ContinueOnError)
	userID := flags.String("user-id", "", "the user id to assign the bundle to (required)")
	bundle := flags.String("bundle", "", "the bundle's name (required; see `page-access list-bundles`)")
	nodeID := flags.String("node-id", "", "the node this assignment is scoped to")
	fleetWide := flags.Bool("fleet-wide", false, "assign the bundle across every node instead of one")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *userID == "" || *bundle == "" {
		return fmt.Errorf("--user-id and --bundle are required")
	}
	if err := validateGrantScope(*nodeID, *fleetWide); err != nil {
		return err
	}

	spec := corepageauthz.RoleAccessAssignmentSpec{
		UserID: *userID, RoleAccessName: *bundle,
		NodeID: *nodeID, FleetWide: *fleetWide, GrantedBy: "operator",
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	return withPageAccessRepository(configPath, func(ctx context.Context, repo *storagepageauthz.Repository) error {
		if err := repo.AssignRoleAccess(ctx, spec, time.Now()); err != nil {
			return err
		}
		if spec.FleetWide {
			fmt.Printf("assigned %s to user %s, fleet-wide\n", spec.RoleAccessName, spec.UserID)
		} else {
			fmt.Printf("assigned %s to user %s for node %s\n", spec.RoleAccessName, spec.UserID, spec.NodeID)
		}
		return nil
	})
}

func roleAccessUnassign(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server role-access unassign", flag.ContinueOnError)
	assignmentID := flags.String("assignment-id", "", "the assignment id to revoke (required; see `role-access assignments`)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *assignmentID == "" {
		return fmt.Errorf("--assignment-id is required")
	}
	return withPageAccessRepository(configPath, func(ctx context.Context, repo *storagepageauthz.Repository) error {
		if err := repo.RevokeRoleAccessAssignment(ctx, *assignmentID, "operator", time.Now()); err != nil {
			return err
		}
		fmt.Printf("unassigned role-access assignment %s\n", *assignmentID)
		return nil
	})
}

func roleAccessAssignments(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server role-access assignments", flag.ContinueOnError)
	userID := flags.String("user-id", "", "the user id to list bundle assignments for (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *userID == "" {
		return fmt.Errorf("--user-id is required")
	}
	return withPageAccessRepository(configPath, func(ctx context.Context, repo *storagepageauthz.Repository) error {
		assignments, err := repo.ListRoleAccessAssignments(ctx, *userID)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tBUNDLE\tNODE\tGRANTED\tREVOKED")
		for _, a := range assignments {
			node := "fleet-wide"
			if a.NodeID != nil {
				node = *a.NodeID
			}
			revoked := "-"
			if a.RevokedAt != nil {
				revoked = a.RevokedAt.Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.RoleAccessName, node, a.GrantedAt.Format(time.RFC3339), revoked)
		}
		return w.Flush()
	})
}

func pageAccessListBundles(configPath string) error {
	return withPageAccessRepository(configPath, func(ctx context.Context, repo *storagepageauthz.Repository) error {
		defs, err := repo.ListRoleAccessDefinitions(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tPAGES\tCREATED")
		for _, d := range defs {
			pages := ""
			for i, p := range d.Pages {
				if i > 0 {
					pages += ","
				}
				pages += string(p)
			}
			if pages == "" {
				pages = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", d.Name, pages, d.CreatedAt.Format(time.RFC3339))
		}
		return w.Flush()
	})
}
