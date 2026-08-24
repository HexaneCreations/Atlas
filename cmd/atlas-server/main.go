// Command atlas-server runs the Atlas control plane: the HTTP API and, from
// Phase 1, the collector scheduler.
//
// Usage:
//
//	atlas-server [--config path] [command]
//
// Commands:
//
//	serve      run the server (default)
//	migrate    apply pending database migrations and exit
//	config     print the resolved configuration and exit
//	version    print build information and exit
//
// Configuration is resolved from defaults, then an optional YAML file, then
// ATLAS_ environment variables. See docs/operations/configuration.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hexane/atlas/internal/app"
	"github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/platform/build"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/postgres"
	storagefleet "github.com/hexane/atlas/internal/storage/fleet"
)

func main() {
	if err := run(); err != nil {
		// Configuration and startup failures may occur before a logger
		// exists, so the last-resort channel is stderr. Everything after
		// startup is logged structurally.
		fmt.Fprintf(os.Stderr, "atlas-server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	flags := flag.NewFlagSet("atlas-server", flag.ContinueOnError)
	flags.StringVar(&configPath, "config", "", "path to an Atlas YAML configuration file")
	flags.Usage = usage(flags)

	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	command := "serve"
	if args := flags.Args(); len(args) > 0 {
		command = args[0]
	}

	switch command {
	case "serve":
		return serve(configPath)
	case "migrate":
		return migrate(configPath)
	case "config":
		return printConfig(configPath)
	case "enroll-token":
		return enrollToken(configPath, flags.Args()[1:])
	case "peer":
		return peerCommand(configPath, flags.Args()[1:])
	case "grant":
		return grantCommand(configPath, flags.Args()[1:])
	case "version":
		fmt.Println(build.Current())
		return nil
	default:
		flags.Usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func usage(flags *flag.FlagSet) func() {
	return func() {
		fmt.Fprint(flags.Output(), `atlas-server — Atlas control plane

Usage:
  atlas-server [flags] [command]

Commands:
  serve             run the server (default)
  migrate           apply pending database migrations and exit
  config            print the resolved configuration, with secrets redacted
  enroll-token      create an agent enrollment token (HTTPS/mTLS transport only)
  peer              authorize, list or revoke an agent libp2p Peer ID
  grant             authorize or revoke an AgentOps operation grant for a node
  version           print build information

Flags:
`)
		flags.PrintDefaults()
		fmt.Fprintf(flags.Output(), "\nEnvironment variables are prefixed %s_ and override the configuration file.\n",
			config.DefaultEnvPrefix)
	}
}

// serve runs Atlas until it receives a termination signal.
func serve(configPath string) error {
	cfg, logger, err := app.LoadConfigAndLogger(configPath)
	if err != nil {
		return err
	}

	instance, err := app.New(cfg, logger)
	if err != nil {
		return err
	}

	// SIGINT and SIGTERM cancel the context, which the supervisor turns into
	// an ordered shutdown. NotifyContext restores the default handler after
	// the first signal, so a second Ctrl-C during a slow drain kills the
	// process immediately — the behaviour an operator expects when they have
	// decided not to wait.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return instance.Run(ctx)
}

// migrate applies pending migrations and exits.
//
// It exists so operators running several replicas can apply schema changes as
// a deliberate, separate step — a job that runs once — rather than racing
// instances on startup.
func migrate(configPath string) error {
	cfg, logger, err := app.LoadConfigAndLogger(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	instance, err := app.New(cfg, logger)
	if err != nil {
		return err
	}

	if err := instance.Pool.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = instance.Pool.Stop(context.WithoutCancel(ctx)) }()

	if err := instance.RunMigrationsNow(ctx); err != nil {
		return err
	}
	return nil
}

// printConfig writes the resolved configuration to stdout.
//
// Operators use it to confirm which layer supplied a value when a deployment
// behaves unexpectedly. The database password is never part of the output:
// the field is tagged to be omitted, so this cannot become a way to read a
// secret out of a running container.
func printConfig(configPath string) error {
	cfg, err := config.Load(config.Options{Path: configPath})
	if err != nil {
		return err
	}

	out := cfg.Render()
	out["database_dsn"] = cfg.Database.SafeDSN()
	out["recognised_env_vars"] = config.EnvVars()

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// enrollToken creates a bounded enrollment token an operator hands to
// provisioning. See docs/architecture/agent-design.md §4.
func enrollToken(configPath string, args []string) error {
	flags := flag.NewFlagSet("atlas-server enroll-token", flag.ContinueOnError)
	environment := flags.String("environment", "", "environment tag for nodes enrolled with this token (required)")
	cidr := flags.String("cidr", "0.0.0.0/0", "network allowed to redeem this token")
	maxUses := flags.Int("max-uses", 1, "number of times this token may be redeemed")
	ttl := flags.Duration("ttl", time.Hour, "how long this token remains valid")
	label := flags.String("label", "", "operator-facing label for this token")
	allowReenroll := flags.Bool("allow-reenroll", false, "permit re-enrolling a node id that already holds a live certificate")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *environment == "" {
		return fmt.Errorf("--environment is required")
	}

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

	tok, err := fleet.NewToken()
	if err != nil {
		return err
	}

	repo := storagefleet.NewRepository(pool.DB())
	spec := fleet.TokenSpec{
		Label: *label, Environment: *environment, AllowedCIDR: *cidr,
		MaxUses: *maxUses, TTL: *ttl, AllowReenroll: *allowReenroll,
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := repo.CreateToken(ctx, tok.Hash, spec, time.Now()); err != nil {
		return err
	}

	fmt.Println(tok.Plaintext)
	fmt.Fprintf(os.Stderr, "Token created. It will not be shown again. Expires in %s, %d use(s), environment %q.\n",
		*ttl, *maxUses, *environment)
	return nil
}
