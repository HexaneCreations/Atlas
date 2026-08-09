// Package postgres provides Atlas's PostgreSQL/TimescaleDB access layer: a
// supervised connection pool, a forward-only migration runner, and health
// probes.
//
// It exposes pgx types rather than database/sql. Atlas depends on Postgres
// specifics — TimescaleDB hypertables, COPY for bulk metric ingest, LISTEN
// and NOTIFY, array and JSONB parameters — and database/sql's portable
// interface would hide exactly the features the platform is built on, while
// costing a layer of reflection on every query.
package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/lifecycle"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is a supervised PostgreSQL connection pool.
//
// It satisfies [lifecycle.Component]: Start opens the pool and verifies
// connectivity, Stop drains it.
type Pool struct {
	cfg    config.Database
	logger *slog.Logger
	pool   *pgxpool.Pool
}

// NewPool builds a pool. It connects nothing until Start.
func NewPool(cfg config.Database, logger *slog.Logger) *Pool {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Pool{cfg: cfg, logger: logger}
}

// Name implements [lifecycle.Component].
func (p *Pool) Name() string { return "postgres.pool" }

// Start opens the pool and verifies the database is reachable.
//
// Connectivity is checked synchronously and a failure aborts startup. Postgres
// is a hard dependency: it holds the configuration, the service catalog, and
// every historical metric. An Atlas that started without it would accept
// requests it cannot answer and silently discard the samples it collects —
// worse than not starting, because monitoring would appear to be running.
func (p *Pool) Start(ctx context.Context) error {
	poolCfg, err := pgxpool.ParseConfig(p.cfg.DSN())
	if err != nil {
		// The DSN contains the password, so the parse error is wrapped as
		// internal and never reaches a client.
		return errs.Wrap(err, errs.CodeInternal, "invalid database configuration").
			WithOp("postgres.Pool.Start")
	}

	poolCfg.MaxConns = p.cfg.MaxConns
	poolCfg.MinConns = p.cfg.MinConns
	poolCfg.MaxConnLifetime = p.cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = p.cfg.MaxConnIdleTime
	// Jitter prevents a pool opened all at once from expiring all at once,
	// which would otherwise produce a periodic reconnect stampede.
	poolCfg.MaxConnLifetimeJitter = p.cfg.MaxConnLifetime / 10
	poolCfg.HealthCheckPeriod = 30 * time.Second
	poolCfg.ConnConfig.Tracer = &queryTracer{logger: p.logger, slowQuery: slowQueryThreshold}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not create the database pool").
			WithOp("postgres.Pool.Start")
	}

	pingCtx, cancel := context.WithTimeout(ctx, p.cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return errs.Wrap(err, errs.CodeUnavailable, "database is unreachable").
			WithOp("postgres.Pool.Start").
			WithDetail("target", p.cfg.SafeDSN())
	}

	p.pool = pool
	p.logger.InfoContext(ctx, "database pool ready",
		slog.String("target", p.cfg.SafeDSN()),
		slog.Int("max_conns", int(p.cfg.MaxConns)),
	)
	return nil
}

// Stop closes the pool, waiting for checked-out connections to be returned.
//
// pgxpool.Close blocks until every connection is released and has no context
// parameter, so it is run in a goroutine and bounded by the supervisor's
// deadline. Without that, one leaked connection would hang shutdown
// indefinitely.
func (p *Pool) Stop(ctx context.Context) error {
	if p.pool == nil {
		return nil
	}
	closed := make(chan struct{})
	go func() {
		p.pool.Close()
		close(closed)
	}()

	select {
	case <-closed:
		return nil
	case <-ctx.Done():
		return errs.Wrap(ctx.Err(), errs.CodeDeadlineExceeded,
			"database pool did not drain before the shutdown deadline").
			WithOp("postgres.Pool.Stop")
	}
}

// DB returns the underlying pgx pool for query execution.
//
// It returns nil before Start. Repositories receive the pool at construction
// time, after startup, so they never observe that case.
func (p *Pool) DB() *pgxpool.Pool { return p.pool }

// Ping verifies the database answers. Used by the readiness probe.
func (p *Pool) Ping(ctx context.Context) error {
	if p.pool == nil {
		return errs.New(errs.CodeUnavailable, "database pool is not started").
			WithOp("postgres.Pool.Ping")
	}
	if err := p.pool.Ping(ctx); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "database is unreachable").
			WithOp("postgres.Pool.Ping")
	}
	return nil
}

// Stats reports pool utilisation.
//
// Atlas exposes this on its own health endpoint: a monitoring platform that
// cannot report on its own saturation is one blind spot away from being
// unable to explain its own latency.
type Stats struct {
	// AcquiredConns are connections currently checked out.
	AcquiredConns int32 `json:"acquired_conns"`
	// IdleConns are open connections available for use.
	IdleConns int32 `json:"idle_conns"`
	// TotalConns is the current pool size.
	TotalConns int32 `json:"total_conns"`
	// MaxConns is the configured ceiling.
	MaxConns int32 `json:"max_conns"`
	// EmptyAcquireCount counts acquisitions that had to wait for a free
	// connection. A rising value means the pool is undersized for the load.
	EmptyAcquireCount int64 `json:"empty_acquire_count"`
	// CanceledAcquireCount counts acquisitions abandoned before a connection
	// became available.
	CanceledAcquireCount int64 `json:"canceled_acquire_count"`
}

// Stats returns a snapshot of pool utilisation.
func (p *Pool) Stats() Stats {
	if p.pool == nil {
		return Stats{MaxConns: p.cfg.MaxConns}
	}
	s := p.pool.Stat()
	return Stats{
		AcquiredConns:        s.AcquiredConns(),
		IdleConns:            s.IdleConns(),
		TotalConns:           s.TotalConns(),
		MaxConns:             s.MaxConns(),
		EmptyAcquireCount:    s.EmptyAcquireCount(),
		CanceledAcquireCount: s.CanceledAcquireCount(),
	}
}

// Version returns the PostgreSQL server version string, for diagnostics.
func (p *Pool) Version(ctx context.Context) (string, error) {
	if p.pool == nil {
		return "", errs.New(errs.CodeUnavailable, "database pool is not started").
			WithOp("postgres.Pool.Version")
	}
	var version string
	if err := p.pool.QueryRow(ctx, "SELECT version()").Scan(&version); err != nil {
		return "", errs.Wrap(err, errs.CodeUnavailable, "could not read the server version").
			WithOp("postgres.Pool.Version")
	}
	return version, nil
}

// HasExtension reports whether an extension is installed in the current
// database.
func (p *Pool) HasExtension(ctx context.Context, name string) (bool, error) {
	if p.pool == nil {
		return false, errs.New(errs.CodeUnavailable, "database pool is not started").
			WithOp("postgres.Pool.HasExtension")
	}
	var present bool
	const q = `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)`
	if err := p.pool.QueryRow(ctx, q, name).Scan(&present); err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not query installed extensions").
			WithOp("postgres.Pool.HasExtension").
			WithDetail("extension", name)
	}
	return present, nil
}

var _ lifecycle.Component = (*Pool)(nil)

// String renders the pool target without credentials.
func (p *Pool) String() string { return fmt.Sprintf("postgres(%s)", p.cfg.SafeDSN()) }
