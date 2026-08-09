package v1

import (
	"net/http"
	"strconv"
	"time"

	"github.com/hexane/atlas/internal/core/activity"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// Activity feed limits.
const (
	defaultActivityLimit = 25
	maxActivityLimit     = 200
)

// ActivityEntryResponse is one recorded occurrence.
type ActivityEntryResponse struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Topic    string    `json:"topic"`
	Severity string    `json:"severity"`
	Title    string    `json:"title"`
	Detail   string    `json:"detail,omitempty"`
	Source   string    `json:"source,omitempty"`
}

// ListActivityResponse is the recent-activity feed.
type ListActivityResponse struct {
	Entries []ActivityEntryResponse `json:"entries"`
	Total   int                     `json:"total"`
	// Since is when this Atlas instance began recording. The feed is held in
	// memory, so it starts empty after a restart — a client that does not
	// show this cannot distinguish "nothing has happened" from "nothing has
	// happened yet".
	Since time.Time `json:"since"`
	// Dropped counts events the bus discarded because the recorder could not
	// keep up. Non-zero means the feed has gaps, and saying so is better than
	// presenting an incomplete list as complete.
	Dropped uint64 `json:"dropped"`
}

// ListActivity returns the most recent notable events, newest first.
func (h *Handler) ListActivity(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListActivity"

	if h.deps.Activity == nil {
		return errs.New(errs.CodeNotImplemented, "the activity feed is not enabled").WithOp(op)
	}

	limit := defaultActivityLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return errs.New(errs.CodeInvalidArgument, "limit must be a positive number").
				WithOp(op).WithDetail("field", "limit")
		}
		limit = min(n, maxActivityLimit)
	}

	entries := h.deps.Activity.Recent(limit)
	out := make([]ActivityEntryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, ActivityEntryResponse{
			ID: e.ID, Time: e.Time, Topic: string(e.Topic),
			Severity: string(e.Severity), Title: e.Title, Detail: e.Detail, Source: e.Source,
		})
	}

	httpx.JSON(w, r, http.StatusOK, ListActivityResponse{
		Entries: out,
		Total:   len(out),
		Since:   h.deps.Activity.Since(),
		Dropped: h.deps.Activity.Dropped(),
	})
	return nil
}

var _ = activity.Entry{}
