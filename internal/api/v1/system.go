// Package v1 implements version 1 of the Atlas HTTP API.
//
// Versioning is by URL prefix (/api/v1). Within a version, changes must be
// additive: new fields and new endpoints are fine, and removing or repurposing
// either is not. A breaking change means /api/v2 served alongside v1, because
// Atlas is consumed by dashboards, scripts, and alerting rules that nobody
// upgrades in lockstep. See docs/api/versioning.md.
//
// Handlers here are deliberately thin: they read a request, call a
// collaborator, and render. Anything resembling logic belongs below them, so
// that it can be tested without an HTTP request.
package v1

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/core/activity"
	"github.com/hexane/atlas/internal/core/alert"
	"github.com/hexane/atlas/internal/core/capacityplanning"
	"github.com/hexane/atlas/internal/core/costanalysis"
	"github.com/hexane/atlas/internal/core/eventstore"
	"github.com/hexane/atlas/internal/core/goldensignals"
	"github.com/hexane/atlas/internal/core/healthscore"
	"github.com/hexane/atlas/internal/core/incident"
	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/core/scheduler"
	"github.com/hexane/atlas/internal/core/slo"
	"github.com/hexane/atlas/internal/platform/build"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/hostid"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/plugin/cron"
	"github.com/hexane/atlas/internal/plugin/docker"
	"github.com/hexane/atlas/internal/plugin/ports"
	"github.com/hexane/atlas/internal/plugin/process"
	"github.com/hexane/atlas/internal/plugin/service"
	"github.com/hexane/atlas/internal/plugin/system"
	"github.com/hexane/atlas/internal/storage/metric"
)

// Prefix is the path every version 1 route is mounted under.
const Prefix = "/api/v1"

// Deps are the collaborators the v1 handlers need.
//
// They are passed explicitly rather than resolved from a container: the
// compiler then proves at every call site that a handler's dependencies
// exist, and a reader can see what an endpoint touches without tracing
// registrations.
type Deps struct {
	Config *config.Config
	Health *health.Registry
	Pool   *postgres.Pool
	Bus    *eventbus.Bus
	// Collection exposes the running collection pipeline. Declared as an
	// interface so this package depends on the behaviour it needs rather than
	// on the composition root, which would be a cycle.
	Collection CollectionSource
	// Activity is the in-memory recent-activity feed. Nil disables the
	// endpoint rather than crashing it.
	Activity *activity.Recorder
	// Inventory is the latest-only snapshot store agents push into. Nil is a
	// valid, ordinary state — it means this Atlas instance has no agent
	// support wired in, and every inventory endpoint simply behaves as it did
	// before this milestone: local scopes work, remote scopes report
	// unavailable rather than trying to reach a store that does not exist.
	Inventory inventory.Store
	// Nodes answers whether Atlas has ever recorded a given node id. Used
	// only by the remote inventory path, to tell "no such node" (404) apart
	// from "this node exists but has not reported inventory" (503) — two
	// different facts a caller must not see collapsed into one. A narrow
	// interface rather than the full [CollectionSource.Repository] surface,
	// so the distinction can be tested without a database.
	Nodes NodeExistence
	// EventStore is the durable event log. Nil disables the endpoint rather
	// than crashing it, the same convention as Activity.
	EventStore EventStore
	// Alerts is the alert rule store. Nil disables the endpoints rather than
	// crashing them, the same convention as Activity and EventStore.
	Alerts AlertStore
	// Incidents is the incident store. Nil disables the endpoints rather
	// than crashing them, the same convention as the others above.
	Incidents IncidentStore
	// HealthScore is the health score engine. Nil disables the endpoints
	// rather than crashing them, the same convention as the others above.
	HealthScore *healthscore.Engine
	// CostAnalysis is the cost analysis engine. Nil disables the endpoints
	// rather than crashing them, the same convention as the others above.
	CostAnalysis *costanalysis.Engine
	// CapacityPlanning is the capacity planning engine. Nil disables the
	// endpoints rather than crashing them, the same convention as the
	// others above.
	CapacityPlanning *capacityplanning.Engine
	// GoldenSignals is the Golden Signals engine. Nil disables the
	// endpoints rather than crashing them, the same convention as the
	// others above.
	GoldenSignals *goldensignals.Engine
	// SLO is the SLO evaluation engine. Nil disables the endpoints rather
	// than crashing them, the same convention as the others above.
	SLO *slo.Engine
	// SLOStore is the SLO definition store. Nil disables the endpoints
	// rather than crashing them, the same convention as the others above.
	SLOStore SLOStore
	// NotificationStore is the notification channel and delivery store.
	// Nil disables the endpoints rather than crashing them, the same
	// convention as the others above.
	NotificationStore NotificationStore
}

