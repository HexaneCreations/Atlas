package v1

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/hexane/atlas/internal/api/session"
	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/plugin/docker"
)

// clockSkew is how far in the future an until may be and still mean now.
const clockSkew = time.Minute

// logRangeConfig is the configured range limits, with the defaults for a
// configuration that predates them.
func (h *Handler) logRangeConfig() config.ContainerLogs {
	if h.deps.Config != nil && h.deps.Config.ContainerLogs.MaxRange > 0 {
		return h.deps.Config.ContainerLogs
	}
	return config.Default().ContainerLogs
}

func badLogParam(field, format string, args ...any) error {
	return errs.New(errs.CodeInvalidArgument, format, args...).WithOp("v1.parseLogRange").WithDetail("field", field)
}

func parseLogTime(q url.Values, field string) (time.Time, bool, error) {
	raw := q.Get(field)
	if raw == "" {
		if q.Has(field) {
			return time.Time{}, false, badLogParam(field, "%s must be an RFC 3339 timestamp", field)
		}
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil || len(raw) > 40 {
		return time.Time{}, false, badLogParam(field, "%s must be an RFC 3339 timestamp", field)
	}
	return t.UTC(), true, nil
}

// parseLogRange validates a ranged read's parameters: since (required),
// until (absent means now), before (the cursor), limit.
func parseLogRange(q url.Values, maxRange time.Duration, now time.Time) (rangeQuery, error) {
	if q.Has("tail") {
		return rangeQuery{}, badLogParam("tail", "tail cannot be combined with until, before or limit; use limit")
	}
	from, ok, err := parseLogTime(q, "since")
	if err != nil {
		return rangeQuery{}, err
	}
	if !ok {
		return rangeQuery{}, badLogParam("since", "since is required for a time range")
	}
	until, hasUntil, err := parseLogTime(q, "until")
	if err != nil {
		return rangeQuery{}, err
	}
	before, hasBefore, err := parseLogTime(q, "before")
	if err != nil {
		return rangeQuery{}, err
	}

	limit := defaultLogLimit
	if raw := q.Get("limit"); raw != "" || q.Has("limit") {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 || n > maxLogLimit {
			return rangeQuery{}, badLogParam("limit", "limit must be between 1 and %d", maxLogLimit)
		}
		limit = n
	}

	if from.After(now) {
		return rangeQuery{}, badLogParam("since", "since is in the future")
	}
	end := now
	if hasUntil {
		if until.After(now.Add(clockSkew)) {
			return rangeQuery{}, badLogParam("until", "until is in the future; leave it out for a range that ends now")
		}
		if !until.After(from) {
			return rangeQuery{}, badLogParam("until", "since must be before until")
		}
		end = minTime(until, now)
	}
	if end.Sub(from) > maxRange {
		return rangeQuery{}, badLogParam("since", "a range may span at most %s", maxRange)
	}
	out := rangeQuery{From: from, Limit: limit}
	if hasUntil {
		out.Upper, out.Until = end, end
	}
	if hasBefore {
		if !before.After(from) || before.After(end) {
			return rangeQuery{}, badLogParam("before", "before must lie inside the range")
		}
		out.Upper = before.Add(-time.Nanosecond)
	}
	return out, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// logRangeSource is containerLogSource for a ranged read.
func (h *Handler) logRangeSource(scope inventory.Scope, containerID string) (*rangeSource, error) {
	const op = "v1.Handler.logRangeSource"
	if h.deps.Collection != nil && scope.IsLocal(h.deps.Collection.Identity().NodeID) {
		client, err := h.dockerClient(scope)
		if err != nil {
			return nil, err
		}
		return &rangeSource{direct: true, read: func(ctx context.Context, opts docker.LogOptions) (<-chan docker.LogLine, <-chan error) {
			return client.Logs(ctx, containerID, opts)
		}}, nil
	}
	if h.deps.RemoteLogs == nil {
		return nil, errs.New(errs.CodeUnavailable, "remote container logs are not available on this control plane").WithOp(op)
	}
	remote, nodeID := h.deps.RemoteLogs, scope.NodeID
	return &rangeSource{direct: remote.SupportsUntil(nodeID), read: func(ctx context.Context, opts docker.LogOptions) (<-chan docker.LogLine, <-chan error) {
		return remote.ContainerLogs(ctx, nodeID, containerID, opts)
	}}, nil
}

// containerLogsRange serves GET /containers/{id}/logs with since, until,
// before or limit: the newest limit lines of the range (or of the part
// older than before), oldest first.
//
// A range that ends now is read as a tail, which every source serves. A
// bounded one is read in windows searched back from its end, and holds one
// of the per-node and per-user scan slots while it reads. Nothing about the
// lines themselves is logged.
func (h *Handler) containerLogsRange(w http.ResponseWriter, r *http.Request, scope inventory.Scope, containerID string) error {
	const op = "v1.Handler.containerLogsRange"
	cfg := h.logRangeConfig()
	now := time.Now().UTC()
	q, err := parseLogRange(r.URL.Query(), cfg.MaxRange, now)
	if err != nil {
		return err
	}
	src, err := h.logRangeSource(scope, containerID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(r.Context(), cfg.ScanTimeout)
	defer cancel()
	if !q.Upper.IsZero() {
		principal, _ := session.PrincipalFrom(r.Context())
		release, ok := h.logScans.acquire(h.resolvedNode(scope), principal.UserID)
		if !ok {
			w.Header().Set("Retry-After", "2")
			return errs.New(errs.CodeRateLimited, "too many log time-range reads at once; try again shortly").WithOp(op)
		}
		defer release()
	}

	page, err := readRange(ctx, src, q, newScanBudget(cfg, now))
	if err != nil {
		return err
	}
	out := ContainerLogsResponse{
		ContainerID: containerID, Lines: make([]LogLineResponse, 0, len(page.Lines)), Total: len(page.Lines),
		Since: &q.From, More: page.More, NextBefore: page.NextBefore, Truncated: page.Truncated,
		ScanLimited: page.ScanLimited, AvailableSince: page.AvailableSince, Fallback: page.Fallback,
	}
	if !q.Until.IsZero() {
		out.Until = &q.Until
	}
	for _, line := range page.Lines {
		resp := LogLineResponse{Time: line.Time, Stream: line.Stream, Message: line.Message}
		if len(resp.Message) > maxLogLineBytes {
			resp.Message, resp.Truncated = cutUTF8(resp.Message, maxLogLineBytes), true
		}
		out.Lines = append(out.Lines, resp)
	}
	httpx.JSON(w, r, http.StatusOK, out)
	return nil
}

// cutUTF8 is s's first n bytes or fewer, never splitting a UTF-8 sequence.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
