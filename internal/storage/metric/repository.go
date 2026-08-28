package metric

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository reads and writes metric storage.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository builds a repository over an started pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// InsertBatch writes a batch of samples and refreshes the node's liveness.
//
// Samples go in via COPY rather than multi-row INSERT. At a fifteen-second
// cadence across a handful of collectors this is already thousands of rows a
// minute per node, and COPY is roughly an order of magnitude cheaper than
// INSERT for that shape — it skips per-row parse and plan entirely.
//
// The node upsert and the sample copy share one transaction, so a node row
// always exists by the time its samples are visible. Readers therefore never
// see a sample whose node they cannot resolve.
func (r *Repository) InsertBatch(ctx context.Context, env transport.Envelope, batch collect.Batch) error {
	const op = "metric.Repository.InsertBatch"

	if len(batch.Samples) == 0 {
		// Still touch the node: a collector that legitimately produced no
		// samples is evidence the node is alive, and treating it as silence
		// would eventually report a healthy node as down.
		return r.touchNode(ctx, env)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not begin the ingest transaction").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if env.ID != "" {
		const dedupSQL = `INSERT INTO ingested_envelopes (envelope_id, node_id, kind) VALUES ($1, $2, $3) ON CONFLICT (envelope_id) DO NOTHING`
		tag, err := tx.Exec(ctx, dedupSQL, env.ID, env.Origin.NodeID, string(env.Kind()))
		if err != nil {
			return errs.Wrap(err, errs.CodeUnavailable, "could not record envelope idempotency").WithOp(op)
		}
		if tag.RowsAffected() == 0 {
			// Already applied — most likely a transport retry after a
			// timeout on a write that actually succeeded. Commit nothing.
			return nil
		}
	}

	if err := upsertNode(ctx, tx, env); err != nil {
		return err
	}

	rows := make([][]any, 0, len(batch.Samples))
	for _, s := range batch.Samples {
		labels, err := marshalLabels(s.Labels)
		if err != nil {
			return errs.Wrap(err, errs.CodeInvalidArgument,
				"sample %q has labels that cannot be stored", s.Metric).
				WithOp(op).WithDetail("metric", s.Metric)
		}
		rows = append(rows, []any{
			s.Time, env.Origin.NodeID, batch.CollectorID,
			s.Metric, s.Value, string(s.Unit), string(s.Kind), labels,
		})
	}

	copied, err := tx.CopyFrom(ctx,
		pgx.Identifier{"metric_samples"},
		[]string{"time", "node_id", "collector_id", "metric", "value", "unit", "kind", "labels"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not write metric samples").
			WithOp(op).WithDetail("collector_id", batch.CollectorID)
	}
	if copied != int64(len(rows)) {
		return errs.New(errs.CodeInternal,
			"wrote %d of %d samples", copied, len(rows)).
			WithOp(op).WithDetail("collector_id", batch.CollectorID)
	}

	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not commit metric samples").WithOp(op)
	}
	return nil
}

// UpdateClockSkew records the most recently observed clock skew for a node.
func (r *Repository) UpdateClockSkew(ctx context.Context, nodeID string, skewSeconds float64) error {
	const op = "metric.Repository.UpdateClockSkew"

	_, err := r.pool.Exec(ctx, `UPDATE nodes SET clock_skew_seconds = $2 WHERE node_id = $1`, nodeID, skewSeconds)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record clock skew").
			WithOp(op).WithDetail("node_id", nodeID)
	}
	return nil
}

// upsertNodeSQL creates the node on first sight and refreshes its liveness
// afterwards.
//
// Only hostname and last_seen_at are updated here. The richer facts — OS,
// kernel, cores, boot time — come from the host collector via UpdateNodeFacts,
// and using COALESCE below means an ingest from a collector that does not know
// them cannot erase what the host collector established.
const upsertNodeSQL = `
	INSERT INTO nodes (node_id, hostname, agent_version, environment, first_seen_at, last_seen_at)
	VALUES ($1, $2, $3, NULLIF($4, ''), now(), now())
	ON CONFLICT (node_id) DO UPDATE SET
		hostname      = COALESCE(NULLIF(EXCLUDED.hostname, ''), nodes.hostname),
		agent_version = COALESCE(NULLIF(EXCLUDED.agent_version, ''), nodes.agent_version),
		environment   = COALESCE(EXCLUDED.environment, nodes.environment),
		last_seen_at  = now()`

func upsertNode(ctx context.Context, tx pgx.Tx, env transport.Envelope) error {
	_, err := tx.Exec(ctx, upsertNodeSQL,
		env.Origin.NodeID, env.Origin.Hostname, env.Origin.AgentVersion, env.Origin.Environment)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the node").
			WithOp("metric.upsertNode").WithDetail("node_id", env.Origin.NodeID)
	}
	return nil
}

