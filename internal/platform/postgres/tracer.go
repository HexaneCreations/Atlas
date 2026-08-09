package postgres

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// slowQueryThreshold is the duration above which a query is logged at warn
// level. Atlas's queries are metric reads and bulk inserts; anything past a
// quarter second is worth an operator's attention.
const slowQueryThreshold = 250 * time.Millisecond

// queryTracer records query timing through pgx's tracing hook.
//
// This is Atlas applying its own principle to itself: the platform that
// explains other systems' latency must be able to explain its own. Because it
// runs on every query, it does the cheapest possible thing on the hot path —
// stash a start time — and formats only when a query is slow or fails, or
// when debug logging is on.
//
// SQL text is logged; parameter values are not. Arguments to Atlas's queries
// include host names, container ids, and eventually credentials read from the
// catalog, and the query text alone is enough to identify what ran.
type queryTracer struct {
	logger    *slog.Logger
	slowQuery time.Duration
}

type traceKey struct{}

type traceData struct {
	start time.Time
	sql   string
}

// TraceQueryStart implements pgx.QueryTracer.
func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceKey{}, &traceData{start: time.Now(), sql: data.SQL})
}

// TraceQueryEnd implements pgx.QueryTracer.
func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	td, ok := ctx.Value(traceKey{}).(*traceData)
	if !ok {
		return
	}
	elapsed := time.Since(td.start)

	// The platform logger merges context attributes on every record, so
	// passing ctx here is what correlates a slow query with the request that
	// issued it — no request-scoped logger needs to be threaded down.
	logger := t.logger

	attrs := []any{
		slog.String("sql", td.sql),
		slog.Duration("duration", elapsed),
	}

	switch {
	case data.Err != nil:
		// Cancellation is the normal result of a client disconnecting mid
		// request; it is not a database problem.
		if ctx.Err() != nil {
			logger.DebugContext(ctx, "query cancelled", attrs...)
			return
		}
		logger.ErrorContext(ctx, "query failed", append(attrs, slog.Any("error", data.Err))...)
	case elapsed >= t.slowQuery:
		logger.WarnContext(ctx, "slow query", append(attrs, slog.String("command_tag", data.CommandTag.String()))...)
	default:
		logger.DebugContext(ctx, "query", attrs...)
	}
}

var _ pgx.QueryTracer = (*queryTracer)(nil)
