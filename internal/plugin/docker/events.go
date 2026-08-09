package docker

import (
	"context"
	"log/slog"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/platform/eventbus"
)

// Event topics published from the Docker daemon's stream.
//
// Named <plugin>.<resource>.<action>, general to specific, so a subscriber can
// match docker.container.** without enumerating actions that do not exist yet.
const (
	TopicContainerStarted   eventbus.Topic = "docker.container.started"
	TopicContainerStopped   eventbus.Topic = "docker.container.stopped"
	TopicContainerDied      eventbus.Topic = "docker.container.died"
	TopicContainerOOM       eventbus.Topic = "docker.container.oom"
	TopicContainerHealth    eventbus.Topic = "docker.container.health_changed"
	TopicContainerCreated   eventbus.Topic = "docker.container.created"
	TopicContainerDestroyed eventbus.Topic = "docker.container.destroyed"
	TopicImageChanged       eventbus.Topic = "docker.image.changed"
	TopicVolumeChanged      eventbus.Topic = "docker.volume.changed"
	TopicNetworkChanged     eventbus.Topic = "docker.network.changed"
	TopicDaemonEvent        eventbus.Topic = "docker.daemon.event"
)

// ContainerEvent is the payload of the container topics.
type ContainerEvent struct {
	NodeID      string `json:"node_id"`
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	Image       string `json:"image,omitempty"`
	Action      string `json:"action"`
	// ExitCode is present on a die event and is the reason the container
	// stopped. 137 is SIGKILL, which usually means the OOM killer.
	ExitCode   string            `json:"exit_code,omitempty"`
	Health     string            `json:"health,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	At         time.Time         `json:"at"`
}

// ActivityDescription implements [activity.Describer], so the recent-activity
// feed can name the container rather than showing a bare 64-character id.
//
// The interface is satisfied structurally, not by importing the activity
// package: plugins depend on core, so core cannot depend back on a plugin to
// recognise this type. See the activity package doc.
func (e ContainerEvent) ActivityDescription() (title, detail string) {
	name := e.Name
	if name == "" {
		name = shortID(e.ContainerID)
	}

	switch e.Action {
	case "start":
		title = "Container started"
	case "stop", "kill":
		title = "Container stopped"
	case "die":
		title = "Container exited"
	case "oom":
		title = "Container out of memory"
	case "health_status":
		title = "Container health changed"
	default:
		title = "Container " + e.Action
	}

	detail = name
	if e.Health != "" {
		detail += " · " + e.Health
	}
	// A non-zero exit is the whole reason a die event is worth reading; zero
	// is a clean shutdown and adding "exit 0" to every one of them is noise.
	if e.ExitCode != "" && e.ExitCode != "0" {
		detail += " · exit " + e.ExitCode
	}
	return title, detail
}

// eventStreamer follows the Docker daemon's event stream.
//
// It is a [collect.Streamer] rather than a goroutine started in Plugin.Init,
// which means the scheduler supervises it: a daemon restart ends the stream and
// the scheduler reconnects with jittered backoff, a panic is contained, and the
// stream's health appears alongside every other collector's.
//
// Events serve two purposes and are handled differently for each. Every event
// is published on the bus, where the incident timeline and the WebSocket
// fan-out consume it. Only the ones worth charting become samples — a count of
// restarts and OOM kills over time — because emitting a sample per event would
// make a busy image build look like a container incident.
type eventStreamer struct {
	client Client
	bus    *eventbus.Bus
	logger *slog.Logger
	nodeID string
}

func newEventStreamer(c Client, bus *eventbus.Bus, nodeID string, logger *slog.Logger) *eventStreamer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &eventStreamer{client: c, bus: bus, logger: logger, nodeID: nodeID}
}

func (s *eventStreamer) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "docker.events",
		Name:        "Docker events",
		Description: "The Docker daemon's event stream: lifecycle, health, and OOM kills.",
	}
}

func (s *eventStreamer) Stream(ctx context.Context, out chan<- collect.Sample) error {
	events, errCh := s.client.Events(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil

		case err, ok := <-errCh:
			if !ok {
				return nil
			}
			return err

		case event, ok := <-events:
			if !ok {
				// The daemon closed the stream — usually a restart. Returning
				// nil tells the scheduler to reconnect rather than treating it
				// as a fault.
				return nil
			}
			s.handle(ctx, event, out)
		}
	}
}

func (s *eventStreamer) handle(ctx context.Context, e Event, out chan<- collect.Sample) {
	topic, chartable := classify(e)

	if s.bus != nil {
		s.bus.Publish(ctx, eventbus.Event{
			Topic:   topic,
			Source:  "plugin.docker",
			Subject: e.ActorID,
			Payload: ContainerEvent{
				NodeID:      s.nodeID,
				ContainerID: e.ActorID,
				Name:        e.ActorName,
				Image:       e.Attributes["image"],
				Action:      e.Action,
				ExitCode:    e.Attributes["exitCode"],
				Health:      e.Attributes["health_status"],
				Attributes:  e.Attributes,
				At:          e.Time,
			},
		})
	}

	if !chartable {
		return
	}

	// A counter increment of one, labelled by what happened. Charted as a rate
	// this answers "how often are containers dying", which is the question an
	// operator actually asks — and it does so without a series per event.
	labels := map[string]string{"action": e.Action}
	if e.ActorName != "" {
		labels["container"] = e.ActorName
	}

	select {
	case out <- collect.Sample{
		Metric: "docker.events",
		Value:  1,
		Unit:   collect.UnitCount,
		Kind:   collect.KindCounter,
		Time:   eventTime(e),
		Labels: labels,
	}:
	case <-ctx.Done():
	}
}

// classify maps a daemon event to a topic, and says whether it is worth
// charting.
//
// Most events are not. A busy CI host emits thousands of image pull, tag, and
// untag events an hour; charting them would bury the handful that indicate a
// problem. Only lifecycle transitions and failures become samples.
func classify(e Event) (eventbus.Topic, bool) {
	if e.Type != "container" {
		switch e.Type {
		case "image":
			return TopicImageChanged, false
		case "volume":
			return TopicVolumeChanged, false
		case "network":
			return TopicNetworkChanged, false
		default:
			return TopicDaemonEvent, false
		}
	}

	switch e.Action {
	case "start":
		return TopicContainerStarted, true
	case "stop", "kill":
		return TopicContainerStopped, true
	case "die":
		return TopicContainerDied, true
	case "oom":
		// An OOM kill is the single most actionable container event: the
		// container was killed for exceeding its memory limit, and no
		// instantaneous memory reading will show it after the fact.
		return TopicContainerOOM, true
	case "create":
		return TopicContainerCreated, false
	case "destroy":
		return TopicContainerDestroyed, false
	default:
		// health_status arrives as "health_status: healthy".
		if len(e.Action) >= 13 && e.Action[:13] == "health_status" {
			return TopicContainerHealth, true
		}
		return TopicDaemonEvent, false
	}
}

// eventTime falls back to now when the daemon supplies no usable timestamp,
// since a zero time would be rejected by sample validation and lose the event.
func eventTime(e Event) time.Time {
	if e.Time.IsZero() || e.Time.Year() < 2000 {
		return time.Now()
	}
	return e.Time
}