// SLOStore is the SLO definition store the API reads and writes. Satisfied
// by [*github.com/hexane/atlas/internal/storage/slo.Repository].
type SLOStore interface {
	ListSLOs(ctx context.Context) ([]slo.Definition, error)
	GetSLO(ctx context.Context, id string) (slo.Definition, error)
	CreateSLO(ctx context.Context, def slo.Definition) (slo.Definition, error)
	UpdateSLO(ctx context.Context, def slo.Definition) (slo.Definition, error)
	DeleteSLO(ctx context.Context, id string) error
}

// NotificationStore is the notification channel and delivery store the API
// reads and writes. Satisfied by
// [*github.com/hexane/atlas/internal/storage/notification.Repository].
type NotificationStore interface {
	ListChannels(ctx context.Context) ([]notification.Channel, error)
	GetChannel(ctx context.Context, id string) (notification.Channel, error)
	CreateChannel(ctx context.Context, c notification.Channel) (notification.Channel, error)
	UpdateChannel(ctx context.Context, c notification.Channel) (notification.Channel, error)
	DeleteChannel(ctx context.Context, id string) error
	ListDeliveries(ctx context.Context, filter notification.DeliveryFilter) ([]notification.Delivery, error)
}

// EventStore is the durable event log the API reads from. Satisfied by
// [*github.com/hexane/atlas/internal/storage/eventstore.Repository].
type EventStore interface {
	Query(ctx context.Context, filter eventstore.Filter) ([]eventstore.Record, error)
}

// AlertStore is the alert rule store the API reads and writes. Satisfied by
// [*github.com/hexane/atlas/internal/storage/alert.Repository].
type AlertStore interface {
	ListRules(ctx context.Context) ([]alert.Rule, error)
	GetRule(ctx context.Context, id string) (alert.Rule, error)
	CreateRule(ctx context.Context, rule alert.Rule) (alert.Rule, error)
	UpdateRule(ctx context.Context, rule alert.Rule) (alert.Rule, error)
	DeleteRule(ctx context.Context, id string) error
	ListActiveStates(ctx context.Context) ([]alert.AlertState, error)
	QueryHistory(ctx context.Context, filter alert.HistoryFilter) ([]alert.HistoryEntry, error)
}

// IncidentStore is the incident store the API reads from. Satisfied by
// [*github.com/hexane/atlas/internal/storage/incident.Repository].
type IncidentStore interface {
	ListIncidents(ctx context.Context, filter incident.Filter) ([]incident.Incident, error)
	GetDetail(ctx context.Context, id string) (incident.Detail, error)
}

// NodeExistence is the minimal node lookup the remote inventory path needs.
// Satisfied by [*metric.Repository].
type NodeExistence interface {
	GetNode(ctx context.Context, nodeID string) (metric.Node, error)
}

