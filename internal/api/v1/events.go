package v1

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/core/eventstore"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

const (
	defaultEventsLimit = 50
	maxEventsLimit     = 500
)

// EventResponse is one durably stored event.
type EventResponse struct {
	ID         string          `json:"id"`
	Time       time.Time       `json:"time"`
	NodeID     string          `json:"node_id"`
	Topic      string          `json:"topic"`
	Source     string          `json:"source,omitempty"`
	Subject    string          `json:"subject,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	ReceivedAt time.Time       `json:"received_at"`
}

// ListEventsResponse is a page of the durable event log.
type ListEventsResponse struct {
	Events []EventResponse `json:"events"`
	Total  int             `json:"total"`
}

// ListEvents returns durable events, newest first, filtered by node, topic,
// and time range.
func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListEvents"

	if h.deps.EventStore == nil {
		return errs.New(errs.CodeNotImplemented, "the event store is not enabled").WithOp(op)
	}

	q := r.URL.Query()
	filter := eventstore.Filter{
		NodeID: strings.TrimSpace(q.Get("node")),
		Topic:  strings.TrimSpace(q.Get("topic")),
		Limit:  defaultEventsLimit,
	}

	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return errs.New(errs.CodeInvalidArgument, "limit must be a positive number").
				WithOp(op).WithDetail("field", "limit")
		}
		filter.Limit = min(n, maxEventsLimit)
	}
	for field, dst := range map[string]*time.Time{"since": &filter.Since, "until": &filter.Until, "before": &filter.Before} {
		raw := q.Get(field)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return errs.New(errs.CodeInvalidArgument, "%s must be RFC3339", field).
				WithOp(op).WithDetail("field", field)
		}
		*dst = t
	}

	records, err := h.deps.EventStore.Query(r.Context(), filter)
	if err != nil {
		return err
	}

	out := make([]EventResponse, 0, len(records))
	for _, rec := range records {
		out = append(out, EventResponse{
			ID: rec.ID, Time: rec.Time, NodeID: rec.NodeID, Topic: rec.Topic,
			Source: rec.Source, Subject: rec.Subject, Payload: rec.Payload, ReceivedAt: rec.ReceivedAt,
		})
	}

	httpx.JSON(w, r, http.StatusOK, ListEventsResponse{Events: out, Total: len(out)})
	return nil
}
