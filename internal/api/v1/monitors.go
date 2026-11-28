package v1

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/api/session"
	"github.com/hexane/atlas/internal/core/monitor"
	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// MonitorStore stores website monitors. Satisfied by
// [*github.com/hexane/atlas/internal/storage/monitor.Repository].
type MonitorStore interface {
	ListMonitors(ctx context.Context) ([]monitor.View, error)
	GetMonitor(ctx context.Context, monitorID string) (monitor.View, error)
	// StoredMonitor returns the configuration and secret a test of a saved
	// monitor starts from. It must only read.
	StoredMonitor(ctx context.Context, monitorID string) (monitor.Monitor, monitor.Secret, error)
	CreateMonitor(ctx context.Context, m monitor.Monitor, s monitor.Secret, by string) (monitor.View, error)
	UpdateMonitor(ctx context.Context, monitorID string, apply monitor.UpdateFunc, by string) (monitor.View, error)
	SetMonitorActive(ctx context.Context, monitorID string, active bool, by string) (monitor.View, error)
	DeleteMonitor(ctx context.Context, monitorID string) error
	// SetMonitorChannels replaces the monitor's channel selection.
	SetMonitorChannels(ctx context.Context, monitorID string, channelIDs []string) ([]string, error)

	RecentHeartbeats(ctx context.Context, monitorIDs []string, n int) (map[string][]monitor.Heartbeat, error)
	UptimeSince(ctx context.Context, monitorIDs []string, since time.Time) (map[string]monitor.UptimeCount, error)
	Uptime(ctx context.Context, monitorID string, now time.Time) (monitor.Uptime, error)
	Heartbeats(ctx context.Context, monitorID string, since time.Time, limit int) ([]monitor.Heartbeat, error)
	HourlyHistory(ctx context.Context, monitorID string, since time.Time) ([]monitor.HourlyBucket, error)
	Events(ctx context.Context, monitorID string, before time.Time, limit int) ([]monitor.Event, error)

	ListGroups(ctx context.Context) ([]monitor.Group, error)
	CreateGroup(ctx context.Context, g monitor.Group) (monitor.Group, error)
	UpdateGroup(ctx context.Context, g monitor.Group) (monitor.Group, error)
	DeleteGroup(ctx context.Context, groupID string) error
}

// MonitorTester runs on-demand checks. Satisfied by [*monitor.Tester].
type MonitorTester interface {
	Test(ctx context.Context, m monitor.Monitor, s monitor.Secret) (monitor.Result, error)
}

// MonitorProbe reports the prober's own connectivity.
type MonitorProbe interface {
	ProbeState() monitor.ProbeState
}

// heartbeatBarLength is how many recent checks the list shows per monitor.
const heartbeatBarLength = 50

// --------------------------------------------------------------- shapes ----

// HeartbeatResponse is one check.
type HeartbeatResponse struct {
	Time       time.Time `json:"time"`
	Status     string    `json:"status"`
	StatusCode *int      `json:"status_code"`
	ResponseMS *int      `json:"response_ms"`
	Failure    string    `json:"failure"`
}

