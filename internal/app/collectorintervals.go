package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/plugin/docker"
	"github.com/hexane/atlas/internal/plugin/service"
)

// The collectors behind docker.container.* and service.*.
const (
	containerCollectorID = "docker.containers"
	serviceCollectorID   = "service.units"
)

// inertDockerClient lets the docker plugin initialise, and so contribute its
// collectors, without a daemon. Only the calls Detect and Init make are
// implemented.
type inertDockerClient struct{ docker.Client }

func (inertDockerClient) Ping(context.Context) (docker.Version, error) {
	return docker.Version{}, nil
}
func (inertDockerClient) Close() error { return nil }

// registeredIntervals returns the interval each container and service
// collector registers with, read from the collectors' own descriptors.
//
// The running collection pipeline cannot answer this: a control plane in a
// container has no Docker daemon or systemd, so those plugins are never
// activated there even though every agent activates them. An interval a
// collector leaves unset resolves to def, as the scheduler resolves it.
func registeredIntervals(ctx context.Context, def time.Duration, logger *slog.Logger) (map[string]time.Duration, error) {
	const op = "app.registeredIntervals"

	dockerPlugin := docker.New(docker.Options{
		NewClient: func(string) (docker.Client, error) { return inertDockerClient{}, nil },
	})
	if _, err := dockerPlugin.Detect(ctx); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not read the container collector's interval").WithOp(op)
	}
	servicePlugin := service.New(service.Options{})

	out := map[string]time.Duration{}
	for _, p := range []plugin.Plugin{dockerPlugin, servicePlugin} {
		// Discarded: the plugin's own "ready" line would describe a daemon
		// that is not there.
		env := plugin.Env{Logger: slog.New(slog.DiscardHandler), Config: plugin.NoConfig}
		if err := p.Init(ctx, env); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read plugin %q's collector intervals", p.Descriptor().ID).WithOp(op)
		}
		for _, c := range p.Collectors() {
			d := c.Descriptor()
			out[d.ID] = d.Interval
			if d.Interval <= 0 {
				out[d.ID] = def
			}
		}
		_ = p.Close(ctx)
	}
	for _, id := range []string{containerCollectorID, serviceCollectorID} {
		if out[id] <= 0 {
			return nil, errs.New(errs.CodeInternal, "no collector %q is registered", id).WithOp(op)
		}
	}
	logger.DebugContext(ctx, "notification staleness intervals",
		slog.Duration(containerCollectorID, out[containerCollectorID]), slog.Duration(serviceCollectorID, out[serviceCollectorID]))
	return out, nil
}
