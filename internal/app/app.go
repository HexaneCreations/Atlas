// Package app is Atlas's composition root.
//
// Every dependency in the process is constructed here, in one function, and
// passed explicitly into whatever needs it. There is no service locator, no
// global registry, and no reflection-based injection container.
//
// That is a deliberate choice rather than an omission. Constructor injection
// makes the dependency graph a thing you can read top to bottom in one file;
// it makes a missing dependency a compile error instead of a nil pointer on
// first request; and it needs no framework to be learned by whoever maintains
// this in three years. The cost is that this file grows as Atlas does, which
// is the right thing for it to do — a composition root that stays small is
// usually one that has pushed its wiring somewhere less visible. See
// docs/adr/0004-constructor-injection.md.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/hexane/atlas/internal/api"
	"github.com/hexane/atlas/internal/core/activity"
	corealert "github.com/hexane/atlas/internal/core/alert"
	coreeventstore "github.com/hexane/atlas/internal/core/eventstore"
	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/build"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/hostid"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/platform/lifecycle"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/migrations"
)

// App is a fully wired Atlas instance.
type App struct {
	Config     *config.Config
	Logger     *slog.Logger
	Bus        *eventbus.Bus
	Pool       *postgres.Pool
	Migrator   *postgres.Migrator
	Health     *health.Registry
	HTTPServer *httpx.Server
	Collection *collectionPipeline
	Activity   *activity.Recorder
	Fleet      *fleetPipeline

	supervisor *lifecycle.Supervisor
}