// MonitorLiveResponse is a monitor's current probe state.
type MonitorLiveResponse struct {
	Status              string     `json:"status"`
	StatusSince         time.Time  `json:"status_since"`
	DownSince           *time.Time `json:"down_since"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastCheckAt         *time.Time `json:"last_check_at"`
	LastStatusCode      *int       `json:"last_status_code"`
	LastResponseMS      *int       `json:"last_response_ms"`
	LastFailure         string     `json:"last_failure"`
	NextCheckAt         time.Time  `json:"next_check_at"`
	CertExpiresAt       *time.Time `json:"cert_expires_at"`
}

// MonitorResponse is a monitor as the API shows it. Redacted is true for
// readers without notifications.manage: the URL loses its query string and
// the authentication, header and audit fields are left out. No response
// ever carries a header value, credential or request body.
type MonitorResponse struct {
	ID                   string     `json:"id"`
	Name                 string     `json:"name"`
	Type                 string     `json:"type"`
	URL                  string     `json:"url"`
	Host                 string     `json:"host"`
	Method               string     `json:"method"`
	IntervalSeconds      int        `json:"interval_seconds"`
	Retries              int        `json:"retries"`
	RetryIntervalSeconds int        `json:"retry_interval_seconds"`
	TimeoutSeconds       int        `json:"timeout_seconds"`
	MaxRedirects         int        `json:"max_redirects"`
	AcceptedStatus       []string   `json:"accepted_status"`
	BodyEncoding         string     `json:"body_encoding"`
	Keyword              string     `json:"keyword"`
	RecoverySuccesses    int        `json:"recovery_successes"`
	CertNotify           bool       `json:"cert_notify"`
	CertWarnDays         int        `json:"cert_warn_days"`
	CertCritDays         int        `json:"cert_crit_days"`
	GroupID              string     `json:"group_id"`
	Description          string     `json:"description"`
	Tags                 []string   `json:"tags"`
	Active               bool       `json:"active"`
	AlertsMutedUntil     *time.Time `json:"alerts_muted_until"`
	AuthType             string     `json:"auth_type,omitempty"`
	HeaderNames          []string   `json:"header_names,omitempty"`
	SecretConfigured     *bool      `json:"secret_configured,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedBy            string     `json:"created_by,omitempty"`
	UpdatedAt            time.Time  `json:"updated_at"`
	UpdatedBy            string     `json:"updated_by,omitempty"`
	Redacted             bool       `json:"redacted"`
	// NotificationsConfigured is false when no channel is selected: the
	// monitor then notifies no one. ChannelIDs is the selection itself,
	// shown only to readers who manage notifications.
	NotificationsConfigured bool                `json:"notifications_configured"`
	ChannelIDs              *[]string           `json:"channel_ids,omitempty"`
	Live                    MonitorLiveResponse `json:"live"`
	Uptime24h               *float64            `json:"uptime_24h,omitempty"`
	Heartbeats              []HeartbeatResponse `json:"heartbeats,omitempty"`
}

// MonitorGroupResponse is one group.
type MonitorGroupResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
}

// ProbeStateResponse is whether the prober has lost connectivity.
type ProbeStateResponse struct {
	Offline bool       `json:"offline"`
	Since   *time.Time `json:"since"`
}

// ListMonitorsResponse is GET /monitors.
type ListMonitorsResponse struct {
	Monitors []MonitorResponse      `json:"monitors"`
	Groups   []MonitorGroupResponse `json:"groups"`
	Probe    ProbeStateResponse     `json:"probe"`
}

// MonitorUptimeResponse holds uptime percentages, null when nothing counted.
type MonitorUptimeResponse struct {
	Day   *float64 `json:"day"`
	Week  *float64 `json:"week"`
	Month *float64 `json:"month"`
}

// MonitorDetailResponse is GET /monitors/{monitorID}.
type MonitorDetailResponse struct {
	MonitorResponse
	Uptime MonitorUptimeResponse `json:"uptime"`
}

// MonitorHeaderRequest is one custom header. A null value keeps the stored
// value for that name.
type MonitorHeaderRequest struct {
	Name  string  `json:"name"`
	Value *string `json:"value"`
}

// MonitorRequest is the body of POST /monitors and PUT /monitors/{id}.
// On create an omitted field takes its default; on update it keeps its
// current value. Secret fields (headers, credentials, body) are write-only:
// null or omitted keeps what is stored, headers replaces the whole set.
type MonitorRequest struct {
	Name                 *string                 `json:"name"`
	URL                  *string                 `json:"url"`
	Method               *string                 `json:"method"`
	IntervalSeconds      *int                    `json:"interval_seconds"`
	Retries              *int                    `json:"retries"`
	RetryIntervalSeconds *int                    `json:"retry_interval_seconds"`
	TimeoutSeconds       *int                    `json:"timeout_seconds"`
	MaxRedirects         *int                    `json:"max_redirects"`
	AcceptedStatus       []string                `json:"accepted_status"`
	BodyEncoding         *string                 `json:"body_encoding"`
	Keyword              *string                 `json:"keyword"`
	RecoverySuccesses    *int                    `json:"recovery_successes"`
	CertNotify           *bool                   `json:"cert_notify"`
	CertWarnDays         *int                    `json:"cert_warn_days"`
	CertCritDays         *int                    `json:"cert_crit_days"`
	GroupID              *string                 `json:"group_id"`
	Description          *string                 `json:"description"`
	Tags                 []string                `json:"tags"`
	Active               *bool                   `json:"active"`
	AuthType             *string                 `json:"auth_type"`
	Headers              *[]MonitorHeaderRequest `json:"headers"`
	BasicUsername        *string                 `json:"basic_username"`
	BasicPassword        *string                 `json:"basic_password"`
	BearerToken          *string                 `json:"bearer_token"`
	Body                 *string                 `json:"body"`
	// ChannelIDs is the notification channel selection. Omitted on create
	// selects every channel flagged monitor_default; on update, keeps it.
	ChannelIDs *[]string `json:"channel_ids"`
}

