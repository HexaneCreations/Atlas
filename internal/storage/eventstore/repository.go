package eventstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	coreeventstore "github.com/hexane/atlas/internal/core/eventstore"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultQueryLimit = 100
	maxQueryLimit     = 500
)

// Repository reads and writes the durable event log.
type Repository struct{ pool *pgxpool.Pool }

// NewRepository builds a repository over a started pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

const insertSQL = `
	INSERT INTO events (id, time, node_id, topic, source, subject, payload, received_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	ON CONFLICT (id, time) DO NOTHING`

// Insert persists one event record. Idempotent on (id, time): a retried
// delivery — from the bus's own retry-unaware forwarding, or from a spooled
// envelope replayed after an outage — does not duplicate the row.
func (r *Repository) Insert(ctx context.Context, rec coreeventstore.Record) error {
	const op = "eventstore.Repository.Insert"

	if rec.ID == "" || rec.NodeID == "" || rec.Topic == "" {
		return errs.New(errs.CodeInvalidArgument, "event record requires an id, node id, and topic").WithOp(op)
	}
	payload := rec.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	if rec.ReceivedAt.IsZero() {
		rec.ReceivedAt = time.Now()
	}

	_, err := r.pool.Exec(ctx, insertSQL,
		rec.ID, rec.Time, rec.NodeID, rec.Topic, rec.Source, rec.Subject, payload, rec.ReceivedAt)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not persist event").
			WithOp(op).WithDetail("topic", rec.Topic)
	}
	return nil
}

// Query returns events matching filter, newest first.
func (r *Repository) Query(ctx context.Context, filter coreeventstore.Filter) ([]coreeventstore.Record, error) {
	const op = "eventstore.Repository.Query"

	limit := filter.Limit
	if limit <= 0 || limit > maxQueryLimit {
		limit = defaultQueryLimit
	}

	q := `SELECT id, time, node_id, topic, source, subject, payload, received_at FROM events WHERE true`
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if filter.NodeID != "" {
		q += " AND node_id = " + arg(filter.NodeID)
	}
	if filter.Topic != "" {
		q += " AND topic = " + arg(filter.Topic)
	}
	if !filter.Since.IsZero() {
		q += " AND time >= " + arg(filter.Since)
	}
	if !filter.Until.IsZero() {
		q += " AND time <= " + arg(filter.Until)
	}
	if !filter.Before.IsZero() {
		q += " AND time < " + arg(filter.Before)
	}
	q += " ORDER BY time DESC LIMIT " + arg(limit)

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not query events").WithOp(op)
	}
	defer rows.Close()

	records := []coreeventstore.Record{}
	for rows.Next() {
		var rec coreeventstore.Record
		if err := rows.Scan(&rec.ID, &rec.Time, &rec.NodeID, &rec.Topic,
			&rec.Source, &rec.Subject, &rec.Payload, &rec.ReceivedAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read an event").WithOp(op)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not query events").WithOp(op)
	}
	return records, nil
}

// receiver adapts a [coreeventstore.Store] to [transport.Receiver] for the
// fleet ingest path: an agent-forwarded event becomes a durable record
// attributed to the authenticated node, not to whatever node id the payload
// happened to claim.
//
// Store rather than *Repository so the caller may pass a [coreeventstore.Tap]
// wrapper — the alert rule engine's event hook needs to see fleet-forwarded
// events too, not just locally produced ones.
type receiver struct{ store coreeventstore.Store }

// NewReceiver builds the [transport.Receiver] for KindEvents.
func NewReceiver(store coreeventstore.Store) transport.Receiver { return receiver{store: store} }

func (r receiver) Kind() transport.Kind { return transport.KindEvents }

func (r receiver) Receive(ctx context.Context, env transport.Envelope) error {
	const op = "eventstore.receiver.Receive"

	event, ok := transport.EventsOf(env)
	if !ok {
		return errs.New(errs.CodeInternal, "events receiver given a non-events envelope").WithOp(op)
	}

	payload, err := marshalPayload(event.Payload)
	if err != nil {
		payload = []byte("{}")
	}

	return r.store.Insert(ctx, coreeventstore.Record{
		ID: event.ID, Time: event.Time, NodeID: env.Origin.NodeID, Topic: string(event.Topic),
		Source: event.Source, Subject: event.Subject, Payload: payload, ReceivedAt: time.Now(),
	})
}

func marshalPayload(payload any) ([]byte, error) {
	if payload == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(payload)
}