func (r *Repository) touchNode(ctx context.Context, env transport.Envelope) error {
	_, err := r.pool.Exec(ctx, upsertNodeSQL,
		env.Origin.NodeID, env.Origin.Hostname, env.Origin.AgentVersion, env.Origin.Environment)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the node").
			WithOp("metric.Repository.touchNode")
	}
	return nil
}

// NodeFacts are the descriptive properties of a machine, refreshed by the host
// collector.
type NodeFacts struct {
	OS           string
	Platform     string
	Kernel       string
	Architecture string
	CPUCores     int
	BootTime     time.Time
}

// UpdateNodeFacts records the descriptive properties of a node.
//
// Separate from sample ingest because these change rarely — a kernel upgrade,
// a resize — and writing them on every batch would be pure waste at a
// fifteen-second cadence.
func (r *Repository) UpdateNodeFacts(ctx context.Context, nodeID string, facts NodeFacts) error {
	const op = "metric.Repository.UpdateNodeFacts"

	const q = `
		UPDATE nodes SET
			os           = COALESCE(NULLIF($2, ''), os),
			platform     = COALESCE(NULLIF($3, ''), platform),
			kernel       = COALESCE(NULLIF($4, ''), kernel),
			architecture = COALESCE(NULLIF($5, ''), architecture),
			cpu_cores    = COALESCE(NULLIF($6, 0), cpu_cores),
			boot_time    = COALESCE($7, boot_time)
		WHERE node_id = $1`

	var bootTime *time.Time
	if !facts.BootTime.IsZero() {
		bootTime = &facts.BootTime
	}

	_, err := r.pool.Exec(ctx, q, nodeID,
		facts.OS, facts.Platform, facts.Kernel, facts.Architecture, facts.CPUCores, bootTime)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not update node facts").
			WithOp(op).WithDetail("node_id", nodeID)
	}
	return nil
}

// EnsureNode creates the node row if the first metric batch for it has not
// landed yet, and refreshes its liveness otherwise. Callers promoting
// inventory facts (host facts, interface addressing) for a remote agent use
// this first, so an inventory push that races ahead of the node's metrics
// does not silently write nothing — UpdateNodeFacts and ReplaceNodeAddresses
// both require the row to exist.
func (r *Repository) EnsureNode(ctx context.Context, env transport.Envelope) error {
	return r.touchNode(ctx, env)
}

// UpdatePublicIP records the source address the control plane observed a
// node's connection arriving from. Server-observed, never agent-reported;
// overwritten on each observation, like last_seen_at. A blank ip is a no-op.
//
// This is an UPDATE, not an upsert: on the HTTPS enrollment path the node row
// does not exist until the agent's first telemetry batch, so an enrollment
// that precedes any telemetry records nothing here — an accepted limitation,
// since HTTPS refreshes this only at enrollment, not continuously.
func (r *Repository) UpdatePublicIP(ctx context.Context, nodeID, ip string) error {
	const op = "metric.Repository.UpdatePublicIP"

	if ip == "" {
		return nil
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE nodes SET public_ip = $2::inet WHERE node_id = $1`, nodeID, ip)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the observed public ip").
			WithOp(op).WithDetail("node_id", nodeID)
	}
	return nil
}

// ReplaceNodeAddresses replaces a node's entire set of per-interface
// addresses in one transaction: every existing row for the node is deleted
// and the supplied set inserted. Replace-wholesale rather than upsert so an
// interface that has gone away on the host leaves no stale row behind,
// matching inventory_snapshots' own replace-on-arrival convention for this
// same data.
func (r *Repository) ReplaceNodeAddresses(ctx context.Context, nodeID string, observedAt time.Time, addrs []NodeAddress) error {
	const op = "metric.Repository.ReplaceNodeAddresses"

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not begin the address update").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM node_addresses WHERE node_id = $1`, nodeID); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not clear the node's addresses").
			WithOp(op).WithDetail("node_id", nodeID)
	}

	const ins = `
		INSERT INTO node_addresses (node_id, interface, address, observed_at)
		VALUES ($1, $2, $3::inet, $4)
		ON CONFLICT (node_id, interface, address) DO UPDATE SET observed_at = EXCLUDED.observed_at`
	for _, a := range addrs {
		if a.Interface == "" || a.Address == "" {
			continue
		}
		if _, err := tx.Exec(ctx, ins, nodeID, a.Interface, a.Address, observedAt); err != nil {
			return errs.Wrap(err, errs.CodeUnavailable, "could not record a node address").
				WithOp(op).WithDetail("node_id", nodeID).WithDetail("address", a.Address)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not commit the node addresses").WithOp(op)
	}
	return nil
}