// CollectionSource is the view of the collection pipeline the API needs.
type CollectionSource interface {
	Repository() *metric.Repository
	CollectorHealth() []scheduler.CollectorHealth
	PluginStates() []plugin.State
	SchedulerStats() scheduler.Stats
	IngestStats() metric.Stats
	Identity() hostid.Identity
	// DockerClient is nil where Docker is absent, which is the normal state on
	// most hosts rather than a failure.
	DockerClient() docker.Client
	// PluginActive distinguishes "nothing found" from "not available here".
	PluginActive(id string) bool
	// Every inventory call carries the scope it is asking about.
	//
	// Atlas can only read its own host today, and each of these returns a
	// typed unavailable error for any other node rather than an empty result —
	// an empty container list for a remote host would read as "nothing is
	// running there". Once agents push remote inventory, satisfying these for
	// other nodes is an implementation change behind an unchanged seam.
	Processes(ctx context.Context, scope inventory.Scope) ([]process.Process, error)
	Services(ctx context.Context, scope inventory.Scope) ([]service.Unit, error)
	// ServiceGraph is the dependency graph. Nil where there is no service
	// manager, the same convention as the other inventories.
	ServiceGraph(ctx context.Context, scope inventory.Scope) (*service.Graph, error)
	CronJobs(ctx context.Context, scope inventory.Scope) ([]cron.Job, error)
	Ports(ctx context.Context, scope inventory.Scope) ([]ports.Listener, error)
	Mounts(ctx context.Context, scope inventory.Scope) ([]system.MountInfo, error)
}

// scopeFrom reads the inventory scope from a request.
//
// `node` is optional and defaults to the host Atlas runs on, which keeps every
// existing caller working and makes the common case the short one. Responses
// echo the resolved node id so a caller always knows which host answered
// rather than having to assume.
func (h *Handler) scopeFrom(r *http.Request) inventory.Scope {
	return inventory.Scope{NodeID: strings.TrimSpace(r.URL.Query().Get("node"))}
}

// resolvedNode reports which node a scope actually addressed, for echoing back
// in the response.
func (h *Handler) resolvedNode(scope inventory.Scope) string {
	if scope.NodeID != "" {
		return scope.NodeID
	}
	if h.deps.Collection == nil {
		return ""
	}
	return h.deps.Collection.Identity().NodeID
}

// Handler serves the version 1 API.
type Handler struct {
	deps Deps
}

// NewHandler builds the v1 handler.
func NewHandler(deps Deps) *Handler { return &Handler{deps: deps} }