// SetMonitorActiveRequest is PUT /monitors/{id}/active.
type SetMonitorActiveRequest struct {
	Active *bool `json:"active"`
}

// MonitorGroupRequest is the body of POST and PUT /monitor-groups.
type MonitorGroupRequest struct {
	Name     *string `json:"name"`
	Position *int    `json:"position"`
}

func set[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

func (req MonitorRequest) apply(m *monitor.Monitor) {
	set(&m.Name, req.Name)
	set(&m.URL, req.URL)
	if req.Method != nil {
		m.Method = monitor.Method(strings.ToUpper(*req.Method))
	}
	set(&m.IntervalSeconds, req.IntervalSeconds)
	set(&m.Retries, req.Retries)
	set(&m.RetryIntervalSeconds, req.RetryIntervalSeconds)
	set(&m.TimeoutSeconds, req.TimeoutSeconds)
	set(&m.MaxRedirects, req.MaxRedirects)
	if req.AcceptedStatus != nil {
		m.AcceptedStatus = req.AcceptedStatus
	}
	if req.BodyEncoding != nil {
		m.BodyEncoding = monitor.BodyEncoding(*req.BodyEncoding)
	}
	set(&m.Keyword, req.Keyword)
	set(&m.RecoverySuccesses, req.RecoverySuccesses)
	set(&m.CertNotify, req.CertNotify)
	set(&m.CertWarnDays, req.CertWarnDays)
	set(&m.CertCritDays, req.CertCritDays)
	set(&m.GroupID, req.GroupID)
	set(&m.Description, req.Description)
	if req.Tags != nil {
		m.Tags = req.Tags
	}
	if req.AuthType != nil {
		m.AuthType = monitor.AuthType(*req.AuthType)
	}
	m.ChannelIDs = nil
	if req.ChannelIDs != nil {
		m.ChannelIDs = nonNilIDs(*req.ChannelIDs)
	}
}

func (req MonitorRequest) secretInput() monitor.SecretInput {
	in := monitor.SecretInput{
		BasicUsername: req.BasicUsername, BasicPassword: req.BasicPassword,
		BearerToken: req.BearerToken, Body: req.Body,
	}
	if req.Headers != nil {
		in.HeadersSet = true
		for _, h := range *req.Headers {
			in.Headers = append(in.Headers, monitor.HeaderInput{Name: h.Name, Value: h.Value})
		}
	}
	return in
}

// build applies req over base and returns the monitor and secret to store.
func (h *Handler) buildMonitor(req MonitorRequest, base monitor.Monitor, stored monitor.Secret) (monitor.Monitor, monitor.Secret, error) {
	m := base
	req.apply(&m)
	if req.ChannelIDs != nil {
		if err := validateChannelIDs(*req.ChannelIDs); err != nil {
			return monitor.Monitor{}, monitor.Secret{}, err
		}
	}
	secret, names, err := monitor.MergeSecret(stored, req.secretInput(), m.AuthType, m.Method)
	if err != nil {
		return monitor.Monitor{}, monitor.Secret{}, err
	}
	m.HeaderNames = names
	if err := monitor.Validate(m, h.deps.MonitorAllowPrivateTargets); err != nil {
		return monitor.Monitor{}, monitor.Secret{}, err
	}
	return m, secret, nil
}

func presentHeartbeat(b monitor.Heartbeat) HeartbeatResponse {
	return HeartbeatResponse{Time: b.Time, Status: string(b.Status), StatusCode: b.StatusCode, ResponseMS: b.ResponseMS, Failure: string(b.Failure)}
}

func presentMonitor(v monitor.View, full bool) MonitorResponse {
	out := MonitorResponse{
		ID: v.ID, Name: v.Name, Type: "http", URL: v.URL, Host: monitor.Host(v.URL), Method: string(v.Method),
		IntervalSeconds: v.IntervalSeconds, Retries: v.Retries, RetryIntervalSeconds: v.RetryIntervalSeconds,
		TimeoutSeconds: v.TimeoutSeconds, MaxRedirects: v.MaxRedirects, AcceptedStatus: v.AcceptedStatus,
		BodyEncoding: string(v.BodyEncoding), Keyword: v.Keyword, RecoverySuccesses: v.RecoverySuccesses,
		CertNotify: v.CertNotify, CertWarnDays: v.CertWarnDays, CertCritDays: v.CertCritDays, GroupID: v.GroupID,
		Description: v.Description, Tags: v.Tags, Active: v.Active, AlertsMutedUntil: presentMute(v.AlertsMutedUntil, time.Now()),
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, NotificationsConfigured: len(v.ChannelIDs) > 0,
		Live: MonitorLiveResponse{
			Status: string(v.Live.Status), StatusSince: v.Live.StatusSince, DownSince: v.Live.DownSince,
			ConsecutiveFailures: v.Live.ConsecutiveFailures, LastCheckAt: v.Live.LastCheckAt,
			LastStatusCode: v.Live.LastStatusCode, LastResponseMS: v.Live.LastResponseMS,
			LastFailure: string(v.Live.LastFailure), NextCheckAt: v.Live.NextCheckAt, CertExpiresAt: v.Live.CertExpiresAt,
		},
	}
	if full {
		configured := v.SecretConfigured
		out.AuthType, out.HeaderNames, out.SecretConfigured = string(v.AuthType), v.HeaderNames, &configured
		out.CreatedBy, out.UpdatedBy = v.CreatedBy, v.UpdatedBy
		ids := nonNilIDs(v.ChannelIDs)
		out.ChannelIDs = &ids
		if out.HeaderNames == nil {
			out.HeaderNames = []string{}
		}
	} else {
		out.URL, out.Redacted = monitor.RedactedURL(v.URL), true
	}
	return out
}

func presentGroup(g monitor.Group) MonitorGroupResponse {
	return MonitorGroupResponse{ID: g.ID, Name: g.Name, Position: g.Position}
}

// ---------------------------------------------------------------- authz ----

// monitorReader authorizes a read: node.read plus the websites page. full
// says whether the caller also manages notifications and so sees the
// unredacted configuration.
func (h *Handler) monitorReader(r *http.Request, op string) (MonitorStore, bool, error) {
	if err := h.requirePermission(r, user.PermissionNodeRead, pageauthz.PageWebsites); err != nil {
		return nil, false, err
	}
	if h.deps.Monitors == nil {
		return nil, false, errs.New(errs.CodeUnavailable, "website monitoring is not available on this instance").WithOp(op)
	}
	full := true
	if h.deps.Authz != nil {
		principal, _ := session.PrincipalFrom(r.Context())
		full = h.deps.Authz.Require(r.Context(), principal, user.PermissionNotificationsManage, "") == nil
	}
	return h.deps.Monitors, full, nil
}

// monitorWriter authorizes a change: notifications.manage, fleet-wide.
func (h *Handler) monitorWriter(r *http.Request, op string) (MonitorStore, error) {
	if err := h.requirePermission(r, user.PermissionNotificationsManage, pageauthz.PageNone); err != nil {
		return nil, err
	}
	if h.deps.Monitors == nil {
		return nil, errs.New(errs.CodeUnavailable, "website monitoring is not available on this instance").WithOp(op)
	}
	return h.deps.Monitors, nil
}

func monitorAttrs(v monitor.View) []slog.Attr {
	return []slog.Attr{slog.String("monitor_id", v.ID), slog.String("name", v.Name), slog.String("host", monitor.Host(v.URL))}
}

// ------------------------------------------------------------- handlers ----

// ListMonitors lists every monitor with its live state, last-day uptime and
// heartbeat bar. Optional filters: q (name, host, tag), status, active,
// group_id, tag.
func (h *Handler) ListMonitors(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListMonitors"
	store, full, err := h.monitorReader(r, op)
	if err != nil {
		return err
	}
	ctx := r.Context()
	views, err := store.ListMonitors(ctx)
	if err != nil {
		return err
	}
	views, err = filterMonitors(r, views)
	if err != nil {
		return err
	}
	groups, err := store.ListGroups(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, len(views))
	for i, v := range views {
		ids[i] = v.ID
	}
	beats, err := store.RecentHeartbeats(ctx, ids, heartbeatBarLength)
	if err != nil {
		return err
	}
	uptime, err := store.UptimeSince(ctx, ids, time.Now().Add(-24*time.Hour))
	if err != nil {
		return err
	}

	out := ListMonitorsResponse{Monitors: make([]MonitorResponse, 0, len(views)), Groups: make([]MonitorGroupResponse, 0, len(groups))}
	for _, v := range views {
		m := presentMonitor(v, full)
		m.Uptime24h = uptime[v.ID].Percent()
		m.Heartbeats = make([]HeartbeatResponse, 0, len(beats[v.ID]))
		for _, b := range beats[v.ID] {
			m.Heartbeats = append(m.Heartbeats, presentHeartbeat(b))
		}
		out.Monitors = append(out.Monitors, m)
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, presentGroup(g))
	}
	if h.deps.MonitorProbe != nil {
		ps := h.deps.MonitorProbe.ProbeState()
		out.Probe = ProbeStateResponse{Offline: ps.Offline, Since: ps.Since}
	}
	httpx.JSON(w, r, http.StatusOK, out)
	return nil
}

