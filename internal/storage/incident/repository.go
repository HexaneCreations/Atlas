package incident

import (
	"context"
	"fmt"
	"time"

	coreincident "github.com/hexane/atlas/internal/core/incident"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultListLimit = 50
	maxListLimit     = 500
)

// Repository reads and writes incidents and their members.
type Repository struct{ pool *pgxpool.Pool }

// NewRepository builds a repository over a started pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

const incidentColumns = `id, title, status, severity, root_cause_kind, root_cause_ref_id, root_cause_topic,
	opened_at, updated_at, resolved_at`

func scanIncident(row pgx.Row) (coreincident.Incident, error) {
	var (
		inc                      coreincident.Incident
		rcKind, rcRefID, rcTopic *string
		resolvedAt               *time.Time
	)
	err := row.Scan(&inc.ID, &inc.Title, &inc.Status, &inc.Severity, &rcKind, &rcRefID, &rcTopic,
		&inc.OpenedAt, &inc.UpdatedAt, &resolvedAt)
	if err != nil {
		return coreincident.Incident{}, err
	}
	if rcKind != nil {
		inc.RootCauseKind = coreincident.MemberKind(*rcKind)
	}
	if rcRefID != nil {
		inc.RootCauseRefID = *rcRefID
	}
	if rcTopic != nil {
		inc.RootCauseTopic = *rcTopic
	}
	if resolvedAt != nil {
		inc.ResolvedAt = *resolvedAt
	}
	return inc, nil
}

// FindCorrelatable returns the most recently updated open incident with a
// member on nodeID at or after since.
func (r *Repository) FindCorrelatable(ctx context.Context, nodeID string, since time.Time) (coreincident.Incident, bool, error) {
	const op = "incident.Repository.FindCorrelatable"

	const q = `
		SELECT ` + incidentColumns + ` FROM incidents i
		WHERE i.status = 'open' AND EXISTS (
			SELECT 1 FROM incident_members m
			WHERE m.incident_id = i.id AND m.node_id = $1 AND m.time >= $2
		)
		ORDER BY i.updated_at DESC
		LIMIT 1`

	row := r.pool.QueryRow(ctx, q, nodeID, since)
	inc, err := scanIncident(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return coreincident.Incident{}, false, nil
		}
		return coreincident.Incident{}, false, errs.Wrap(err, errs.CodeUnavailable, "could not search for a correlatable incident").WithOp(op)
	}
	return inc, true, nil
}

// FindCorrelatableByEnvironment returns the most recently updated open
// incident with a member on some node tagged with environment, at or after
// since — the environment correlation tier. See [coreincident.Engine.correlate].
func (r *Repository) FindCorrelatableByEnvironment(ctx context.Context, environment string, since time.Time) (coreincident.Incident, bool, error) {
	const op = "incident.Repository.FindCorrelatableByEnvironment"

	const q = `
		SELECT ` + incidentColumns + ` FROM incidents i
		WHERE i.status = 'open' AND EXISTS (
			SELECT 1 FROM incident_members m
			JOIN nodes n ON n.node_id = m.node_id
			WHERE m.incident_id = i.id AND n.environment = $1 AND m.time >= $2
		)
		ORDER BY i.updated_at DESC
		LIMIT 1`

	row := r.pool.QueryRow(ctx, q, environment, since)
	inc, err := scanIncident(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return coreincident.Incident{}, false, nil
		}
		return coreincident.Incident{}, false, errs.Wrap(err, errs.CodeUnavailable, "could not search for a correlatable incident by environment").WithOp(op)
	}
	return inc, true, nil
}

// CreateIncident persists a new incident.
func (r *Repository) CreateIncident(ctx context.Context, inc coreincident.Incident) (coreincident.Incident, error) {
	const op = "incident.Repository.CreateIncident"

	const q = `
		INSERT INTO incidents (id, title, status, severity, root_cause_kind, root_cause_ref_id, root_cause_topic,
			opened_at, updated_at, resolved_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

	_, err := r.pool.Exec(ctx, q, inc.ID, inc.Title, inc.Status, inc.Severity,
		nullString(string(inc.RootCauseKind)), nullString(inc.RootCauseRefID), nullString(inc.RootCauseTopic),
		inc.OpenedAt, inc.UpdatedAt, nullTime(inc.ResolvedAt))
	if err != nil {
		return coreincident.Incident{}, errs.Wrap(err, errs.CodeUnavailable, "could not create the incident").WithOp(op)
	}
	return inc, nil
}

// UpdateIncident persists changes to status, severity, and timestamps.
func (r *Repository) UpdateIncident(ctx context.Context, inc coreincident.Incident) error {
	const op = "incident.Repository.UpdateIncident"

	const q = `
		UPDATE incidents SET status = $2, severity = $3, updated_at = $4, resolved_at = $5
		WHERE id = $1`

	tag, err := r.pool.Exec(ctx, q, inc.ID, inc.Status, inc.Severity, inc.UpdatedAt, nullTime(inc.ResolvedAt))
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not update the incident").WithOp(op)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeNotFound, "incident not found").WithOp(op).WithDetail("id", inc.ID)
	}
	return nil
}

// AddMember attaches an event or alert entry to an incident. Idempotent on
// (kind, ref_id): offering the same occurrence twice is a no-op.
func (r *Repository) AddMember(ctx context.Context, m coreincident.Member) error {
	const op = "incident.Repository.AddMember"

	const q = `
		INSERT INTO incident_members (id, incident_id, kind, ref_id, node_id, topic, severity, time, is_root_cause)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (kind, ref_id) DO NOTHING`

	_, err := r.pool.Exec(ctx, q, m.ID, m.IncidentID, m.Kind, m.RefID, m.NodeID, m.Topic, m.Severity, m.Time, m.IsRootCause)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not add the incident member").WithOp(op)
	}
	return nil
}

// GetIncident returns one incident by id.
func (r *Repository) GetIncident(ctx context.Context, incidentID string) (coreincident.Incident, error) {
	const op = "incident.Repository.GetIncident"

	row := r.pool.QueryRow(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id = $1`, incidentID)
	inc, err := scanIncident(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return coreincident.Incident{}, errs.New(errs.CodeNotFound, "incident not found").WithOp(op).WithDetail("id", incidentID)
		}
		return coreincident.Incident{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the incident").WithOp(op)
	}
	return inc, nil
}