// Mount registers every v1 route on mux.
//
// Routes are declared with Go 1.22 method-and-pattern syntax, so an
// unsupported method returns 405 from the router rather than reaching a
// handler that has to check for itself.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.Handle("GET "+Prefix+"/system/info", httpx.Handler(h.SystemInfo))
	mux.Handle("GET "+Prefix+"/system/health", httpx.Handler(h.SystemHealth))
	mux.Handle("GET "+Prefix+"/system/runtime", httpx.Handler(h.SystemRuntime))

	mux.Handle("GET "+Prefix+"/nodes", httpx.Handler(h.ListNodes))
	mux.Handle("GET "+Prefix+"/nodes/{nodeID}", httpx.Handler(h.GetNode))

	mux.Handle("GET "+Prefix+"/metrics", httpx.Handler(h.QueryMetrics))
	mux.Handle("GET "+Prefix+"/metrics/latest", httpx.Handler(h.LatestMetrics))
	mux.Handle("GET "+Prefix+"/metrics/names", httpx.Handler(h.ListMetricNames))

	mux.Handle("GET "+Prefix+"/collectors", httpx.Handler(h.ListCollectors))
	mux.Handle("GET "+Prefix+"/activity", httpx.Handler(h.ListActivity))
	mux.Handle("GET "+Prefix+"/events", httpx.Handler(h.ListEvents))

	mux.Handle("GET "+Prefix+"/alerts/rules", httpx.Handler(h.ListAlertRules))
	mux.Handle("POST "+Prefix+"/alerts/rules", httpx.Handler(h.CreateAlertRule))
	mux.Handle("GET "+Prefix+"/alerts/rules/{ruleID}", httpx.Handler(h.GetAlertRule))
	mux.Handle("PUT "+Prefix+"/alerts/rules/{ruleID}", httpx.Handler(h.UpdateAlertRule))
	mux.Handle("DELETE "+Prefix+"/alerts/rules/{ruleID}", httpx.Handler(h.DeleteAlertRule))
	mux.Handle("GET "+Prefix+"/alerts", httpx.Handler(h.ListActiveAlerts))
	mux.Handle("GET "+Prefix+"/alerts/history", httpx.Handler(h.ListAlertHistory))

	mux.Handle("GET "+Prefix+"/incidents", httpx.Handler(h.ListIncidents))
	mux.Handle("GET "+Prefix+"/incidents/{incidentID}", httpx.Handler(h.GetIncident))

	mux.Handle("GET "+Prefix+"/health/score", httpx.Handler(h.HealthScore))
	mux.Handle("GET "+Prefix+"/health/fleet", httpx.Handler(h.HealthScoreFleet))

	mux.Handle("GET "+Prefix+"/cost/estimate", httpx.Handler(h.CostEstimate))
	mux.Handle("GET "+Prefix+"/cost/fleet", httpx.Handler(h.CostFleet))

	mux.Handle("GET "+Prefix+"/capacity/summary", httpx.Handler(h.CapacitySummary))
	mux.Handle("GET "+Prefix+"/capacity/fleet", httpx.Handler(h.CapacityFleet))

	mux.Handle("GET "+Prefix+"/signals", httpx.Handler(h.GoldenSignals))
	mux.Handle("GET "+Prefix+"/signals/fleet", httpx.Handler(h.GoldenSignalsFleet))

	mux.Handle("GET "+Prefix+"/slo", httpx.Handler(h.ListSLOs))
	mux.Handle("POST "+Prefix+"/slo", httpx.Handler(h.CreateSLO))
	mux.Handle("GET "+Prefix+"/slo/{sloID}", httpx.Handler(h.GetSLO))
	mux.Handle("PUT "+Prefix+"/slo/{sloID}", httpx.Handler(h.UpdateSLO))
	mux.Handle("DELETE "+Prefix+"/slo/{sloID}", httpx.Handler(h.DeleteSLO))
	mux.Handle("GET "+Prefix+"/slo/{sloID}/status", httpx.Handler(h.SLOStatus))

	mux.Handle("GET "+Prefix+"/notifications/channels", httpx.Handler(h.ListNotificationChannels))
	mux.Handle("POST "+Prefix+"/notifications/channels", httpx.Handler(h.CreateNotificationChannel))
	mux.Handle("GET "+Prefix+"/notifications/channels/{channelID}", httpx.Handler(h.GetNotificationChannel))
	mux.Handle("PUT "+Prefix+"/notifications/channels/{channelID}", httpx.Handler(h.UpdateNotificationChannel))
	mux.Handle("DELETE "+Prefix+"/notifications/channels/{channelID}", httpx.Handler(h.DeleteNotificationChannel))
	mux.Handle("GET "+Prefix+"/notifications/deliveries", httpx.Handler(h.ListNotificationDeliveries))

	mux.Handle("GET "+Prefix+"/containers", httpx.Handler(h.ListContainers))
	mux.Handle("GET "+Prefix+"/containers/{containerID}", httpx.Handler(h.GetContainer))
	mux.Handle("GET "+Prefix+"/containers/{containerID}/logs", httpx.Handler(h.ContainerLogs))
	mux.Handle("GET "+Prefix+"/containers/{containerID}/logs/follow", httpx.Handler(h.ContainerLogsFollow))

	mux.Handle("GET "+Prefix+"/processes", httpx.Handler(h.ListProcesses))
	mux.Handle("GET "+Prefix+"/services", httpx.Handler(h.ListServices))
	// The literal /services/graph route is registered before the {unit}
	// wildcard. Go's mux prefers the more specific pattern regardless of
	// registration order, but keeping them adjacent makes the precedence
	// visible to a reader.
	mux.Handle("GET "+Prefix+"/services/graph", httpx.Handler(h.ServiceGraph))
	mux.Handle("GET "+Prefix+"/services/{unit}", httpx.Handler(h.ServiceDetail))
	mux.Handle("GET "+Prefix+"/services/{unit}/impact", httpx.Handler(h.ServiceImpact))
	mux.Handle("GET "+Prefix+"/cron", httpx.Handler(h.ListCronJobs))
	mux.Handle("GET "+Prefix+"/ports", httpx.Handler(h.ListPorts))
	mux.Handle("GET "+Prefix+"/mounts", httpx.Handler(h.ListMounts))
}

