package monitor

import (
	"context"
	"errors"
	"slices"

	coremonitor "github.com/hexane/atlas/internal/core/monitor"
	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/jackc/pgx/v5"
)

var (
	_ corenotification.MonitorSource        = (*Repository)(nil)
	_ corenotification.MonitorChannelSource = (*Repository)(nil)
	_ corenotification.MonitorInfoSource    = (*Repository)(nil)
)

// replaceChannels makes ids the monitor's whole channel selection.
func replaceChannels(ctx context.Context, tx pgx.Tx, op, monitorID string, ids []string) error {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if ids == nil {
		ids = []string{} // NULL would match no row in the delete
	}
	if _, err := tx.Exec(ctx, `DELETE FROM monitor_channels WHERE monitor_id = $1 AND NOT (channel_id = ANY($2))`,
		monitorID, ids); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not save the monitor's channels").WithOp(op)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO monitor_channels (monitor_id, channel_id)
		SELECT $1, unnest($2::text[]) ON CONFLICT DO NOTHING`, monitorID, ids); err != nil {
		if pgCode(err) == "23503" {
			return errs.New(errs.CodeInvalidArgument, "a selected notification channel does not exist").
				WithOp(op).WithDetail("field", "channel_ids")
		}
		return errs.Wrap(err, errs.CodeUnavailable, "could not save the monitor's channels").WithOp(op)
	}
	return nil
}

// SetMonitorChannels replaces the monitor's channel selection and returns
// it as stored.
func (r *Repository) SetMonitorChannels(ctx context.Context, monitorID string, ids []string) ([]string, error) {
	const op = "monitor.Repository.SetMonitorChannels"
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not begin saving the monitor's channels").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked string
	err = tx.QueryRow(ctx, `SELECT id FROM monitors WHERE id = $1 FOR UPDATE`, monitorID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound(op, monitorID)
	}
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor").WithOp(op)
	}
	if err = replaceChannels(ctx, tx, op, monitorID, ids); err != nil {
		return nil, err
	}
	out, err := channelIDs(ctx, tx, monitorID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor's channels").WithOp(op)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not save the monitor's channels").WithOp(op)
	}
	return out, nil
}

// MonitorChannelIDs implements [corenotification.MonitorChannelSource].
func (r *Repository) MonitorChannelIDs(ctx context.Context, monitorID string) ([]string, error) {
	out, err := channelIDs(ctx, r.pool, monitorID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor's channels").WithOp("monitor.Repository.MonitorChannelIDs")
	}
	return out, nil
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func channelIDs(ctx context.Context, q queryRower, monitorID string) ([]string, error) {
	var out []string
	err := q.QueryRow(ctx, `SELECT ARRAY(SELECT channel_id FROM monitor_channels WHERE monitor_id = $1 ORDER BY channel_id)`,
		monitorID).Scan(&out)
	return out, err
}

// MonitorInfo implements [corenotification.MonitorInfoSource]: the name and
// the URL's host, nothing more of the URL.
func (r *Repository) MonitorInfo(ctx context.Context, monitorID string) (name, host string, err error) {
	const op = "monitor.Repository.MonitorInfo"
	var url string
	err = r.pool.QueryRow(ctx, `SELECT name, url FROM monitors WHERE id = $1`, monitorID).Scan(&name, &url)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", notFound(op, monitorID)
	}
	if err != nil {
		return "", "", errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor").WithOp(op)
	}
	return name, coremonitor.Host(url), nil
}

// ListEvalMonitors implements [corenotification.MonitorSource].
func (r *Repository) ListEvalMonitors(ctx context.Context) ([]corenotification.EvalMonitor, error) {
	const op = "monitor.Repository.ListEvalMonitors"
	rows, err := r.pool.Query(ctx, `
		SELECT m.id, m.active, m.alerts_muted_until, s.status, s.status_since, s.down_since, s.last_failure,
			s.last_status_code, s.last_response_ms, m.cert_notify, m.cert_warn_days, m.cert_crit_days, s.cert_expires_at
		FROM monitors m JOIN monitor_status s ON s.monitor_id = m.id ORDER BY m.id`)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list monitors").WithOp(op)
	}
	defer rows.Close()
	out := []corenotification.EvalMonitor{}
	for rows.Next() {
		var m corenotification.EvalMonitor
		if err := rows.Scan(&m.MonitorID, &m.Active, &m.AlertsMutedUntil, &m.Status, &m.StatusSince, &m.DownSince,
			&m.LastFailure, &m.LastStatusCode, &m.LastResponseMS, &m.CertNotify, &m.CertWarnDays, &m.CertCritDays,
			&m.CertExpiresAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a monitor").WithOp(op)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list monitors").WithOp(op)
	}
	return out, nil
}