// ListNodeAddresses returns a node's per-interface addresses, ordered for a
// stable presentation.
func (r *Repository) ListNodeAddresses(ctx context.Context, nodeID string) ([]NodeAddress, error) {
	const op = "metric.Repository.ListNodeAddresses"

	const q = `
		SELECT interface, address::text, observed_at
		FROM node_addresses
		WHERE node_id = $1
		ORDER BY interface, address`

	rows, err := r.pool.Query(ctx, q, nodeID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list node addresses").WithOp(op)
	}
	defer rows.Close()

	out := []NodeAddress{}
	for rows.Next() {
		var a NodeAddress
		if err := rows.Scan(&a.Interface, &a.Address, &a.ObservedAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a node address").WithOp(op)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list node addresses").WithOp(op)
	}
	return out, nil
}

// ListNodes returns every known node, most recently seen first.
func (r *Repository) ListNodes(ctx context.Context) ([]Node, error) {
	const op = "metric.Repository.ListNodes"

	const q = `
		SELECT node_id, hostname,
		       COALESCE(os, ''), COALESCE(platform, ''), COALESCE(kernel, ''),
		       COALESCE(architecture, ''), COALESCE(cpu_cores, 0),
		       COALESCE(agent_version, ''), COALESCE(environment, ''), boot_time, first_seen_at, last_seen_at,
		       clock_skew_seconds, COALESCE(host(public_ip), '')
		FROM nodes
		ORDER BY last_seen_at DESC, node_id`

	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list nodes").WithOp(op)
	}
	defer rows.Close()

	nodes := []Node{}
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a node").WithOp(op)
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list nodes").WithOp(op)
	}
	return nodes, nil
}

// GetNode returns one node by id.
func (r *Repository) GetNode(ctx context.Context, nodeID string) (Node, error) {
	const op = "metric.Repository.GetNode"

	const q = `
		SELECT node_id, hostname,
		       COALESCE(os, ''), COALESCE(platform, ''), COALESCE(kernel, ''),
		       COALESCE(architecture, ''), COALESCE(cpu_cores, 0),
		       COALESCE(agent_version, ''), COALESCE(environment, ''), boot_time, first_seen_at, last_seen_at,
		       clock_skew_seconds, COALESCE(host(public_ip), '')
		FROM nodes WHERE node_id = $1`

	rows, err := r.pool.Query(ctx, q, nodeID)
	if err != nil {
		return Node{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the node").WithOp(op)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Node{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the node").WithOp(op)
		}
		return Node{}, errs.New(errs.CodeNotFound, "node not found").
			WithOp(op).WithDetail("node_id", nodeID)
	}

	node, err := scanNode(rows)
	if err != nil {
		return Node{}, errs.Wrap(err, errs.CodeInternal, "could not read the node").WithOp(op)
	}
	return node, nil
}

func scanNode(rows pgx.Rows) (Node, error) {
	var n Node
	var bootTime *time.Time

	err := rows.Scan(&n.NodeID, &n.Hostname, &n.OS, &n.Platform, &n.Kernel,
		&n.Architecture, &n.CPUCores, &n.AgentVersion, &n.Environment,
		&bootTime, &n.FirstSeenAt, &n.LastSeenAt, &n.ClockSkewSeconds, &n.PublicIP)
	if err != nil {
		return Node{}, err
	}
	if bootTime != nil {
		n.BootTime = *bootTime
	}
	return n, nil
}

// ListMetricNames returns the distinct metrics a node has reported recently.
//
// Scoped to a recent window rather than all of history: an unbounded DISTINCT
// over a hypertable is expensive, and a metric no collector has produced in a
// day is not something a caller wants offered as a current choice.
func (r *Repository) ListMetricNames(ctx context.Context, nodeID string, since time.Duration) ([]string, error) {
	const op = "metric.Repository.ListMetricNames"

	if since <= 0 {
		since = 24 * time.Hour
	}

	const q = `
		SELECT DISTINCT metric FROM metric_samples
		WHERE node_id = $1 AND time > now() - $2::interval
		ORDER BY metric`

	rows, err := r.pool.Query(ctx, q, nodeID, since)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list metrics").WithOp(op)
	}
	defer rows.Close()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a metric name").WithOp(op)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list metrics").WithOp(op)
	}
	return names, nil
}

func marshalLabels(labels map[string]string) ([]byte, error) {
	if len(labels) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(labels)
}

func unmarshalLabels(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var labels map[string]string
	if err := json.Unmarshal(raw, &labels); err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	if len(labels) == 0 {
		return nil, nil
	}
	return labels, nil
}

// unitAndKind converts stored text back into the collect domain types.
func unitAndKind(unit, kind string) (collect.Unit, collect.Kind) {
	return collect.Unit(unit), collect.Kind(kind)
}