// New builds an Atlas instance from configuration.
//
// Nothing is started and nothing connects to anything; New only constructs.
// Keeping construction free of side effects is what lets a test build the
// whole graph and inspect it, and it keeps the ordering decisions in one
// place — [Supervisor.Register] below — rather than scattered through
// constructors.
func New(cfg *config.Config, logger *slog.Logger) (*App, error) {
	const op = "app.New"

	if cfg == nil {
		return nil, errs.New(errs.CodeInvalidArgument, "configuration is required").WithOp(op)
	}
	if logger == nil {
		logger = log.Discard()
	}

	bus := eventbus.New(eventbus.Options{
		BufferSize: cfg.EventBus.BufferSize,
		Logger:     logger,
	})

	pool := postgres.NewPool(cfg.Database, logger)
	migrator := postgres.NewMigrator(nil, migrations.FS, logger) // pool attached at Start

	// Identity is resolved before anything else that needs it, and a failure
	// here is fatal: observations attributed to no node are unattributable
	// forever, and no later correction can recover which machine produced them.
	identity, err := hostid.Resolve(hostid.Options{
		ConfiguredID: cfg.Node.ID,
		StateFile:    cfg.Node.IDFile,
	})
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not resolve this node's identity").WithOp(op)
	}

	incidentPipe := newIncidentPipeline(logger, pool)
	notificationPipe := newNotificationPipeline(logger, pool)
	// Alert transitions reach both the incident correlator and the
	// notification dispatcher. Neither depends on the other — the same
	// "independent consumers of one hook" shape as onEvent below.
	onTransition := func(ctx context.Context, entry corealert.HistoryEntry) {
		incidentPipe.HandleAlertTransition(ctx, entry)
		notificationPipe.HandleAlertTransition(ctx, entry)
	}
	alertPipe := newAlertPipeline(logger, pool, onTransition)
	// Events reach both engines: the alert engine matches event rules, the
	// incident correlator groups notable ones. Neither depends on the other.
	onEvent := func(ctx context.Context, rec coreeventstore.Record) {
		alertPipe.HandleEvent(ctx, rec)
		incidentPipe.HandleEvent(ctx, rec)
	}
	collection := newCollectionPipeline(cfg, logger, bus, pool, identity, onEvent)
	activityLog := activity.New(activity.Options{Bus: bus, Logger: logger})
	fleetPipe := newFleetPipeline(cfg, logger, pool, collection, onEvent)

	healthReg := health.NewRegistry(logger)
	healthReg.Register(databaseCheck{pool: pool})
	healthReg.Register(collectorsCheck{pipeline: collection})

	healthScore := newHealthEngine(cfg.Collection.DefaultInterval, pool, logger)
	costAnalysis := newCostEngine(pool)
	capacityPlanning := newCapacityEngine(pool)
	goldenSignals := newGoldenSignalsEngine(pool, capacityPlanning)
	sloEngine := newSLOEngine(pool)

	handler := api.New(api.Deps{
		Config:            cfg,
		Health:            healthReg,
		Pool:              pool,
		Bus:               bus,
		Collection:        collection,
		Activity:          activityLog,
		Inventory:         lazyInventoryStore{pool: pool},
		RemoteLogs:        fleetPipe,
		Nodes:             nodeLookup{pipeline: collection},
		EventStore:        lazyEventStore{pool: pool},
		Alerts:            lazyAlertStore{pool: pool},
		Incidents:         lazyIncidentStore{pool: pool},
		HealthScore:       healthScore,
		CostAnalysis:      costAnalysis,
		CapacityPlanning:  capacityPlanning,
		GoldenSignals:     goldenSignals,
		SLO:               sloEngine,
		SLOStore:          lazySLOStore{pool: pool},
		NotificationStore: lazyNotificationStore{pool: pool},
		Users:             lazyUserStore{pool: pool},
		Sessions:          lazyUserStore{pool: pool},
		Authz:             lazyAuthorizer{pool: pool},
		UserAdmin:         lazyUserStore{pool: pool},
		LoginLimiter:      coreuser.NewLoginLimiter(),
		Logger:            logger,
	})
	server := httpx.NewServer(cfg.Server, handler, logger)

	app := &App{
		Config:     cfg,
		Logger:     logger,
		Bus:        bus,
		Pool:       pool,
		Migrator:   migrator,
		Health:     healthReg,
		HTTPServer: server,
		Collection: collection,
		Activity:   activityLog,
		Fleet:      fleetPipe,
		supervisor: lifecycle.New(lifecycle.Options{
			Logger:          logger,
			ShutdownTimeout: cfg.Server.ShutdownTimeout,
		}),
	}

	// Registration order is startup order; shutdown is its exact reverse.
	//
	//  1. The event bus first: it holds no external resources and everything
	//     else may want to publish during its own startup.
	//  2. Postgres next, including migrations, so the schema is current
	//     before anything reads it.
	//  3. The incident correlator, so its hooks are live before the alert
	//     engine or collection can produce something to correlate.
	//  4. The alert rule engine, so its event hook is live before collection
	//     or fleet can produce an event that needs evaluating.
	//  5. The collection pipeline, which needs a live pool and a current
	//     schema, and must be producing before the API offers to serve what it
	//     produces.
	//  6. The HTTP server last, so Atlas only begins accepting requests once
	//     it can actually answer them. On the way down this reverses: the
	//     listener drains first, and everything it was reading outlives it.
	app.supervisor.Register(
		lifecycle.Func{
			ComponentName: "event.bus",
			OnStop:        func(context.Context) error { return bus.Close() },
		},
		pool,
		lifecycle.Func{
			ComponentName: "postgres.migrations",
			OnStart:       app.runMigrations,
		},
		incidentPipe,
		notificationPipe,
		alertPipe,
		collection,
		// The activity recorder starts after the pipeline so it is listening
		// before the collectors it records can report anything, and stops
		// before the bus closes under it.
		activityLog,
		// Needs collection.Repository(), so it starts after the pipeline.
		fleetPipe,
		server,
	)

	return app, nil
}

// runMigrations applies pending migrations if configured to.
//
// It runs as its own lifecycle component rather than inside the pool's Start,
// so that migration is visible in the startup sequence and can be skipped
// without touching connection setup.
func (a *App) runMigrations(ctx context.Context) error {
	a.Migrator = postgres.NewMigrator(a.Pool.DB(), migrations.FS, a.Logger)

	if !a.Config.Database.MigrateOnStart {
		pending, err := a.Migrator.Pending(ctx)
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			// Refusing to start is the safe response: an application running
			// against a schema older than it expects fails in ways that look
			// like data corruption rather than like a missed migration.
			return errs.New(errs.CodeFailedPrecondition,
				"%d migration(s) are pending and migrate_on_start is disabled; run `atlas-server migrate`",
				len(pending)).WithOp("app.App.runMigrations")
		}
		a.Logger.InfoContext(ctx, "schema is current, no migrations to apply")
		return nil
	}

	result, err := a.Migrator.Apply(ctx)
	if err != nil {
		return err
	}
	a.Logger.InfoContext(ctx, "database schema ready",
		slog.Int("applied", len(result.Applied)),
		slog.Int("already_current", result.AlreadyCurrent),
		slog.Duration("duration", result.Duration),
	)
	return nil
}