// ListIncidents returns incidents matching filter, most recently updated
// first.
func (r *Repository) ListIncidents(ctx context.Context, filter coreincident.Filter) ([]coreincident.Incident, error) {
	const op = "incident.Repository.ListIncidents"

	limit := filter.Limit
	if limit <= 0 || limit > maxListLimit {
		limit = defaultListLimit
	}

	q := `SELECT ` + incidentColumns + ` FROM incidents i WHERE true`
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if filter.Status != "" {
		q += " AND i.status = " + arg(filter.Status)
	}
	if filter.NodeID != "" {
		q += " AND EXISTS (SELECT 1 FROM incident_members m WHERE m.incident_id = i.id AND m.node_id = " + arg(filter.NodeID) + ")"
	}
	if !filter.Since.IsZero() {
		q += " AND i.opened_at >= " + arg(filter.Since)
	}
	if !filter.Before.IsZero() {
		q += " AND i.opened_at < " + arg(filter.Before)
	}
	q += " ORDER BY i.updated_at DESC LIMIT " + arg(limit)

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list incidents").WithOp(op)
	}
	defer rows.Close()

	incidents := []coreincident.Incident{}
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read an incident").WithOp(op)
		}
		incidents = append(incidents, inc)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list incidents").WithOp(op)
	}
	return incidents, nil
}

// ListOpenIncidents returns every open incident, for the resolver sweep.
func (r *Repository) ListOpenIncidents(ctx context.Context) ([]coreincident.Incident, error) {
	return r.ListIncidents(ctx, coreincident.Filter{Status: coreincident.StatusOpen, Limit: maxListLimit})
}

// GetDetail returns an incident with its members and the distinct nodes and
// subjects its members touch, computed from the members themselves rather
// than a redundant denormalized copy on the incident row.
func (r *Repository) GetDetail(ctx context.Context, incidentID string) (coreincident.Detail, error) {
	const op = "incident.Repository.GetDetail"

	inc, err := r.GetIncident(ctx, incidentID)
	if err != nil {
		return coreincident.Detail{}, err
	}

	const q = `
		SELECT id, incident_id, kind, ref_id, node_id, topic, severity, time, is_root_cause
		FROM incident_members WHERE incident_id = $1 ORDER BY time`
	rows, err := r.pool.Query(ctx, q, incidentID)
	if err != nil {
		return coreincident.Detail{}, errs.Wrap(err, errs.CodeUnavailable, "could not read incident members").WithOp(op)
	}
	defer rows.Close()

	nodeSeen := map[string]bool{}
	subjectSeen := map[string]bool{}
	detail := coreincident.Detail{Incident: inc}

	for rows.Next() {
		var m coreincident.Member
		if err := rows.Scan(&m.ID, &m.IncidentID, &m.Kind, &m.RefID, &m.NodeID, &m.Topic, &m.Severity, &m.Time, &m.IsRootCause); err != nil {
			return coreincident.Detail{}, errs.Wrap(err, errs.CodeInternal, "could not read an incident member").WithOp(op)
		}
		detail.Members = append(detail.Members, m)
		if m.NodeID != "" && !nodeSeen[m.NodeID] {
			nodeSeen[m.NodeID] = true
			detail.AffectedNodes = append(detail.AffectedNodes, m.NodeID)
		}
	}
	if err := rows.Err(); err != nil {
		return coreincident.Detail{}, errs.Wrap(err, errs.CodeUnavailable, "could not read incident members").WithOp(op)
	}

	// Affected subjects come from the referenced events' Subject — the
	// resource an event is about (a container id, a unit name). Members
	// only carry topic, not subject, so this is a second, targeted query
	// against the event store rather than widening incident_members with a
	// column duplicated from a table that already has it.
	subjects, err := r.eventSubjects(ctx, detail.Members)
	if err != nil {
		return coreincident.Detail{}, err
	}
	for _, s := range subjects {
		if s != "" && !subjectSeen[s] {
			subjectSeen[s] = true
			detail.AffectedSubjects = append(detail.AffectedSubjects, s)
		}
	}

	return detail, nil
}

func (r *Repository) eventSubjects(ctx context.Context, members []coreincident.Member) ([]string, error) {
	const op = "incident.Repository.eventSubjects"

	var eventIDs []string
	for _, m := range members {
		if m.Kind == coreincident.MemberEvent {
			eventIDs = append(eventIDs, m.RefID)
		}
	}
	if len(eventIDs) == 0 {
		return nil, nil
	}

	rows, err := r.pool.Query(ctx, `SELECT DISTINCT subject FROM events WHERE id = ANY($1) AND subject != ''`, eventIDs)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read event subjects").WithOp(op)
	}
	defer rows.Close()

	var subjects []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a subject").WithOp(op)
		}
		subjects = append(subjects, s)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read event subjects").WithOp(op)
	}
	return subjects, nil
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

var _ coreincident.Store = (*Repository)(nil)
