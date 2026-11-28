// Package monitor stores website monitors, their live probe state, heartbeat
// history and up/down events (migrations/0025_website_monitoring.sql).
//
// Every query here is plain SQL. What depends on TimescaleDB lives in the
// migration only: the monitor_heartbeats hypertable, its compression and
// retention policies, and the monitor_heartbeats_1h continuous aggregate,
// which [Repository.HourlyHistory] reads like any view.
package monitor

import (
	"context"
	"errors"
	"time"

	coremonitor "github.com/hexane/atlas/internal/core/monitor"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/hexane/atlas/internal/platform/secretbox"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository reads and writes the monitor tables. Secrets are sealed with
// key, bound to the monitor id; a nil key refuses to store a secret rather
// than keeping plaintext.
type Repository struct {
	pool *pgxpool.Pool
	key  *secretbox.Key
}

// NewRepository builds a repository over a started pool.
func NewRepository(pool *pgxpool.Pool, key *secretbox.Key) *Repository {
	return &Repository{pool: pool, key: key}
}

// errNoKey means a secret was sent but no encryption key is configured.
var errNoKey = errs.New(errs.CodeUnavailable,
	"monitor credentials, headers and bodies cannot be stored: no encryption key is configured (ATLAS_NOTIFICATIONS_ENCRYPTION_KEY)")

func aad(monitorID string) []byte { return []byte("monitor:" + monitorID) }

func (r *Repository) seal(op, monitorID string, s coremonitor.Secret) (*secretbox.Sealed, error) {
	if s.Empty() {
		return nil, nil
	}
	if r.key == nil {
		return nil, errNoKey.WithOp(op)
	}
	plain, err := coremonitor.EncodeSecret(s)
	if err != nil {
		return nil, errs.New(errs.CodeInternal, "could not encode the monitor secret").WithOp(op)
	}
	sealed, err := r.key.Seal(plain, aad(monitorID))
	if err != nil {
		return nil, errs.New(errs.CodeInternal, "could not encrypt the monitor secret").WithOp(op)
	}
	return &sealed, nil
}

// open decrypts a stored secret; ok is false when it cannot be read with
// the configured key. No error or result carries secret material.
func (r *Repository) open(monitorID string, ciphertext []byte, keyID *string) (coremonitor.Secret, bool) {
	if ciphertext == nil || keyID == nil {
		return coremonitor.Secret{}, true
	}
	if r.key == nil {
		return coremonitor.Secret{}, false
	}
	plain, err := r.key.Open(secretbox.Sealed{KeyID: *keyID, Ciphertext: ciphertext}, aad(monitorID))
	if err != nil {
		return coremonitor.Secret{}, false
	}
	s, err := coremonitor.DecodeSecret(plain)
	if err != nil {
		return coremonitor.Secret{}, false
	}
	return s, true
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func pgConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

const monitorColumns = `m.id, m.name, m.url, m.method, m.interval_seconds, m.retries, m.retry_interval_seconds,
	m.timeout_seconds, m.max_redirects, m.accepted_status, m.body_encoding, COALESCE(m.keyword, ''),
	m.recovery_successes, m.cert_notify, m.cert_warn_days, m.cert_crit_days, COALESCE(m.group_id, ''),
	m.description, m.tags, m.active, m.alerts_muted_until, m.auth_type, m.header_names,
	m.secret_ciphertext IS NOT NULL, m.created_at, m.created_by, m.updated_at, m.updated_by`

func monitorDest(m *coremonitor.Monitor, method, bodyEncoding, authType *string) []any {
	return []any{&m.ID, &m.Name, &m.URL, method, &m.IntervalSeconds, &m.Retries, &m.RetryIntervalSeconds,
		&m.TimeoutSeconds, &m.MaxRedirects, &m.AcceptedStatus, bodyEncoding, &m.Keyword,
		&m.RecoverySuccesses, &m.CertNotify, &m.CertWarnDays, &m.CertCritDays, &m.GroupID,
		&m.Description, &m.Tags, &m.Active, &m.AlertsMutedUntil, authType, &m.HeaderNames,
		&m.SecretConfigured, &m.CreatedAt, &m.CreatedBy, &m.UpdatedAt, &m.UpdatedBy}
}

func finishMonitor(m *coremonitor.Monitor, method, bodyEncoding, authType string) {
	m.Method = coremonitor.Method(method)
	m.BodyEncoding = coremonitor.BodyEncoding(bodyEncoding)
	m.AuthType = coremonitor.AuthType(authType)
}

const liveColumns = `s.status, s.consecutive_failures, s.status_since, s.down_since, s.last_check_at,
	s.last_status_code, s.last_response_ms, s.last_failure, s.next_check_at, s.cert_expires_at`

func scanView(row pgx.Row) (coremonitor.View, error) {
	var v coremonitor.View
	var method, bodyEncoding, authType, status, failure string
	dest := append(monitorDest(&v.Monitor, &method, &bodyEncoding, &authType),
		&status, &v.Live.ConsecutiveFailures, &v.Live.StatusSince, &v.Live.DownSince, &v.Live.LastCheckAt,
		&v.Live.LastStatusCode, &v.Live.LastResponseMS, &failure, &v.Live.NextCheckAt, &v.Live.CertExpiresAt,
		&v.ChannelIDs)
	if err := row.Scan(dest...); err != nil {
		return coremonitor.View{}, err
	}
	finishMonitor(&v.Monitor, method, bodyEncoding, authType)
	v.Live.Status = coremonitor.Status(status)
	v.Live.LastFailure = coremonitor.Failure(failure)
	return v, nil
}

const viewQuery = `SELECT ` + monitorColumns + `, ` + liveColumns + `,
	ARRAY(SELECT c.channel_id FROM monitor_channels c WHERE c.monitor_id = m.id ORDER BY c.channel_id)
	FROM monitors m JOIN monitor_status s ON s.monitor_id = m.id`

func notFound(op, monitorID string) error {
	return errs.New(errs.CodeNotFound, "monitor not found").WithOp(op).WithDetail("monitor_id", monitorID)
}

// writeError maps constraint violations on a monitor write to client errors.
func writeError(op string, err error) error {
	switch pgCode(err) {
	case "23503":
		return errs.New(errs.CodeNotFound, "monitor group not found").WithOp(op).WithDetail("field", "group_id")
	case "23514":
		return errs.New(errs.CodeInvalidArgument, "the monitor settings are not valid").WithOp(op).
			WithDetail("constraint", pgConstraint(err))
	}
	return errs.Wrap(err, errs.CodeUnavailable, "could not save the monitor").WithOp(op)
}

// GetMonitor returns one monitor with its live state.
func (r *Repository) GetMonitor(ctx context.Context, monitorID string) (coremonitor.View, error) {
	const op = "monitor.Repository.GetMonitor"
	v, err := scanView(r.pool.QueryRow(ctx, viewQuery+` WHERE m.id = $1`, monitorID))
	if errors.Is(err, pgx.ErrNoRows) {
		return coremonitor.View{}, notFound(op, monitorID)
	}
	if err != nil {
		return coremonitor.View{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor").WithOp(op)
	}
	return v, nil
}

// StoredMonitor returns a monitor's configuration and decrypted secret
// for an on-demand test. It only reads.
func (r *Repository) StoredMonitor(ctx context.Context, monitorID string) (coremonitor.Monitor, coremonitor.Secret, error) {
	const op = "monitor.Repository.StoredMonitor"
	var m coremonitor.Monitor
	var method, bodyEncoding, authType string
	var ciphertext []byte
	var keyID *string
	dest := append(monitorDest(&m, &method, &bodyEncoding, &authType), &ciphertext, &keyID)
	err := r.pool.QueryRow(ctx, `SELECT `+monitorColumns+`, m.secret_ciphertext, m.secret_key_id
		FROM monitors m WHERE m.id = $1`, monitorID).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return coremonitor.Monitor{}, coremonitor.Secret{}, notFound(op, monitorID)
	}
	if err != nil {
		return coremonitor.Monitor{}, coremonitor.Secret{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor").WithOp(op)
	}
	finishMonitor(&m, method, bodyEncoding, authType)
	secret, ok := r.open(monitorID, ciphertext, keyID)
	if !ok {
		return coremonitor.Monitor{}, coremonitor.Secret{}, errs.New(errs.CodeFailedPrecondition,
			"the monitor's stored credentials cannot be decrypted with the configured key; enter them again").WithOp(op)
	}
	return m, secret, nil
}

// ListMonitors returns every monitor with its live state, by group
// position, then name.
func (r *Repository) ListMonitors(ctx context.Context) ([]coremonitor.View, error) {
	const op = "monitor.Repository.ListMonitors"
	rows, err := r.pool.Query(ctx, viewQuery+`
		LEFT JOIN monitor_groups g ON g.id = m.group_id
		ORDER BY g.position NULLS LAST, g.name NULLS LAST, lower(m.name), m.id`)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list monitors").WithOp(op)
	}
	defer rows.Close()
	out := []coremonitor.View{}
	for rows.Next() {
		v, err := scanView(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a monitor").WithOp(op)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list monitors").WithOp(op)
	}
	return out, nil
}

// CreateMonitor stores a new monitor and its secret. The first check is due
// within min(interval, 10s), jittered so monitors created together do not
// probe in lockstep.
func (r *Repository) CreateMonitor(ctx context.Context, m coremonitor.Monitor, s coremonitor.Secret, by string) (coremonitor.View, error) {
	const op = "monitor.Repository.CreateMonitor"
	m.ID = id.New()
	sealed, err := r.seal(op, m.ID, s)
	if err != nil {
		return coremonitor.View{}, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return coremonitor.View{}, errs.Wrap(err, errs.CodeUnavailable, "could not begin creating the monitor").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO monitors (id, name, url, method, interval_seconds, retries, retry_interval_seconds, timeout_seconds,
			max_redirects, accepted_status, body_encoding, keyword, recovery_successes, cert_notify, cert_warn_days,
			cert_crit_days, group_id, description, tags, active, auth_type, header_names, secret_ciphertext,
			secret_key_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULLIF($12, ''), $13, $14, $15, $16, NULLIF($17, ''),
			$18, $19, $20, $21, $22, $23, $24, $25, $25)`,
		append(append(monitorArgs(m), sealedArgs(sealed)...), by)...); err != nil {
		return coremonitor.View{}, writeError(op, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO monitor_status (monitor_id, next_check_at)
		VALUES ($1, now() + make_interval(secs => random() * LEAST($2, 10)))`, m.ID, m.IntervalSeconds); err != nil {
		return coremonitor.View{}, writeError(op, err)
	}
	if m.ChannelIDs == nil {
		if _, err = tx.Exec(ctx, `INSERT INTO monitor_channels (monitor_id, channel_id)
			SELECT $1, id FROM notification_channels WHERE monitor_default`, m.ID); err != nil {
			return coremonitor.View{}, writeError(op, err)
		}
	} else if err = replaceChannels(ctx, tx, op, m.ID, m.ChannelIDs); err != nil {
		return coremonitor.View{}, err
	}
	v, err := scanView(tx.QueryRow(ctx, viewQuery+` WHERE m.id = $1`, m.ID))
	if err != nil {
		return coremonitor.View{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the new monitor").WithOp(op)
	}
	if err := tx.Commit(ctx); err != nil {
		return coremonitor.View{}, errs.Wrap(err, errs.CodeUnavailable, "could not create the monitor").WithOp(op)
	}
	return v, nil
}

// monitorArgs are $1..$22 of the insert; sealedArgs are $23, $24.
func monitorArgs(m coremonitor.Monitor) []any {
	return []any{m.ID, m.Name, m.URL, string(m.Method), m.IntervalSeconds, m.Retries, m.RetryIntervalSeconds,
		m.TimeoutSeconds, m.MaxRedirects, m.AcceptedStatus, string(m.BodyEncoding), m.Keyword, m.RecoverySuccesses,
		m.CertNotify, m.CertWarnDays, m.CertCritDays, m.GroupID, m.Description, nonNil(m.Tags), m.Active,
		string(m.AuthType), nonNil(m.HeaderNames)}
}

func sealedArgs(s *secretbox.Sealed) []any {
	if s == nil {
		return []any{nil, nil}
	}
	return []any{s.Ciphertext, s.KeyID}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// UpdateMonitor replaces a monitor's configuration with what apply returns,
// under a row lock so concurrent edits cannot interleave. An active
// monitor's next check is brought forward to within 5s, so a fixed URL is
// confirmed promptly; the failure streak is kept.
func (r *Repository) UpdateMonitor(ctx context.Context, monitorID string, apply coremonitor.UpdateFunc, by string) (coremonitor.View, error) {
	const op = "monitor.Repository.UpdateMonitor"
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return coremonitor.View{}, errs.Wrap(err, errs.CodeUnavailable, "could not begin updating the monitor").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var stored coremonitor.Monitor
	var method, bodyEncoding, authType string
	var ciphertext []byte
	var keyID *string
	dest := append(monitorDest(&stored, &method, &bodyEncoding, &authType), &ciphertext, &keyID)
	err = tx.QueryRow(ctx, `SELECT `+monitorColumns+`, m.secret_ciphertext, m.secret_key_id
		FROM monitors m WHERE m.id = $1 FOR UPDATE`, monitorID).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return coremonitor.View{}, notFound(op, monitorID)
	}
	if err != nil {
		return coremonitor.View{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor").WithOp(op)
	}
	finishMonitor(&stored, method, bodyEncoding, authType)
	secret, ok := r.open(monitorID, ciphertext, keyID)
	if !ok {
		return coremonitor.View{}, errs.New(errs.CodeFailedPrecondition,
			"the monitor's stored credentials cannot be decrypted with the configured key; delete and re-create the monitor").WithOp(op)
	}

	next, nextSecret, err := apply(stored, secret)
	if err != nil {
		return coremonitor.View{}, err
	}
	sealed, err := r.seal(op, monitorID, nextSecret)
	if err != nil {
		return coremonitor.View{}, err
	}
	var ct []byte
	var kid *string
	if sealed != nil {
		ct, kid = sealed.Ciphertext, &sealed.KeyID
	}
	if _, err := tx.Exec(ctx, `
		UPDATE monitors SET name = $2, url = $3, method = $4, interval_seconds = $5, retries = $6,
			retry_interval_seconds = $7, timeout_seconds = $8, max_redirects = $9, accepted_status = $10,
			body_encoding = $11, keyword = NULLIF($12, ''), recovery_successes = $13, cert_notify = $14,
			cert_warn_days = $15, cert_crit_days = $16, group_id = NULLIF($17, ''), description = $18, tags = $19,
			auth_type = $20, header_names = $21, secret_ciphertext = $22, secret_key_id = $23,
			updated_at = now(), updated_by = $24
		WHERE id = $1`,
		monitorID, next.Name, next.URL, string(next.Method), next.IntervalSeconds, next.Retries,
		next.RetryIntervalSeconds, next.TimeoutSeconds, next.MaxRedirects, next.AcceptedStatus,
		string(next.BodyEncoding), next.Keyword, next.RecoverySuccesses, next.CertNotify, next.CertWarnDays,
		next.CertCritDays, next.GroupID, next.Description, nonNil(next.Tags), string(next.AuthType),
		nonNil(next.HeaderNames), ct, kid, by); err != nil {
		return coremonitor.View{}, writeError(op, err)
	}
	if next.ChannelIDs != nil {
		if err = replaceChannels(ctx, tx, op, monitorID, next.ChannelIDs); err != nil {
			return coremonitor.View{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE monitor_status SET next_check_at = LEAST(next_check_at, now() + make_interval(secs => random() * 5))
		WHERE monitor_id = $1`, monitorID); err != nil {
		return coremonitor.View{}, writeError(op, err)
	}
	v, err := scanView(tx.QueryRow(ctx, viewQuery+` WHERE m.id = $1`, monitorID))
	if err != nil {
		return coremonitor.View{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor").WithOp(op)
	}
	if err := tx.Commit(ctx); err != nil {
		return coremonitor.View{}, errs.Wrap(err, errs.CodeUnavailable, "could not update the monitor").WithOp(op)
	}
	return v, nil
}

// SetMonitorActive pauses or resumes a monitor. Resuming starts from a clean
// slate: status unknown, no failure streak, first check within 10s.
func (r *Repository) SetMonitorActive(ctx context.Context, monitorID string, active bool, by string) (coremonitor.View, error) {
	const op = "monitor.Repository.SetMonitorActive"
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return coremonitor.View{}, errs.Wrap(err, errs.CodeUnavailable, "could not begin updating the monitor").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var was bool
	err = tx.QueryRow(ctx, `SELECT active FROM monitors WHERE id = $1 FOR UPDATE`, monitorID).Scan(&was)
	if errors.Is(err, pgx.ErrNoRows) {
		return coremonitor.View{}, notFound(op, monitorID)
	}
	if err != nil {
		return coremonitor.View{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor").WithOp(op)
	}
	if was != active {
		if _, err := tx.Exec(ctx, `UPDATE monitors SET active = $2, updated_at = now(), updated_by = $3 WHERE id = $1`,
			monitorID, active, by); err != nil {
			return coremonitor.View{}, writeError(op, err)
		}
		if active {
			if _, err := tx.Exec(ctx, `
				UPDATE monitor_status s SET status = 'unknown', consecutive_failures = 0, consecutive_successes = 0,
					status_since = now(), down_since = NULL, updated_at = now(),
					next_check_at = now() + make_interval(secs => random() * LEAST(m.interval_seconds, 10))
				FROM monitors m WHERE m.id = s.monitor_id AND s.monitor_id = $1`, monitorID); err != nil {
				return coremonitor.View{}, writeError(op, err)
			}
		}
	}
	v, err := scanView(tx.QueryRow(ctx, viewQuery+` WHERE m.id = $1`, monitorID))
	if err != nil {
		return coremonitor.View{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor").WithOp(op)
	}
	if err := tx.Commit(ctx); err != nil {
		return coremonitor.View{}, errs.Wrap(err, errs.CodeUnavailable, "could not update the monitor").WithOp(op)
	}
	return v, nil
}

// DeleteMonitor removes a monitor with its heartbeats, events, channel
// links and notification state.
func (r *Repository) DeleteMonitor(ctx context.Context, monitorID string) error {
	const op = "monitor.Repository.DeleteMonitor"
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not begin deleting the monitor").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `DELETE FROM monitors WHERE id = $1`, monitorID)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not delete the monitor").WithOp(op)
	}
	if tag.RowsAffected() == 0 {
		return notFound(op, monitorID)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM monitor_heartbeats WHERE monitor_id = $1`, monitorID); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not delete the monitor's heartbeats").WithOp(op)
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not delete the monitor").WithOp(op)
	}
	return nil
}

// ------------------------------------------------------------ history ----

func scanHeartbeat(rows pgx.Rows) (coremonitor.Heartbeat, error) {
	var h coremonitor.Heartbeat
	var status, failure string
	var code *int16
	if err := rows.Scan(&h.Time, &status, &code, &h.ResponseMS, &failure); err != nil {
		return coremonitor.Heartbeat{}, err
	}
	h.Status, h.Failure = coremonitor.Status(status), coremonitor.Failure(failure)
	if code != nil {
		c := int(*code)
		h.StatusCode = &c
	}
	return h, nil
}

// RecentHeartbeats returns up to n of each monitor's newest heartbeats,
// oldest first, for the heartbeat bar.
func (r *Repository) RecentHeartbeats(ctx context.Context, monitorIDs []string, n int) (map[string][]coremonitor.Heartbeat, error) {
	const op = "monitor.Repository.RecentHeartbeats"
	rows, err := r.pool.Query(ctx, `
		SELECT ids.id, h.time, h.status, h.status_code, h.response_ms, h.failure
		FROM unnest($1::text[]) AS ids(id)
		CROSS JOIN LATERAL (
			SELECT time, status, status_code, response_ms, failure FROM monitor_heartbeats
			WHERE monitor_id = ids.id ORDER BY time DESC LIMIT $2
		) h
		ORDER BY ids.id, h.time`, monitorIDs, n)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read heartbeats").WithOp(op)
	}
	defer rows.Close()
	out := make(map[string][]coremonitor.Heartbeat, len(monitorIDs))
	for rows.Next() {
		var monitorID string
		var h coremonitor.Heartbeat
		var status, failure string
		var code *int16
		if err := rows.Scan(&monitorID, &h.Time, &status, &code, &h.ResponseMS, &failure); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a heartbeat").WithOp(op)
		}
		h.Status, h.Failure = coremonitor.Status(status), coremonitor.Failure(failure)
		if code != nil {
			c := int(*code)
			h.StatusCode = &c
		}
		out[monitorID] = append(out[monitorID], h)
	}
	return out, rows.Err()
}

// Heartbeats returns one monitor's heartbeats since since, oldest first,
// at most limit (the newest ones when more exist).
func (r *Repository) Heartbeats(ctx context.Context, monitorID string, since time.Time, limit int) ([]coremonitor.Heartbeat, error) {
	const op = "monitor.Repository.Heartbeats"
	rows, err := r.pool.Query(ctx, `
		SELECT time, status, status_code, response_ms, failure FROM (
			SELECT time, status, status_code, response_ms, failure FROM monitor_heartbeats
			WHERE monitor_id = $1 AND time >= $2 ORDER BY time DESC LIMIT $3
		) h ORDER BY time`, monitorID, since, limit)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read heartbeats").WithOp(op)
	}
	defer rows.Close()
	out := []coremonitor.Heartbeat{}
	for rows.Next() {
		h, err := scanHeartbeat(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a heartbeat").WithOp(op)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HourlyHistory returns one monitor's hourly roll-up since since, oldest
// first.
func (r *Repository) HourlyHistory(ctx context.Context, monitorID string, since time.Time) ([]coremonitor.HourlyBucket, error) {
	const op = "monitor.Repository.HourlyHistory"
	rows, err := r.pool.Query(ctx, `
		SELECT bucket, up_count, pending_count, down_count, unknown_count, avg_response_ms::float8, max_response_ms
		FROM monitor_heartbeats_1h WHERE monitor_id = $1 AND bucket >= $2 ORDER BY bucket`, monitorID, since)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor history").WithOp(op)
	}
	defer rows.Close()
	out := []coremonitor.HourlyBucket{}
	for rows.Next() {
		var b coremonitor.HourlyBucket
		if err := rows.Scan(&b.Bucket, &b.Up, &b.Pending, &b.Down, &b.Unknown, &b.AvgResponseMS, &b.MaxResponseMS); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read the monitor history").WithOp(op)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UptimeSince counts each monitor's heartbeats since since.
func (r *Repository) UptimeSince(ctx context.Context, monitorIDs []string, since time.Time) (map[string]coremonitor.UptimeCount, error) {
	const op = "monitor.Repository.UptimeSince"
	rows, err := r.pool.Query(ctx, `
		SELECT monitor_id,
			count(*) FILTER (WHERE status IN ('up', 'pending')),
			count(*) FILTER (WHERE status <> 'unknown')
		FROM monitor_heartbeats WHERE monitor_id = ANY($1) AND time >= $2
		GROUP BY monitor_id`, monitorIDs, since)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not compute uptime").WithOp(op)
	}
	defer rows.Close()
	out := make(map[string]coremonitor.UptimeCount, len(monitorIDs))
	for rows.Next() {
		var monitorID string
		var u coremonitor.UptimeCount
		if err := rows.Scan(&monitorID, &u.Up, &u.Counted); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not compute uptime").WithOp(op)
		}
		out[monitorID] = u
	}
	return out, rows.Err()
}

// Uptime is one monitor's uptime over the last day, week and 30 days.
func (r *Repository) Uptime(ctx context.Context, monitorID string, now time.Time) (coremonitor.Uptime, error) {
	const op = "monitor.Repository.Uptime"
	var u coremonitor.Uptime
	err := r.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE time >= $2 AND status IN ('up', 'pending')),
			count(*) FILTER (WHERE time >= $2 AND status <> 'unknown'),
			count(*) FILTER (WHERE time >= $3 AND status IN ('up', 'pending')),
			count(*) FILTER (WHERE time >= $3 AND status <> 'unknown'),
			count(*) FILTER (WHERE status IN ('up', 'pending')),
			count(*) FILTER (WHERE status <> 'unknown')
		FROM monitor_heartbeats WHERE monitor_id = $1 AND time >= $4`,
		monitorID, now.Add(-24*time.Hour), now.Add(-7*24*time.Hour), now.Add(-30*24*time.Hour)).
		Scan(&u.Day.Up, &u.Day.Counted, &u.Week.Up, &u.Week.Counted, &u.Month.Up, &u.Month.Counted)
	if err != nil {
		return coremonitor.Uptime{}, errs.Wrap(err, errs.CodeUnavailable, "could not compute uptime").WithOp(op)
	}
	return u, nil
}

// Events returns a monitor's events older than before, newest first.
func (r *Repository) Events(ctx context.Context, monitorID string, before time.Time, limit int) ([]coremonitor.Event, error) {
	const op = "monitor.Repository.Events"
	rows, err := r.pool.Query(ctx, `
		SELECT id, monitor_id, time, kind, status_code, failure, duration_seconds, cert_expires_at FROM monitor_events
		WHERE monitor_id = $1 AND time < $2 ORDER BY time DESC, id DESC LIMIT $3`, monitorID, before, limit)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor events").WithOp(op)
	}
	defer rows.Close()
	out := []coremonitor.Event{}
	for rows.Next() {
		var e coremonitor.Event
		var kind, failure string
		if err := rows.Scan(&e.ID, &e.MonitorID, &e.Time, &kind, &e.StatusCode, &failure, &e.DurationSeconds, &e.CertExpiresAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a monitor event").WithOp(op)
		}
		e.Kind, e.Failure = coremonitor.EventKind(kind), coremonitor.Failure(failure)
		out = append(out, e)
	}
	return out, rows.Err()
}