func filterMonitors(r *http.Request, views []monitor.View) ([]monitor.View, error) {
	q := r.URL.Query()
	text := strings.ToLower(strings.TrimSpace(q.Get("q")))
	status, group, tag := q.Get("status"), q.Get("group_id"), q.Get("tag")
	var active *bool
	if a := q.Get("active"); a != "" {
		b, err := strconv.ParseBool(a)
		if err != nil {
			return nil, errs.New(errs.CodeInvalidArgument, "active must be true or false").WithDetail("field", "active")
		}
		active = &b
	}
	if status != "" && !slices.Contains([]monitor.Status{monitor.StatusUnknown, monitor.StatusUp, monitor.StatusPending, monitor.StatusDown}, monitor.Status(status)) {
		return nil, errs.New(errs.CodeInvalidArgument, "status must be unknown, up, pending or down").WithDetail("field", "status")
	}
	out := views[:0:0]
	for _, v := range views {
		switch {
		case status != "" && string(v.Live.Status) != status,
			active != nil && v.Active != *active,
			group != "" && v.GroupID != group,
			tag != "" && !slices.Contains(v.Tags, tag):
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(v.Name), text) && !strings.Contains(strings.ToLower(monitor.Host(v.URL)), text) &&
			!slices.ContainsFunc(v.Tags, func(t string) bool { return strings.Contains(strings.ToLower(t), text) }) {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// GetMonitor returns one monitor with its uptime.
func (h *Handler) GetMonitor(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.GetMonitor"
	store, full, err := h.monitorReader(r, op)
	if err != nil {
		return err
	}
	v, err := store.GetMonitor(r.Context(), r.PathValue("monitorID"))
	if err != nil {
		return err
	}
	u, err := store.Uptime(r.Context(), v.ID, time.Now())
	if err != nil {
		return err
	}
	out := MonitorDetailResponse{MonitorResponse: presentMonitor(v, full),
		Uptime: MonitorUptimeResponse{Day: u.Day.Percent(), Week: u.Week.Percent(), Month: u.Month.Percent()}}
	out.Uptime24h = out.Uptime.Day
	httpx.JSON(w, r, http.StatusOK, out)
	return nil
}

// CreateMonitor adds a monitor.
func (h *Handler) CreateMonitor(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.CreateMonitor"
	store, err := h.monitorWriter(r, op)
	if err != nil {
		return err
	}
	var req MonitorRequest
	if err := decodeStrict(r, &req); err != nil {
		return err
	}
	if req.Name == nil || req.URL == nil {
		return errs.New(errs.CodeInvalidArgument, "name and url are required").WithOp(op).WithDetail("field", "name")
	}
	base := monitor.Defaults()
	set(&base.Active, req.Active)
	m, secret, err := h.buildMonitor(req, base, monitor.Secret{})
	if err != nil {
		return err
	}
	v, err := store.CreateMonitor(r.Context(), m, secret, actorName(r))
	if err != nil {
		return err
	}
	logAdminAction(r, "monitor created", monitorAttrs(v)...)
	httpx.JSON(w, r, http.StatusCreated, presentMonitor(v, true))
	return nil
}

// UpdateMonitor changes a monitor's configuration. Pausing and resuming go
// through PUT /monitors/{id}/active instead.
func (h *Handler) UpdateMonitor(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.UpdateMonitor"
	store, err := h.monitorWriter(r, op)
	if err != nil {
		return err
	}
	var req MonitorRequest
	if err := decodeStrict(r, &req); err != nil {
		return err
	}
	if req.Active != nil {
		return errs.New(errs.CodeInvalidArgument, "pause or resume a monitor with PUT /monitors/{id}/active").
			WithOp(op).WithDetail("field", "active")
	}
	v, err := store.UpdateMonitor(r.Context(), r.PathValue("monitorID"), func(stored monitor.Monitor, s monitor.Secret) (monitor.Monitor, monitor.Secret, error) {
		return h.buildMonitor(req, stored, s)
	}, actorName(r))
	if err != nil {
		return err
	}
	logAdminAction(r, "monitor updated", monitorAttrs(v)...)
	httpx.JSON(w, r, http.StatusOK, presentMonitor(v, true))
	return nil
}

// SetMonitorActive pauses or resumes a monitor.
func (h *Handler) SetMonitorActive(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.SetMonitorActive"
	store, err := h.monitorWriter(r, op)
	if err != nil {
		return err
	}
	var req SetMonitorActiveRequest
	if err := decodeStrict(r, &req); err != nil {
		return err
	}
	if req.Active == nil {
		return errs.New(errs.CodeInvalidArgument, "active is required").WithOp(op).WithDetail("field", "active")
	}
	v, err := store.SetMonitorActive(r.Context(), r.PathValue("monitorID"), *req.Active, actorName(r))
	if err != nil {
		return err
	}
	logAdminAction(r, "monitor active set", append(monitorAttrs(v), slog.Bool("active", *req.Active))...)
	httpx.JSON(w, r, http.StatusOK, presentMonitor(v, true))
	return nil
}

// DeleteMonitor removes a monitor and its history.
func (h *Handler) DeleteMonitor(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.DeleteMonitor"
	store, err := h.monitorWriter(r, op)
	if err != nil {
		return err
	}
	monitorID := r.PathValue("monitorID")
	if err := store.DeleteMonitor(r.Context(), monitorID); err != nil {
		return err
	}
	logAdminAction(r, "monitor deleted", slog.String("monitor_id", monitorID))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// TestMonitorRequest is a draft monitor to check once: a create request,
// plus the saved monitor whose configuration and stored secrets fill in
// whatever the draft leaves out.
type TestMonitorRequest struct {
	MonitorRequest
	MonitorID string `json:"monitor_id,omitempty"`
}

// TestMonitorResponse is a test's outcome. Nothing from the response body,
// its headers or an error's text is included.
type TestMonitorResponse struct {
	Status     string `json:"status"`
	Failure    string `json:"failure"`
	StatusCode *int   `json:"status_code"`
	ResponseMS *int   `json:"response_ms"`
}

// testCheckTimeout bounds a whole test, a little above the longest timeout
// a check may have, so the check's own timeout is what normally ends it.
const testCheckTimeout = monitor.MaxTimeoutSeconds*time.Second + 5*time.Second

// TestMonitorDraft checks a draft, or a saved monitor with draft changes,
// once. It stores nothing and leaves the monitor's live state, counters and
// events alone. The audit line names only the target host.
func (h *Handler) TestMonitorDraft(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.TestMonitorDraft"
	store, err := h.monitorWriter(r, op)
	if err != nil {
		return err
	}
	if h.deps.MonitorTester == nil {
		return errs.New(errs.CodeUnavailable, "website tests are not available on this instance").WithOp(op)
	}
	var req TestMonitorRequest
	if decodeStrict(r, &req) != nil {
		// Not the decoder's own text, which can quote the body and is logged.
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	// A test needs no name; an empty one keeps the placeholder or stored name.
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		req.Name = nil
	}
	base, stored := monitor.Defaults(), monitor.Secret{}
	base.Name = "test"
	if req.MonitorID != "" {
		if base, stored, err = store.StoredMonitor(r.Context(), req.MonitorID); err != nil {
			return err
		}
	} else if req.URL == nil {
		return errs.New(errs.CodeInvalidArgument, "url is required").WithOp(op).WithDetail("field", "url")
	}
	m, secret, err := h.buildMonitor(req.MonitorRequest, base, stored)
	if err != nil {
		return err
	}
	host := strings.ToLower(monitor.Host(m.URL))
	audit := func(outcome string) {
		logAdminAction(r, "monitor tested", slog.String("monitor_id", req.MonitorID), slog.String("host", host),
			slog.String("outcome", outcome))
	}
	principal, _ := session.PrincipalFrom(r.Context())
	if retryAfter, ok := h.deps.MonitorTestLimiter.Allow(principal.UserID, "monitor-host:"+host); !ok {
		audit("rate_limited")
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
		return errs.New(errs.CodeRateLimited, "too many website tests; try again shortly").WithOp(op)
	}

	ctx, cancel := context.WithTimeout(r.Context(), testCheckTimeout)
	defer cancel()
	res, err := h.deps.MonitorTester.Test(ctx, m, secret)
	if err != nil {
		audit("busy")
		return err
	}
	out := TestMonitorResponse{Status: string(monitor.StatusDown), Failure: string(res.Failure)}
	if res.Up {
		out.Status = string(monitor.StatusUp)
	}
	if res.StatusCode > 0 {
		out.StatusCode = &res.StatusCode
	}
	if res.StatusCode > 0 || res.ResponseMS > 0 {
		out.ResponseMS = &res.ResponseMS
	}
	audit(out.Status)
	httpx.JSON(w, r, http.StatusOK, out)
	return nil
}

func monitorIntParam(r *http.Request, name string, def, lo, hi int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		return 0, errs.New(errs.CodeInvalidArgument, "%s must be between %d and %d", name, lo, hi).WithDetail("field", name)
	}
	return n, nil
}

func timeParam(r *http.Request, name string, def time.Time) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, errs.New(errs.CodeInvalidArgument, "%s must be an RFC 3339 time", name).WithDetail("field", name)
	}
	return t, nil
}

// MonitorHeartbeats returns a monitor's raw checks since ?since (default
// the last day, at most the 30 days kept), oldest first, at most ?limit
// (default 500, at most 2000) of the newest.
func (h *Handler) MonitorHeartbeats(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.MonitorHeartbeats"
	store, _, err := h.monitorReader(r, op)
	if err != nil {
		return err
	}
	now := time.Now()
	since, err := timeParam(r, "since", now.Add(-24*time.Hour))
	if err != nil {
		return err
	}
	since = maxTime(since, now.Add(-30*24*time.Hour))
	limit, err := monitorIntParam(r, "limit", 500, 1, 2000)
	if err != nil {
		return err
	}
	v, err := store.GetMonitor(r.Context(), r.PathValue("monitorID"))
	if err != nil {
		return err
	}
	beats, err := store.Heartbeats(r.Context(), v.ID, since, limit)
	if err != nil {
		return err
	}
	out := make([]HeartbeatResponse, 0, len(beats))
	for _, b := range beats {
		out = append(out, presentHeartbeat(b))
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"monitor_id": v.ID, "heartbeats": out})
	return nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// HourlyBucketResponse is one hour of checks.
type HourlyBucketResponse struct {
	Bucket        time.Time `json:"bucket"`
	Up            int       `json:"up"`
	Pending       int       `json:"pending"`
	Down          int       `json:"down"`
	Unknown       int       `json:"unknown"`
	AvgResponseMS *float64  `json:"avg_response_ms"`
	MaxResponseMS *int      `json:"max_response_ms"`
}

var historyRanges = map[string]time.Duration{
	"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "365d": 365 * 24 * time.Hour,
}

// MonitorHistory returns a monitor's hourly roll-up for ?range: 24h, 7d
// (default), 30d or 365d.
func (h *Handler) MonitorHistory(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.MonitorHistory"
	store, _, err := h.monitorReader(r, op)
	if err != nil {
		return err
	}
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "7d"
	}
	d, ok := historyRanges[rng]
	if !ok {
		return errs.New(errs.CodeInvalidArgument, "range must be 24h, 7d, 30d or 365d").WithOp(op).WithDetail("field", "range")
	}
	v, err := store.GetMonitor(r.Context(), r.PathValue("monitorID"))
	if err != nil {
		return err
	}
	buckets, err := store.HourlyHistory(r.Context(), v.ID, time.Now().Add(-d))
	if err != nil {
		return err
	}
	out := make([]HourlyBucketResponse, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, HourlyBucketResponse{Bucket: b.Bucket, Up: b.Up, Pending: b.Pending, Down: b.Down, Unknown: b.Unknown,
			AvgResponseMS: b.AvgResponseMS, MaxResponseMS: b.MaxResponseMS})
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"monitor_id": v.ID, "range": rng, "buckets": out})
	return nil
}

