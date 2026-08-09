// Package api assembles Atlas's HTTP surface.
//
// It owns route composition and the orchestration probes, and delegates the
// versioned business endpoints to a per-version package. Keeping the router
// separate from any version means adding /api/v2 later changes this file by
// one line rather than restructuring anything.
package api

import (
	"context"
	"net/http"
	"time"

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
	}).Mount(mux)

	requestTimeout := deps.Config.Server.WriteTimeout - time.Second
	if requestTimeout <= 0 {
		requestTimeout = deps.Config.Server.WriteTimeout
	}

	// JSONErrorFallback sits directly outside the mux, so it sees the router's
	// own 404 and 405 responses and rewrites them into the standard envelope.
	// It must be inside the base chain, so those rewritten responses still get
	// a request id, security headers, and an access-log line.
	base := httpx.BaseMiddleware(deps.Config.Server, requestTimeout)(
		httpx.JSONErrorFallback()(mux),
	)
	stream := httpx.StreamMiddleware(deps.Config.Server)(mux)

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
