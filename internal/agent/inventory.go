package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/plugin/cron"
	"github.com/hexane/atlas/internal/plugin/docker"
	"github.com/hexane/atlas/internal/plugin/ports"
	"github.com/hexane/atlas/internal/plugin/process"
	"github.com/hexane/atlas/internal/plugin/service"
	"github.com/hexane/atlas/internal/plugin/system"
)

func inventorySources(
	active map[string]bool,
	processPlugin *process.Plugin, servicePlugin *service.Plugin, cronPlugin *cron.Plugin,
	portsPlugin *ports.Plugin, systemPlugin *system.Plugin, dockerPlugin *docker.Plugin,
) []inventorySource {
	var sources []inventorySource

	if active["process"] {
		sources = append(sources, inventorySource{coreinventory.SubjectProcesses, func(ctx context.Context) (any, error) {
			return processPlugin.Inventory(ctx)
		}})
	}
	if active["service"] {
		sources = append(sources,
			inventorySource{coreinventory.SubjectServices, func(ctx context.Context) (any, error) {
				return servicePlugin.Inventory(ctx)
			}},
			inventorySource{coreinventory.SubjectServiceGraph, func(ctx context.Context) (any, error) {
				return servicePlugin.Graph(ctx)
			}},
		)
	}
	if active["cron"] {
		sources = append(sources, inventorySource{coreinventory.SubjectCronJobs, func(ctx context.Context) (any, error) {
			return cronPlugin.Inventory(ctx)
		}})
	}
	if active["ports"] {
		sources = append(sources, inventorySource{coreinventory.SubjectPorts, func(ctx context.Context) (any, error) {
			return portsPlugin.Inventory(ctx)
		}})
	}
	if active["system"] {
		sources = append(sources, inventorySource{coreinventory.SubjectMounts, func(ctx context.Context) (any, error) {
			return systemPlugin.Mounts(ctx)
		}})
	}
	if active["docker"] {
		sources = append(sources, inventorySource{coreinventory.SubjectContainers, func(ctx context.Context) (any, error) {
			client := dockerPlugin.DockerClient()
			if client == nil {
				return nil, nil
			}
			return client.Containers(ctx)
		}})
	}

	return sources
}

type inventorySource struct {
	subject coreinventory.Subject
	fetch   func(ctx context.Context) (any, error)
}

// inventoryPusher periodically pushes changed inventory snapshots.
//
// A subject is skipped when its content hash is unchanged since the last
// push — most subjects on a stable host rarely change, and this is what
// keeps the steady-state push volume low.
type inventoryPusher struct {
	tr       transport.Transport
	origin   transport.Origin
	sources  []inventorySource
	interval time.Duration
	logger   *slog.Logger

	mu       sync.Mutex
	lastHash map[coreinventory.Subject]string
}

func newInventoryPusher(tr transport.Transport, origin transport.Origin, interval time.Duration, logger *slog.Logger, sources []inventorySource) *inventoryPusher {
	return &inventoryPusher{
		tr: tr, origin: origin, sources: sources, interval: interval,
		logger: logger, lastHash: map[coreinventory.Subject]string{},
	}
}

func (p *inventoryPusher) run(ctx context.Context) {
	p.pushAll(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pushAll(ctx)
		}
	}
}

func (p *inventoryPusher) pushAll(ctx context.Context) {
	for _, src := range p.sources {
		data, err := src.fetch(ctx)
		if err != nil {
			p.logger.WarnContext(ctx, "inventory fetch failed",
				slog.String("subject", string(src.subject)), slog.String("error", err.Error()))
			continue
		}
		if data == nil {
			continue
		}

		raw, err := json.Marshal(data)
		if err != nil {
			p.logger.WarnContext(ctx, "inventory encode failed",
				slog.String("subject", string(src.subject)), slog.String("error", err.Error()))
			continue
		}

		hash := contentHash(raw)
		p.mu.Lock()
		unchanged := p.lastHash[src.subject] == hash
		p.mu.Unlock()
		if unchanged {
			continue
		}

		payload := coreinventory.Payload{
			Subject: src.subject, ObservedAt: time.Now(), ContentHash: hash, Data: raw,
		}
		env := transport.NewEnvelopeOf(p.origin, payload)
		if err := p.tr.Send(ctx, env); err != nil {
			p.logger.WarnContext(ctx, "inventory push failed",
				slog.String("subject", string(src.subject)), slog.String("error", err.Error()))
			continue
		}

		p.mu.Lock()
		p.lastHash[src.subject] = hash
		p.mu.Unlock()
	}
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