// MonitorEventResponse is one down/up transition.
type MonitorEventResponse struct {
	ID              string    `json:"id"`
	Time            time.Time `json:"time"`
	Kind            string    `json:"kind"`
	StatusCode      *int      `json:"status_code"`
	Failure         string    `json:"failure"`
	DurationSeconds *int      `json:"duration_seconds"`
	// CertExpiresAt is set on the cert_warn, cert_crit and cert_renewed
	// events.
	CertExpiresAt *time.Time `json:"cert_expires_at"`
}

// MonitorEvents returns a monitor's events, newest first: at most ?limit
// (default 50, at most 200), older than ?before.
func (h *Handler) MonitorEvents(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.MonitorEvents"
	store, _, err := h.monitorReader(r, op)
	if err != nil {
		return err
	}
	before, err := timeParam(r, "before", time.Now().Add(time.Second))
	if err != nil {
		return err
	}
	limit, err := monitorIntParam(r, "limit", 50, 1, 200)
	if err != nil {
		return err
	}
	v, err := store.GetMonitor(r.Context(), r.PathValue("monitorID"))
	if err != nil {
		return err
	}
	events, err := store.Events(r.Context(), v.ID, before, limit)
	if err != nil {
		return err
	}
	out := make([]MonitorEventResponse, 0, len(events))
	for _, e := range events {
		out = append(out, MonitorEventResponse{ID: e.ID, Time: e.Time, Kind: string(e.Kind), StatusCode: e.StatusCode,
			Failure: string(e.Failure), DurationSeconds: e.DurationSeconds, CertExpiresAt: e.CertExpiresAt})
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"monitor_id": v.ID, "events": out})
	return nil
}