// SystemInfoResponse describes the running Atlas instance.
type SystemInfoResponse struct {
	Version       string    `json:"version"`
	Commit        string    `json:"commit"`
	BuildTime     string    `json:"build_time"`
	GoVersion     string    `json:"go_version"`
	Platform      string    `json:"platform"`
	Dirty         bool      `json:"dirty"`
	Environment   string    `json:"environment"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	APIVersion    string    `json:"api_version"`
}

// SystemInfo reports the identity of this Atlas instance.
//
// This is the first endpoint an operator hits during an incident, and the one
// a fleet inventory scrapes, so it must answer without touching the database:
// it has to work when everything else does not.
func (h *Handler) SystemInfo(w http.ResponseWriter, r *http.Request) error {
	info := build.Current()

	httpx.JSON(w, r, http.StatusOK, SystemInfoResponse{
		Version:       info.Version,
		Commit:        info.Commit,
		BuildTime:     info.BuildTime,
		GoVersion:     info.GoVersion,
		Platform:      info.Platform,
		Dirty:         info.Dirty,
		Environment:   string(h.deps.Config.Environment),
		StartedAt:     build.Started,
		UptimeSeconds: build.Uptime().Seconds(),
		APIVersion:    "v1",
	})
	return nil
}

// SystemHealthResponse is the detailed health of this instance and its
// dependencies.
type SystemHealthResponse struct {
	Status        health.Status  `json:"status"`
	Checks        []health.Check `json:"checks"`
	CheckedAt     time.Time      `json:"checked_at"`
	Version       string         `json:"version"`
	UptimeSeconds float64        `json:"uptime_seconds"`
}

// SystemHealth reports the health of every dependency.
//
// Unlike the /readyz probe, this always returns 200 when Atlas can answer at
// all: it is a diagnostic for humans and dashboards, and a non-200 would make
// the detail unreadable in exactly the situation it is needed. The machine
// verdict lives in the body's status field, and in /readyz for load balancers.
func (h *Handler) SystemHealth(w http.ResponseWriter, r *http.Request) error {
	report := h.deps.Health.Run(r.Context())

	httpx.JSON(w, r, http.StatusOK, SystemHealthResponse{
		Status:        report.Status,
		Checks:        report.Checks,
		CheckedAt:     report.CheckedAt,
		Version:       build.Current().Version,
		UptimeSeconds: build.Uptime().Seconds(),
	})
	return nil
}

// SystemRuntimeResponse reports Atlas's own resource use.
type SystemRuntimeResponse struct {
	Database postgres.Stats `json:"database"`
	EventBus eventbus.Stats `json:"event_bus"`
	Process  ProcessRuntime `json:"process"`
}

// ProcessRuntime describes the Go runtime of this process.
type ProcessRuntime struct {
	Goroutines  int    `json:"goroutines"`
	HeapAllocMB uint64 `json:"heap_alloc_mb"`
	HeapSysMB   uint64 `json:"heap_sys_mb"`
	GCCycles    uint32 `json:"gc_cycles"`
	NumCPU      int    `json:"num_cpu"`
	MaxProcs    int    `json:"max_procs"`
}

// SystemRuntime reports Atlas's own resource consumption.
//
// A monitoring platform that cannot be monitored is a blind spot at the
// centre of the observability strategy. This endpoint is what lets Atlas
// answer questions about itself — a goroutine leak in a collector, a
// saturated connection pool, an event bus dropping events — with the same
// tooling used for everything else it watches.
func (h *Handler) SystemRuntime(w http.ResponseWriter, r *http.Request) error {
	httpx.JSON(w, r, http.StatusOK, SystemRuntimeResponse{
		Database: h.deps.Pool.Stats(),
		EventBus: h.deps.Bus.Stats(),
		Process:  currentProcessRuntime(),
	})
	return nil
}
