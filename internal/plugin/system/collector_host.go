package system

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/platform/eventbus"
)

// FactsRecorder persists the descriptive properties of a node.
//
// Declared here as an interface rather than taking the storage repository
// directly, so this package keeps its dependency direction: a plugin depends
// on a contract it defines, never on a storage implementation.
type FactsRecorder interface {
	UpdateNodeFacts(ctx context.Context, nodeID string, facts NodeFacts) error
}

// NodeFacts are the descriptive properties of a machine.
type NodeFacts struct {
	OS           string
	Platform     string
	Kernel       string
	Architecture string
	CPUCores     int
	BootTime     time.Time
	HardwareUUID string
}

// Event topics published when the machine's identity changes underneath Atlas.
const (
	// TopicHostRebooted is published when boot time moves. A reboot resets
	// every counter on the host, so anything computing rates or comparing
	// against history needs to know it happened rather than infer it from a
	// discontinuity.
	TopicHostRebooted eventbus.Topic = "system.host.rebooted"
	// TopicHostRenamed is published when the hostname changes. Identity is
	// unaffected — that is what node ids are for — but the change belongs on
	// the incident timeline, because a rename usually accompanies a rebuild.
	TopicHostRenamed eventbus.Topic = "system.host.renamed"
)

// HostChange is the payload of the host topics.
type HostChange struct {
	NodeID   string    `json:"node_id"`
	Previous string    `json:"previous"`
	Current  string    `json:"current"`
	At       time.Time `json:"at"`
}

// hostCollector records the descriptive facts about the machine and reports
// its uptime.
//
// Facts are written to storage rather than emitted as samples. A kernel
// version is not a time series: storing "kernel = 6.8.0" once a minute forever
// would be thousands of identical rows to answer a question that is really
// "what is it now". Uptime *is* emitted, because a chart of it makes reboots
// visible at a glance.
type hostCollector struct {
	provider Provider
	recorder FactsRecorder
	bus      *eventbus.Bus
	logger   *slog.Logger
	nodeID   string

	mu           sync.Mutex
	lastBootTime time.Time
	lastHostname string
}

func newHostCollector(p Provider, recorder FactsRecorder, bus *eventbus.Bus, nodeID string, logger *slog.Logger) *hostCollector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &hostCollector{provider: p, recorder: recorder, bus: bus, nodeID: nodeID, logger: logger}
}

func (c *hostCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "system.host",
		Name:        "Host",
		Description: "Machine identity, operating system facts, and uptime.",
		// Descriptive facts change on a kernel upgrade or a reboot, not on a
		// fifteen-second cadence.
		Interval: 60 * time.Second,
		Timeout:  20 * time.Second,
	}
}

func (c *hostCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()

	info, err := c.provider.Host(ctx)
	if err != nil {
		return nil, err
	}

	c.detectChanges(ctx, info, now)

	if c.recorder != nil {
		facts := NodeFacts{
			OS:           info.OS,
			Platform:     PlatformLabel(info),
			Kernel:       info.KernelVersion,
			Architecture: info.KernelArch,
			CPUCores:     info.LogicalCores,
			BootTime:     info.BootTime,
			HardwareUUID: info.HardwareUUID,
		}
		if err := c.recorder.UpdateNodeFacts(ctx, c.nodeID, facts); err != nil {
			// Uptime is still worth reporting even if the facts could not be
			// written; the failure is logged by the sink and surfaced through
			// the collector's own health.
			c.logger.WarnContext(ctx, "could not record host facts", slog.Any("error", err))
		}
	}

	samples := []collect.Sample{
		gauge("system.uptime", info.Uptime().Seconds(), collect.UnitSeconds, now),
	}
	if info.LogicalCores > 0 {
		samples = append(samples, gauge("system.cpu.cores", float64(info.LogicalCores), collect.UnitCount, now))
	}
	return samples, nil
}

// detectChanges publishes an event when the machine's identity shifts.
func (c *hostCollector) detectChanges(ctx context.Context, info HostInfo, now time.Time) {
	c.mu.Lock()
	previousBoot, previousName := c.lastBootTime, c.lastHostname
	c.lastBootTime, c.lastHostname = info.BootTime, info.Hostname
	c.mu.Unlock()

	if c.bus == nil {
		return
	}

	// Boot time is derived from uptime and drifts by a second or two between
	// reads, so only a substantial jump is a reboot.
	if !previousBoot.IsZero() && absDuration(info.BootTime.Sub(previousBoot)) > time.Minute {
		c.logger.InfoContext(ctx, "host reboot detected",
			slog.Time("previous_boot", previousBoot), slog.Time("current_boot", info.BootTime))
		c.bus.Publish(ctx, eventbus.Event{
			Topic: TopicHostRebooted, Source: "plugin.system", Subject: c.nodeID,
			Payload: HostChange{
				NodeID:   c.nodeID,
				Previous: previousBoot.Format(time.RFC3339),
				Current:  info.BootTime.Format(time.RFC3339),
				At:       now,
			},
		})
	}

	if previousName != "" && previousName != info.Hostname {
		c.logger.InfoContext(ctx, "hostname changed",
			slog.String("previous", previousName), slog.String("current", info.Hostname))
		c.bus.Publish(ctx, eventbus.Event{
			Topic: TopicHostRenamed, Source: "plugin.system", Subject: c.nodeID,
			Payload: HostChange{
				NodeID: c.nodeID, Previous: previousName, Current: info.Hostname, At: now,
			},
		})
	}
}

// PlatformLabel renders the distribution and version as one display string
// ("ubuntu 24.04"). It is the single definition of how nodes.platform is
// composed, so both the local host collector and the remote-inventory
// promotion path store the same value.
func PlatformLabel(info HostInfo) string {
	if info.Platform == "" {
		return ""
	}
	if info.PlatformVersion == "" {
		return info.Platform
	}
	return info.Platform + " " + info.PlatformVersion
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