// ListMonitorGroups lists the groups.
func (h *Handler) ListMonitorGroups(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListMonitorGroups"
	store, _, err := h.monitorReader(r, op)
	if err != nil {
		return err
	}
	groups, err := store.ListGroups(r.Context())
	if err != nil {
		return err
	}
	out := make([]MonitorGroupResponse, 0, len(groups))
	for _, g := range groups {
		out = append(out, presentGroup(g))
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"groups": out})
	return nil
}

func groupFromRequest(r *http.Request, g monitor.Group) (monitor.Group, error) {
	var req MonitorGroupRequest
	if err := decodeStrict(r, &req); err != nil {
		return monitor.Group{}, err
	}
	set(&g.Name, req.Name)
	set(&g.Position, req.Position)
	g.Name = strings.TrimSpace(g.Name)
	return g, monitor.ValidateGroup(g)
}

// CreateMonitorGroup adds a group.
func (h *Handler) CreateMonitorGroup(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.CreateMonitorGroup"
	store, err := h.monitorWriter(r, op)
	if err != nil {
		return err
	}
	g, err := groupFromRequest(r, monitor.Group{})
	if err != nil {
		return err
	}
	g, err = store.CreateGroup(r.Context(), g)
	if err != nil {
		return err
	}
	logAdminAction(r, "monitor group created", slog.String("group_id", g.ID), slog.String("name", g.Name))
	httpx.JSON(w, r, http.StatusCreated, presentGroup(g))
	return nil
}

