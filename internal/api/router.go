// Package api assembles Atlas's HTTP surface.
//
// It owns route composition and the orchestration probes, and delegates the
// versioned business endpoints to a per-version package. Keeping the router
// separate from any version means adding /api/v2 later changes this file by
// one line rather than restructuring anything.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/api/session"
	v1 "github.com/hexane/atlas/internal/api/v1"
	"github.com/hexane/atlas/internal/core/activity"
	corecapacityplanning "github.com/hexane/atlas/internal/core/capacityplanning"
	corecostanalysis "github.com/hexane/atlas/internal/core/costanalysis"
	coregoldensignals "github.com/hexane/atlas/internal/core/goldensignals"
	corehealthscore "github.com/hexane/atlas/internal/core/healthscore"
	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	coreslo "github.com/hexane/atlas/internal/core/slo"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/platform/postgres"
)

// probeTimeout bounds a readiness probe. Orchestrators call /readyz on a
// short interval, so a probe that waits on a hung dependency would queue up
// rather than report.
const probeTimeout = 3 * time.Second

// Deps are everything the HTTP surface needs.
type Deps struct {
	Config            *config.Config
	Health            *health.Registry
	Pool              *postgres.Pool
	Bus               *eventbus.Bus
	Collection        v1.CollectionSource
	Activity          *activity.Recorder
	Inventory         coreinventory.Store
	RemoteLogs        v1.RemoteLogSource
	Nodes             v1.NodeExistence
	EventStore        v1.EventStore
	Alerts            v1.AlertStore
	Incidents         v1.IncidentStore
	HealthScore       *corehealthscore.Engine
	CostAnalysis      *corecostanalysis.Engine
	CapacityPlanning  *corecapacityplanning.Engine
	GoldenSignals     *coregoldensignals.Engine
	SLO               *coreslo.Engine
	SLOStore          v1.SLOStore
	NotificationStore v1.NotificationStore
	// Users, Sessions and Authz back human-user authentication and
	// authorization (see internal/core/user and internal/api/v1's auth
	// endpoints). Nil disables login entirely rather than crashing it — the
	// same convention as Activity, EventStore and the other stores above —
	// which is what lets [router_test.go]'s minimal Deps keep working
	// unchanged.
	Users    v1.UserStore
	Sessions v1.SessionStore
	Authz    v1.Authorizer
	// LoginLimiter bounds POST /auth/login attempts. Nil disables
	// throttling rather than crashing it, the same convention as the
	// stores above.
	LoginLimiter v1.LoginLimiter
	// Logger is used only to build the authentication middleware; nil is
	// valid and falls back to slog's default handler.
	Logger *slog.Logger
}

// New builds the complete HTTP handler, middleware included.
func New(deps Deps) http.Handler {
	mux := http.NewServeMux()

	// Orchestration probes sit outside /api/v1 and outside versioning
	// entirely. A Kubernetes probe configuration should never have to change
	// because the API version did.
	mux.Handle("GET /healthz", httpx.Handler(handleLive))
	mux.Handle("GET /readyz", httpx.Handler(handleReady(deps.Health)))

	v1.NewHandler(v1.Deps{
		Config:            deps.Config,
		Health:            deps.Health,
		Pool:              deps.Pool,
		Bus:               deps.Bus,
		Collection:        deps.Collection,
		Activity:          deps.Activity,
		Inventory:         deps.Inventory,
		RemoteLogs:        deps.RemoteLogs,
		Nodes:             deps.Nodes,
		EventStore:        deps.EventStore,
		Alerts:            deps.Alerts,
		Incidents:         deps.Incidents,
		HealthScore:       deps.HealthScore,
		CostAnalysis:      deps.CostAnalysis,
		CapacityPlanning:  deps.CapacityPlanning,
		GoldenSignals:     deps.GoldenSignals,
		SLO:               deps.SLO,
		SLOStore:          deps.SLOStore,
		NotificationStore: deps.NotificationStore,
		Users:             deps.Users,
		Sessions:          deps.Sessions,
		Authz:             deps.Authz,
		LoginLimiter:      deps.LoginLimiter,
		SessionSecure:     deps.Config.Environment.IsProduction(),
	}).Mount(mux)

	requestTimeout := deps.Config.Server.WriteTimeout - time.Second
	if requestTimeout <= 0 {
		requestTimeout = deps.Config.Server.WriteTimeout
	}

	// authenticate is the one shared authentication segment both chains
	// below splice in — see docs/adr/0011-deferred-rbac.md sec 1 and
	// [httpx.BaseMiddleware]'s doc. A nil Sessions store (no database-backed
	// session store wired in, e.g. a minimal test harness) leaves every
	// request anonymous rather than panicking, the same "nil disables rather
	// than crashes" convention every other optional dependency in [Deps]
	// follows.
	authenticate := httpx.Middleware(func(next http.Handler) http.Handler { return next })
	if deps.Sessions != nil {
		authenticate = session.AuthMiddleware(deps.Sessions, deps.Logger)
	}

	// JSONErrorFallback sits directly outside the mux, so it sees the router's
	// own 404 and 405 responses and rewrites them into the standard envelope.
	// It must be inside the base chain, so those rewritten responses still get
	// a request id, security headers, and an access-log line.
	base := httpx.BaseMiddleware(deps.Config.Server, requestTimeout, authenticate)(
		httpx.JSONErrorFallback()(mux),
	)
	stream := httpx.StreamMiddleware(deps.Config.Server, authenticate)(mux)

	// A WebSocket upgrade is dispatched through the streaming chain instead of
	// the base one — same mux, same routes, but without the fixed request
	// deadline that would otherwise cut a live log view off mid-incident. See
	// [httpx.StreamMiddleware].
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if httpx.IsWebSocketUpgrade(r) {
			stream.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

// handleLive answers the liveness probe.
//
// It checks nothing. Liveness asks only "is this process able to serve?", and
// the honest answer is yes if this handler ran at all. Probing dependencies
// here is a well-known way to turn a database blip into a cascading restart
// of every application instance — the orchestrator kills healthy processes
// for a fault they did not have and cannot fix by restarting.
func handleLive(w http.ResponseWriter, r *http.Request) error {
	httpx.JSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
	return nil
}

// handleReady answers the readiness probe.
//
// Unlike liveness, this does check dependencies: readiness asks whether this
// instance should receive traffic, and an Atlas that cannot reach Postgres
// cannot answer a single useful query. A degraded instance still reports
// ready — reduced visibility is not a reason to take the last working
// instance out of rotation.
func handleReady(registry *health.Registry) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()

		report := registry.Run(ctx)

		status := http.StatusOK
		if !report.Serving() {
			status = http.StatusServiceUnavailable
		}
		httpx.JSON(w, r, status, report)
		return nil
	}
}
