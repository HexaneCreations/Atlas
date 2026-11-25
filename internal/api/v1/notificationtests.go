package v1

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/hexane/atlas/internal/api/session"
	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/platform/log"
)

// testSendTimeout bounds a whole test, a little above the senders' own
// timeout so that one is what normally ends it.
const testSendTimeout = notification.DefaultSendTimeout + 5*time.Second

// NotificationTester sends test notifications. Satisfied by
// [*notification.Engine].
type NotificationTester interface {
	SendTest(ctx context.Context, channelID string) (notification.SendTestOutcome, error)
	PrepareDraftTest(ctx context.Context, in notification.ChannelInput, channelID string) (*notification.DraftTest, error)
}

// TestNotificationDraftRequest is a draft channel to test: a create request,
// plus the saved channel whose stored secret to use when secret is empty.
type TestNotificationDraftRequest struct {
	NotificationChannelRequest
	ChannelID string `json:"channel_id,omitempty"`
}

// TestNotificationResponse is a test's outcome. Error is a fixed category
// message, empty when OK.
type TestNotificationResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// logAdminAction records an administrative change the way the install-code
// endpoints do: a structured log line naming the actor. There is no general
// audit table; user_audit_log covers user management only.
func logAdminAction(r *http.Request, msg string, attrs ...slog.Attr) {
	principal, _ := session.PrincipalFrom(r.Context())
	attrs = append(attrs, slog.String("actor_user_id", principal.UserID))
	log.FromContext(r.Context()).LogAttrs(r.Context(), slog.LevelInfo, msg, attrs...)
}

// notificationTester authorizes the caller and returns the tester and the
// store, both present only when notifications are configured.
func (h *Handler) notificationTester(r *http.Request, op string) (NotificationTester, NotificationStore, error) {
	if err := h.requirePermission(r, user.PermissionNotificationsManage, pageauthz.PageNone); err != nil {
		return nil, nil, err
	}
	store, err := h.notifications(op)
	if err != nil {
		return nil, nil, err
	}
	if h.deps.NotificationTester == nil {
		return nil, nil, errs.New(errs.CodeUnavailable, "notifications are disabled: no encryption key is configured").WithOp(op)
	}
	return h.deps.NotificationTester, store, nil
}

func (h *Handler) allowTest(w http.ResponseWriter, r *http.Request, target, op string) error {
	principal, _ := session.PrincipalFrom(r.Context())
	retryAfter, ok := h.deps.NotificationTestLimiter.Allow(principal.UserID, target)
	if ok {
		return nil
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	return errs.New(errs.CodeRateLimited, "too many test notifications; try again shortly").WithOp(op)
}

// testContext outlives the request: a client that goes away mid-send must
// not leave the test's delivery row unfinished for the dispatcher to retry.
func testContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), testSendTimeout)
}

// TestNotificationChannel sends a test through a saved channel. It is
// recorded in the delivery log like any other delivery.
func (h *Handler) TestNotificationChannel(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.TestNotificationChannel"
	tester, store, err := h.notificationTester(r, op)
	if err != nil {
		return err
	}
	// Looked up before the limiter, which must not keep a key per unknown id.
	channel, err := store.GetChannel(r.Context(), r.PathValue("channelID"))
	if err != nil {
		return err
	}
	if err := h.allowTest(w, r, "channel:"+channel.ID, op); err != nil {
		return err
	}

	ctx, cancel := testContext(r)
	defer cancel()
	outcome, err := tester.SendTest(ctx, channel.ID)
	if errors.Is(err, notification.ErrTestClaimed) {
		outcome, err = notification.SendTestOutcome{Error: notification.SendErrorCategory(err)}, nil
	}
	if err != nil {
		return err
	}
	logAdminAction(r, "notification channel tested", slog.String("channel_id", channel.ID),
		slog.String("channel_type", string(channel.Type)), slog.Bool("ok", outcome.Delivered))
	httpx.JSON(w, r, http.StatusOK, TestNotificationResponse{OK: outcome.Delivered, Error: outcome.Error})
	return nil
}

// TestNotificationDraft sends a test through a channel supplied in the body.
// Nothing is stored.
func (h *Handler) TestNotificationDraft(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.TestNotificationDraft"
	tester, _, err := h.notificationTester(r, op)
	if err != nil {
		return err
	}

	var req TestNotificationDraftRequest
	if err := decodeStrict(r, &req); err != nil {
		// Not the decoder's own text, which can quote the body and is logged.
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	draft, err := tester.PrepareDraftTest(r.Context(), req.toInput(), req.ChannelID)
	if err != nil {
		return err
	}
	if err := h.allowTest(w, r, draft.Target(), op); err != nil {
		return err
	}

	ctx, cancel := testContext(r)
	defer cancel()
	outcome := draft.Send(ctx)
	logAdminAction(r, "notification draft tested", slog.String("channel_id", draft.ChannelID),
		slog.String("channel_type", req.Type), slog.Bool("ok", outcome.Delivered))
	httpx.JSON(w, r, http.StatusOK, TestNotificationResponse{OK: outcome.Delivered, Error: outcome.Error})
	return nil
}