// UpdateMonitorGroup renames or reorders a group; omitted fields keep their
// value.
func (h *Handler) UpdateMonitorGroup(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.UpdateMonitorGroup"
	store, err := h.monitorWriter(r, op)
	if err != nil {
		return err
	}
	groupID := r.PathValue("groupID")
	groups, err := store.ListGroups(r.Context())
	if err != nil {
		return err
	}
	i := slices.IndexFunc(groups, func(g monitor.Group) bool { return g.ID == groupID })
	if i < 0 {
		return errs.New(errs.CodeNotFound, "monitor group not found").WithOp(op).WithDetail("group_id", groupID)
	}
	g, err := groupFromRequest(r, groups[i])
	if err != nil {
		return err
	}
	g, err = store.UpdateGroup(r.Context(), g)
	if err != nil {
		return err
	}
	logAdminAction(r, "monitor group updated", slog.String("group_id", g.ID), slog.String("name", g.Name))
	httpx.JSON(w, r, http.StatusOK, presentGroup(g))
	return nil
}

// DeleteMonitorGroup removes a group; its monitors become ungrouped.
func (h *Handler) DeleteMonitorGroup(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.DeleteMonitorGroup"
	store, err := h.monitorWriter(r, op)
	if err != nil {
		return err
	}
	groupID := r.PathValue("groupID")
	if err := store.DeleteGroup(r.Context(), groupID); err != nil {
		return err
	}
	logAdminAction(r, "monitor group deleted", slog.String("group_id", groupID))
	w.WriteHeader(http.StatusNoContent)
	return nil
}
