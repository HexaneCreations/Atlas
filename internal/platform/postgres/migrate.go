package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockKey serialises migration runs across processes. The value is
// arbitrary but must never change: two Atlas versions using different keys
// could migrate concurrently, which is the exact failure the lock prevents.
const advisoryLockKey int64 = 0x41544C4153 // "ATLAS" in ASCII

// migrationFilePattern matches NNNN_description.sql. The numeric prefix is
// the version and defines apply order.
var migrationFilePattern = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// Migration is a single schema change read from the embedded filesystem.
type Migration struct {
	// Version orders migrations. Unique and gap-tolerant.
	Version int64
	// Name is the descriptive part of the filename.
	Name string
	// SQL is the statement text.
	SQL string
	// Checksum is the SHA-256 of SQL, used to detect edits to migrations
	// that have already been applied somewhere.
	Checksum string
}

// Filename reconstructs the source filename.
func (m Migration) Filename() string { return fmt.Sprintf("%04d_%s.sql", m.Version, m.Name) }

// Migrator applies schema migrations.
//
// Migrations are forward-only: there are no down files. Down migrations are
// written once, almost never executed, and therefore almost never correct —
// and a rollback that drops a column destroys data that the forward path
// cannot recreate. Atlas rolls back by deploying the previous application
// version against the newer schema, which requires every migration to be
// backward-compatible with the release before it. That constraint is real
// work, but it is work that gets tested on every deploy, unlike a down file.
// See docs/adr/0007-forward-only-migrations.md.
type Migrator struct {
	pool   *pgxpool.Pool
	fsys   fs.FS
	logger *slog.Logger
}

// NewMigrator builds a migrator reading migrations from fsys.
func NewMigrator(pool *pgxpool.Pool, fsys fs.FS, logger *slog.Logger) *Migrator {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Migrator{pool: pool, fsys: fsys, logger: logger}
}

// Load reads and parses every migration, sorted by version.
//
// It fails on a malformed filename or a duplicate version rather than
// skipping the file: a migration silently ignored because of a typo is a
// schema drift that surfaces much later, somewhere much worse.
func (m *Migrator) Load() ([]Migration, error) {
	const op = "postgres.Migrator.Load"

	entries, err := fs.ReadDir(m.fsys, ".")
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not read the migrations directory").WithOp(op)
	}

	var migrations []Migration
	seen := make(map[int64]string, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		match := migrationFilePattern.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, errs.New(errs.CodeInternal,
				"migration %q does not match NNNN_lower_snake_case.sql", entry.Name()).WithOp(op)
		}

		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal,
				"migration %q has an unparseable version", entry.Name()).WithOp(op)
		}
		if prev, dup := seen[version]; dup {
			return nil, errs.New(errs.CodeInternal,
				"migrations %q and %q share version %d", prev, entry.Name(), version).WithOp(op)
		}
		seen[version] = entry.Name()

		body, err := fs.ReadFile(m.fsys, path.Join(".", entry.Name()))
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal,
				"could not read migration %q", entry.Name()).WithOp(op)
		}
		if len(strings.TrimSpace(string(body))) == 0 {
			return nil, errs.New(errs.CodeInternal, "migration %q is empty", entry.Name()).WithOp(op)
		}

		sum := sha256.Sum256(body)
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     match[2],
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	slices.SortFunc(migrations, func(a, b Migration) int { return int(a.Version - b.Version) })
	return migrations, nil
}

// AppliedMigration records a migration that has run against this database.
type AppliedMigration struct {
	Version     int64     `json:"version"`
	Name        string    `json:"name"`
	Checksum    string    `json:"checksum"`
	AppliedAt   time.Time `json:"applied_at"`
	ExecutionMS int64     `json:"execution_ms"`
}

// Result summarises an [Migrator.Apply] run.
type Result struct {
	// Applied lists migrations run during this call, in order.
	Applied []Migration `json:"applied"`
	// AlreadyCurrent is the count that were already present.
	AlreadyCurrent int `json:"already_current"`
	// Duration is the wall time of the whole run, including lock wait.
	Duration time.Duration `json:"duration"`
}

