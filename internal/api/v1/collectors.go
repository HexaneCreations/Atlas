package v1

import (
	"net/http"

	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/core/scheduler"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/storage/metric"
)

// CollectorsResponse reports the state of collection on this instance.
//
// A monitoring platform that cannot say which of its own collectors are
// failing will eventually report a host as healthy because nothing has
// observed it for an hour. This endpoint is what makes that visible.
type CollectorsResponse struct {
	NodeID     string                      `json:"node_id"`
	Plugins    []plugin.State              `json:"plugins"`
	Collectors []scheduler.CollectorHealth `json:"collectors"`
	Scheduler  scheduler.Stats             `json:"scheduler"`
	Ingest     metric.Stats                `json:"ingest"`
}

// ListCollectors reports plugin activation and per-collector health.
func (h *Handler) ListCollectors(w http.ResponseWriter, r *http.Request) error {
	if h.deps.Collection == nil {
		httpx.JSON(w, r, http.StatusOK, CollectorsResponse{
			Plugins: []plugin.State{}, Collectors: []scheduler.CollectorHealth{},
		})
		return nil
	}

	plugins := h.deps.Collection.PluginStates()
	if plugins == nil {
		plugins = []plugin.State{}
	}
	collectors := h.deps.Collection.CollectorHealth()
	if collectors == nil {
		collectors = []scheduler.CollectorHealth{}
	}

	httpx.JSON(w, r, http.StatusOK, CollectorsResponse{
		NodeID:     h.deps.Collection.Identity().NodeID,
		Plugins:    plugins,
		Collectors: collectors,
		Scheduler:  h.deps.Collection.SchedulerStats(),
		Ingest:     h.deps.Collection.IngestStats(),
	})
	return nil
}
