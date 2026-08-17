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
		sources = append(sources,
			inventorySource{coreinventory.SubjectMounts, func(ctx context.Context) (any, error) {
				return systemPlugin.Mounts(ctx)
			}},
			inventorySource{coreinventory.SubjectNetwork, func(ctx context.Context) (any, error) {
				return systemPlugin.NetworkIdentity(ctx)
			}},
			inventorySource{coreinventory.SubjectHost, func(ctx context.Context) (any, error) {
				return systemPlugin.Host(ctx)
			}},
		)
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
// A subject is skipped for a relationship when its content hash is unchanged
// since the last push that relationship actually accepted — most subjects on
// a stable host rarely change, and this is what keeps the steady-state push
// volume low.
//
// The hash is tracked per relationship, never globally. Inventory is
// snapshot-class: it is dropped rather than spooled on failure, so a global
// cache would let a push that one control plane accepted and another
// rejected be recorded as delivered to both, leaving the second carrying
// stale inventory indefinitely — until the underlying data happened to
// change again.
type inventoryPusher struct {
	fanout   *fanoutTransport
	origin   transport.Origin
	sources  []inventorySource
	interval time.Duration
	logger   *slog.Logger

	mu       sync.Mutex
	lastHash map[string]map[coreinventory.Subject]string
}

func newInventoryPusher(fanout *fanoutTransport, origin transport.Origin, interval time.Duration, logger *slog.Logger, sources []inventorySource) *inventoryPusher {
	return &inventoryPusher{
		fanout: fanout, origin: origin, sources: sources, interval: interval,
		logger: logger, lastHash: map[string]map[coreinventory.Subject]string{},
	}
}

// staleRelationships returns the relationships that have not accepted this
// exact content for this subject yet.
func (p *inventoryPusher) staleRelationships(subject coreinventory.Subject, hash string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var stale []string
	for _, id := range p.fanout.targetIDs() {
		if p.lastHash[id][subject] != hash {
			stale = append(stale, id)
		}
	}
	return stale
}

func (p *inventoryPusher) recordDelivered(id string, subject coreinventory.Subject, hash string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.lastHash[id] == nil {
		p.lastHash[id] = map[coreinventory.Subject]string{}
	}
	p.lastHash[id][subject] = hash
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
		stale := p.staleRelationships(src.subject, hash)
		if len(stale) == 0 {
			continue
		}

		payload := coreinventory.Payload{
			Subject: src.subject, ObservedAt: time.Now(), ContentHash: hash, Data: raw,
		}
		env := transport.NewEnvelopeOf(p.origin, payload)
		for id, err := range p.fanout.SendTo(ctx, stale, env) {
			if err != nil {
				p.logger.WarnContext(ctx, "inventory push failed",
					slog.String("relationship", id),
					slog.String("subject", string(src.subject)), slog.String("error", err.Error()))
				continue
			}
			p.recordDelivered(id, src.subject, hash)
		}
	}
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