// bootstrapSQL creates the ledger. It is written to be safe to run on every
// startup and is deliberately not itself a migration — the migrator cannot
// record migrations before its own table exists.
const bootstrapSQL = `
CREATE TABLE IF NOT EXISTS atlas_schema_migrations (
	version      bigint      PRIMARY KEY,
	name         text        NOT NULL,
	checksum     text        NOT NULL,
	applied_at   timestamptz NOT NULL DEFAULT now(),
	execution_ms bigint      NOT NULL
)`

// Apply runs every migration not yet recorded, in version order.
//
// The whole run is serialised by a session-level advisory lock, so several
// Atlas instances starting at once — a rolling deploy, a scaled replica set —
// cannot apply the same migration twice. Instances that lose the race block
// until the winner finishes, then find nothing to do.
//
// Each migration runs inside its own transaction together with the insert
// that records it, so a migration is either fully applied and recorded or
// neither. A failure stops the run; earlier migrations stay applied, which is
// what makes a retry after a fix pick up where it left off.
func (m *Migrator) Apply(ctx context.Context) (Result, error) {
	const op = "postgres.Migrator.Apply"
	start := time.Now()

	migrations, err := m.Load()
	if err != nil {
		return Result{}, err
	}

	// The advisory lock is session-scoped, so it must be taken and released
	// on one dedicated connection rather than borrowed per statement.
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return Result{}, errs.Wrap(err, errs.CodeUnavailable,
			"could not acquire a connection for migration").WithOp(op)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return Result{}, errs.Wrap(err, errs.CodeUnavailable,
			"could not acquire the migration lock").WithOp(op)
	}
	defer func() {
		// Release on a context that is still live: if ctx was cancelled, the
		// unlock would fail and the lock would linger until the connection
		// is reaped, blocking every other instance in the meantime.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", advisoryLockKey); err != nil {
			m.logger.ErrorContext(ctx, "could not release the migration lock", slog.Any("error", err))
		}
	}()

	if _, err := conn.Exec(ctx, bootstrapSQL); err != nil {
		return Result{}, errs.Wrap(err, errs.CodeInternal,
			"could not create the migration ledger").WithOp(op)
	}

	applied, err := m.appliedOn(ctx, conn)
	if err != nil {
		return Result{}, err
	}
	if err := verifyChecksums(migrations, applied); err != nil {
		return Result{}, err
	}

	result := Result{}
	for _, mig := range migrations {
		if _, done := applied[mig.Version]; done {
			result.AlreadyCurrent++
			continue
		}

		m.logger.InfoContext(ctx, "applying migration",
			slog.Int64("version", mig.Version), slog.String("name", mig.Name))

		migStart := time.Now()
		if err := m.applyOne(ctx, conn, mig, migStart); err != nil {
			return result, err
		}

		m.logger.InfoContext(ctx, "migration applied",
			slog.Int64("version", mig.Version),
			slog.Duration("duration", time.Since(migStart)))
		result.Applied = append(result.Applied, mig)
	}

	result.Duration = time.Since(start)
	return result, nil
}

// applyOne runs a migration and records it in one transaction.
func (m *Migrator) applyOne(ctx context.Context, conn interface {
	Begin(context.Context) (pgx.Tx, error)
}, mig Migration, start time.Time,
) error {
	const op = "postgres.Migrator.applyOne"

	tx, err := conn.Begin(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not begin the migration transaction").
			WithOp(op).WithDetail("version", mig.Version)
	}
	// Rollback on any path that does not reach Commit. After a successful
	// commit this is a no-op that pgx reports as ErrTxClosed.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, mig.SQL); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "migration %s failed", mig.Filename()).
			WithOp(op).WithDetail("version", mig.Version).WithDetail("migration", mig.Filename())
	}

	const record = `
		INSERT INTO atlas_schema_migrations (version, name, checksum, execution_ms)
		VALUES ($1, $2, $3, $4)`
	if _, err := tx.Exec(ctx, record, mig.Version, mig.Name, mig.Checksum, time.Since(start).Milliseconds()); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not record migration %s", mig.Filename()).
			WithOp(op).WithDetail("version", mig.Version)
	}

	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not commit migration %s", mig.Filename()).
			WithOp(op).WithDetail("version", mig.Version)
	}
	return nil
}