// RunMigrationsNow applies pending migrations against an already-started
// pool, regardless of the migrate_on_start setting.
//
// This backs `atlas-server migrate`, the deliberate one-shot path for
// deployments that run several replicas and do not want them racing to
// migrate on startup.
func (a *App) RunMigrationsNow(ctx context.Context) error {
	migrator := postgres.NewMigrator(a.Pool.DB(), migrations.FS, a.Logger)

	result, err := migrator.Apply(ctx)
	if err != nil {
		return err
	}
	a.Logger.InfoContext(ctx, "migrations complete",
		slog.Int("applied", len(result.Applied)),
		slog.Int("already_current", result.AlreadyCurrent),
		slog.Duration("duration", result.Duration),
	)
	return nil
}

// Run starts every component and blocks until ctx is cancelled or a component
// fails, then shuts down in reverse order.
func (a *App) Run(ctx context.Context) error {
	a.Logger.InfoContext(ctx, "starting atlas",
		slog.String("version", build.Current().Version),
		slog.String("environment", string(a.Config.Environment)),
		slog.String("addr", a.Config.Server.Addr()),
		slog.Any("database", a.Config.Database),
	)
	return a.supervisor.Run(ctx)
}

// Addr returns the address the HTTP server is bound to. Only meaningful once
// Run has started the server.
func (a *App) Addr() string { return a.HTTPServer.Addr() }

// agentVersion is stamped onto every observation. In a fleet, agents upgrade
// at different times, and a metric whose meaning changed between versions is
// uninterpretable without knowing which produced it.
func agentVersion() string { return build.Current().Version }

// databaseCheck probes Postgres for the health registry.
//
// It is critical: without the database Atlas can neither store observations
// nor answer a query about them, so an instance in that state should not
// receive traffic.
type databaseCheck struct{ pool *postgres.Pool }

func (c databaseCheck) Name() string   { return "database" }
func (c databaseCheck) Critical() bool { return true }

func (c databaseCheck) Check(ctx context.Context) error { return c.pool.Ping(ctx) }

// collectorsCheck reports whether collection is working.
//
// It is deliberately non-critical. A failing collector means reduced
// visibility into one aspect of one host; taking the instance out of rotation
// for that would remove visibility into everything, which is strictly worse.
type collectorsCheck struct{ pipeline *collectionPipeline }

func (c collectorsCheck) Name() string   { return "collectors" }
func (c collectorsCheck) Critical() bool { return false }

func (c collectorsCheck) Check(context.Context) error {
	var failing []string
	for _, h := range c.pipeline.CollectorHealth() {
		// One missed run is normal on a loaded host; three consecutive
		// failures is a collector that is genuinely not working.
		if h.ConsecutiveFailures >= 3 {
			failing = append(failing, h.CollectorID)
		}
	}
	if len(failing) > 0 {
		return errs.New(errs.CodeUnavailable,
			"%d collector(s) are failing: %s", len(failing), strings.Join(failing, ", "))
	}
	return nil
}

// LoadConfigAndLogger resolves configuration and builds the process logger.
//
// It is separate from [New] because it is what a command-line entry point
// needs before it can report anything at all: until configuration is loaded
// there is no logger, and until there is a logger, failures have nowhere to
// go but stderr.
func LoadConfigAndLogger(path string) (*config.Config, *slog.Logger, error) {
	cfg, err := config.Load(config.Options{Path: path})
	if err != nil {
		return nil, nil, err
	}

	logger, err := log.New(cfg.Logging, os.Stdout)
	if err != nil {
		return nil, nil, fmt.Errorf("build logger: %w", err)
	}
	// Anything logging before it receives an explicit logger — third-party
	// code, early initialisation — still lands in the structured stream.
	slog.SetDefault(logger)

	return cfg, logger, nil
}
