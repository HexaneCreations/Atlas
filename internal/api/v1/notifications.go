package v1

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// NotificationChannelRequest is the body of a create or update request. A
// webhook secret is write-only: it is accepted here but never present on
// [NotificationChannelResponse] — see that type's doc.
type NotificationChannelRequest struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	Enabled       *bool  `json:"enabled,omitempty"`
	WebhookURL    string `json:"webhook_url,omitempty"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
}

func (req NotificationChannelRequest) toChannel(id string) notification.Channel {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return notification.Channel{
		ID: id, Name: req.Name, Type: notification.ChannelType(req.Type), Enabled: enabled,
		Webhook: notification.WebhookConfig{URL: req.WebhookURL, Secret: req.WebhookSecret},
	}
}

// NotificationChannelResponse is one notification channel.
//
// It deliberately has no secret field. A webhook signing secret is
// write-only, the same convention enrollment tokens use (see
// [github.com/hexane/atlas/internal/core/fleet.GeneratedToken]): accepted
// on create/update, never echoed back. See
// [TestNotificationChannelResponseHasNoSecretField].
type NotificationChannelResponse struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Enabled    bool      `json:"enabled"`
	WebhookURL string    `json:"webhook_url,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func presentChannel(c notification.Channel) NotificationChannelResponse {
	return NotificationChannelResponse{
		ID: c.ID, Name: c.Name, Type: string(c.Type), Enabled: c.Enabled,
		WebhookURL: c.Webhook.URL, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// ListNotificationChannelsResponse is every configured channel.
type ListNotificationChannelsResponse struct {
	Channels []NotificationChannelResponse `json:"channels"`
	Total    int                           `json:"total"`
}

func (h *Handler) notifications(op string) (NotificationStore, error) {
	if h.deps.NotificationStore == nil {
		return nil, errs.New(errs.CodeUnavailable, "notifications are not configured").WithOp(op)
	}
	return h.deps.NotificationStore, nil
}

// ListNotificationChannels returns every configured channel.
func (h *Handler) ListNotificationChannels(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListNotificationChannels"
	store, err := h.notifications(op)
	if err != nil {
		return err
	}

	channels, err := store.ListChannels(r.Context())
	if err != nil {
		return err
	}
	out := make([]NotificationChannelResponse, 0, len(channels))
	for _, c := range channels {
		out = append(out, presentChannel(c))
	}
	httpx.JSON(w, r, http.StatusOK, ListNotificationChannelsResponse{Channels: out, Total: len(out)})
	return nil
}

// GetNotificationChannel returns one channel by id.
func (h *Handler) GetNotificationChannel(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.GetNotificationChannel"
	store, err := h.notifications(op)
	if err != nil {
		return err
	}

	c, err := store.GetChannel(r.Context(), r.PathValue("channelID"))
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, presentChannel(c))
	return nil
}

// CreateNotificationChannel defines a new channel.
func (h *Handler) CreateNotificationChannel(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.CreateNotificationChannel"
	store, err := h.notifications(op)
	if err != nil {
		return err
	}

	var req NotificationChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	channel := req.toChannel("")
	if err := channel.Validate(); err != nil {
		return err
	}

	created, err := store.CreateChannel(r.Context(), channel)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusCreated, presentChannel(created))
	return nil
}

// UpdateNotificationChannel replaces an existing channel's definition.
func (h *Handler) UpdateNotificationChannel(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.UpdateNotificationChannel"
	store, err := h.notifications(op)
	if err != nil {
		return err
	}

	var req NotificationChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	channel := req.toChannel(r.PathValue("channelID"))
	if err := channel.Validate(); err != nil {
		return err
	}

	updated, err := store.UpdateChannel(r.Context(), channel)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, presentChannel(updated))
	return nil
}

// DeleteNotificationChannel removes a channel. Its delivery history is
// removed with it — see the migration's ON DELETE CASCADE.
func (h *Handler) DeleteNotificationChannel(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.DeleteNotificationChannel"
	store, err := h.notifications(op)
	if err != nil {
		return err
	}

	if err := store.DeleteChannel(r.Context(), r.PathValue("channelID")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// NotificationDeliveryResponse is one delivery attempt's status — the
// observability surface for whether a notification actually went out.
type NotificationDeliveryResponse struct {
	ID            string    `json:"id"`
	EventID       string    `json:"event_id"`
	ChannelID     string    `json:"channel_id"`
	Trigger       string    `json:"trigger"`
	NodeID        string    `json:"node_id,omitempty"`
	Severity      string    `json:"severity,omitempty"`
	Title         string    `json:"title,omitempty"`
	Message       string    `json:"message,omitempty"`
	EventTime     time.Time `json:"event_time"`
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
	LastError     string    `json:"last_error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ListNotificationDeliveriesResponse is a page of the delivery log.
type ListNotificationDeliveriesResponse struct {
	Deliveries []NotificationDeliveryResponse `json:"deliveries"`
	Total      int                            `json:"total"`
}

// ListNotificationDeliveries returns recent delivery attempts, newest
// first, optionally filtered by channel or status.
func (h *Handler) ListNotificationDeliveries(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListNotificationDeliveries"
	store, err := h.notifications(op)
	if err != nil {
		return err
	}

	q := r.URL.Query()
	filter := notification.DeliveryFilter{
		ChannelID: q.Get("channel_id"),
		Status:    notification.Status(q.Get("status")),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return errs.New(errs.CodeInvalidArgument, "limit must be a positive number").WithOp(op).WithDetail("field", "limit")
		}
		filter.Limit = n
	}

	deliveries, err := store.ListDeliveries(r.Context(), filter)
	if err != nil {
		return err
	}
	out := make([]NotificationDeliveryResponse, 0, len(deliveries))
	for _, d := range deliveries {
		out = append(out, NotificationDeliveryResponse{
			ID: d.ID, EventID: d.EventID, ChannelID: d.ChannelID, Trigger: string(d.Trigger),
			NodeID: d.NodeID, Severity: d.Severity, Title: d.Title, Message: d.Message, EventTime: d.EventTime,
			Status: string(d.Status), Attempts: d.Attempts, NextAttemptAt: d.NextAttemptAt, LastError: d.LastError,
			CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
		})
	}
	httpx.JSON(w, r, http.StatusOK, ListNotificationDeliveriesResponse{Deliveries: out, Total: len(out)})
	return nil
}