// undefinedTable is PostgreSQL's SQLSTATE for a missing relation.
const undefinedTable = "42P01"

// isMissingLedger reports whether err means the migration ledger does not
// exist yet.
//
// A database that has never been migrated has no ledger, which is a normal
// starting state rather than a fault — and it is exactly the state in which
// an operator most needs `Pending` to answer, so it cannot be an error.
func isMissingLedger(err error) bool {
	var pgErr *pgconn.PgError
	return errs.As(err, &pgErr) && pgErr.Code == undefinedTable
}

// Status returns every applied migration, oldest first.
//
// An un-migrated database returns an empty slice rather than an error.
func (m *Migrator) Status(ctx context.Context) ([]AppliedMigration, error) {
	const op = "postgres.Migrator.Status"

	const q = `
		SELECT version, name, checksum, applied_at, execution_ms
		FROM atlas_schema_migrations
		ORDER BY version`
	rows, err := m.pool.Query(ctx, q)
	if err != nil {
		if isMissingLedger(err) {
			return nil, nil
		}
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read migration status").WithOp(op)
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.AppliedAt, &a.ExecutionMS); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not scan migration status").WithOp(op)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read migration status").WithOp(op)
	}
	return out, nil
}

// Pending returns migrations present on disk but not yet applied.
func (m *Migrator) Pending(ctx context.Context) ([]Migration, error) {
	migrations, err := m.Load()
	if err != nil {
		return nil, err
	}
	applied, err := m.Status(ctx)
	if err != nil {
		return nil, err
	}
	done := make(map[int64]struct{}, len(applied))
	for _, a := range applied {
		done[a.Version] = struct{}{}
	}

	var pending []Migration
	for _, mig := range migrations {
		if _, ok := done[mig.Version]; !ok {
			pending = append(pending, mig)
		}
	}
	return pending, nil
}

func (m *Migrator) appliedOn(ctx context.Context, conn interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
},
) (map[int64]AppliedMigration, error) {
	const op = "postgres.Migrator.appliedOn"

	rows, err := conn.Query(ctx, `SELECT version, name, checksum, applied_at, execution_ms FROM atlas_schema_migrations`)
	if err != nil {
		if isMissingLedger(err) {
			return map[int64]AppliedMigration{}, nil
		}
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read applied migrations").WithOp(op)
	}
	defer rows.Close()

	out := make(map[int64]AppliedMigration)
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.AppliedAt, &a.ExecutionMS); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not scan applied migrations").WithOp(op)
		}
		out[a.Version] = a
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read applied migrations").WithOp(op)
	}
	return out, nil
}

// verifyChecksums refuses to proceed if an already-applied migration's file
// has changed.
//
// Editing an applied migration is one of the most damaging things a team can
// do to a shared schema: the author's database matches the new file, everyone
// else's matches the old one, and nothing reports a difference until a query
// fails in production. Comparing checksums turns that into a startup error on
// the first machine that notices.
func verifyChecksums(onDisk []Migration, applied map[int64]AppliedMigration) error {
	const op = "postgres.Migrator.verifyChecksums"

	for _, mig := range onDisk {
		prev, ok := applied[mig.Version]
		if !ok || prev.Checksum == mig.Checksum {
			continue
		}
		return errs.New(errs.CodeFailedPrecondition,
			"migration %s has changed since it was applied on %s; "+
				"applied migrations are immutable — add a new migration instead",
			mig.Filename(), prev.AppliedAt.Format(time.RFC3339)).
			WithOp(op).
			WithDetail("version", mig.Version).
			WithDetail("migration", mig.Filename())
	}
	return nil
}
