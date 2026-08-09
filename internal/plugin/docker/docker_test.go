package docker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/platform/eventbus"
)

func find(t *testing.T, samples []collect.Sample, metric string, labels map[string]string) collect.Sample {
	t.Helper()
	for _, s := range samples {
		if s.Metric != metric {
			continue
		}
		ok := true
		for k, v := range labels {
			if s.Labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return s
		}
	}
	t.Fatalf("no sample %q with labels %v among %d", metric, labels, len(samples))
	return collect.Sample{}
}

func has(samples []collect.Sample, metric string) bool {
	for _, s := range samples {
		if s.Metric == metric {
			return true
		}
	}
	return false
}

func assertValid(t *testing.T, samples []collect.Sample) {
	t.Helper()
	for _, s := range samples {
		if err := s.Validate(); err != nil {
			t.Errorf("invalid sample %+v: %v", s, err)
		}
	}
}

func TestContainerCollector(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	samples, err := newContainerCollector(c, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	assertValid(t, samples)

	labels := map[string]string{"container": "web"}
	if up := find(t, samples, "docker.container.up", labels); up.Value != 1 {
		t.Errorf("up = %v, want 1", up.Value)
	}

	// A container running now but restarted repeatedly is not healthy, and no
	// instantaneous state shows that.
	if r := find(t, samples, "docker.container.restarts", labels); r.Value != 3 {
		t.Errorf("restarts = %v, want 3", r.Value)
	}

	// Compose metadata is what lets Tier 2 group containers into services.
	if up := find(t, samples, "docker.container.up", labels); up.Labels["compose_service"] != "web" {
		t.Errorf("compose_service = %q", up.Labels["compose_service"])
	}

	// Aggregates must include zero states, so a chart shows a state falling to
	// zero rather than the series vanishing.
	if exited := find(t, samples, "docker.containers.count", map[string]string{"state": "exited"}); exited.Value != 0 {
		t.Errorf("exited count = %v, want an explicit 0", exited.Value)
	}
}

// The container id changes on every recreate; charting by it would break a
// dashboard each time. The name is the stable identity.
func TestMetricLabelsExcludeContainerIDAndArbitraryLabels(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	c.containers[0].Labels["annotation.with.pod.uid"] = "b7f1e0a2-9c3d-4e5f-8a1b-2c3d4e5f6a7b"

	samples, err := newContainerCollector(c, nil).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range samples {
		for key, value := range s.Labels {
			if value == c.containers[0].ID {
				t.Errorf("%s carries the container id as label %q", s.Metric, key)
			}
			if key == "annotation.with.pod.uid" {
				t.Errorf("%s copied an arbitrary container label into a series", s.Metric)
			}
		}
	}
}

// Most images declare no health check. Reporting them as unhealthy would make
// the signal fire for every container on the host.
func TestHealthOmittedWhenNoCheckDeclared(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	c.containers[0].Health = HealthNone

	samples, err := newContainerCollector(c, nil).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if s.Metric == "docker.container.healthy" {
			t.Error("emitted a health series for a container with no health check")
		}
	}
}

func TestUnhealthyContainerIsReported(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	c.containers[0].Health = HealthUnhealthy

	samples, err := newContainerCollector(c, nil).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := find(t, samples, "docker.container.healthy", map[string]string{"container": "web"})
	if h.Value != 0 {
		t.Errorf("healthy = %v, want 0 for an unhealthy container", h.Value)
	}
	if h.Labels["health"] != "unhealthy" {
		t.Errorf("health label = %q", h.Labels["health"])
	}
}

func TestExitCodeReportedForStoppedContainers(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	c.containers[0].State = StateExited
	c.containers[0].ExitCode = 137 // SIGKILL, usually the OOM killer

	samples, err := newContainerCollector(c, nil).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	code := find(t, samples, "docker.container.exit_code", map[string]string{"container": "web"})
	if code.Value != 137 {
		t.Errorf("exit_code = %v, want 137", code.Value)
	}
	if has(samples, "docker.container.uptime") {
		t.Error("reported uptime for a stopped container")
	}
}

func TestStatsCollector(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	sc := newStatsCollector(c, nil, 4)
	ctx := context.Background()

	if _, err := sc.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	s := c.stats["abc123def456789"]
	s.NetworkRxBytes, s.NetworkTxBytes = 100000, 200000
	s.BlockReadBytes, s.BlockWriteBytes = 40960, 81920
	c.stats["abc123def456789"] = s

	samples, err := sc.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertValid(t, samples)

	labels := map[string]string{"container": "web"}
	if cpu := find(t, samples, "docker.container.cpu.usage", labels); cpu.Value != 12.5 {
		t.Errorf("cpu = %v, want 12.5", cpu.Value)
	}
	if mem := find(t, samples, "docker.container.memory.usage", labels); mem.Value != float64(256<<20) {
		t.Errorf("memory = %v", mem.Value)
	}
	if rx := find(t, samples, "docker.container.network.rx", labels); rx.Value <= 0 {
		t.Errorf("rx rate = %v, want positive after the counter advanced", rx.Value)
	}
}

// A limit of zero means unlimited; a percentage against it is meaningless and
// the division would produce a NaN the pipeline rejects.
func TestStatsOmitsMemoryPercentWhenUnlimited(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	s := c.stats["abc123def456789"]
	s.MemoryLimit, s.MemoryPercent = 0, 0
	c.stats["abc123def456789"] = s

	samples, err := newStatsCollector(c, nil, 4).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertValid(t, samples)
	if has(samples, "docker.container.memory.percent") {
		t.Error("emitted a memory percentage for an unlimited container")
	}
	if !has(samples, "docker.container.memory.usage") {
		t.Error("absolute memory usage was lost")
	}
}

// A container that stops between the list and the stats read is routine.
func TestStatsSkipsContainersThatVanish(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	c.containers = append(c.containers, Container{
		ID: "gone999", Name: "ghost", State: StateRunning,
	})
	c.statsErr["gone999"] = errors.New("no such container")

	samples, err := newStatsCollector(c, nil, 4).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v; one vanished container must not fail the run", err)
	}
	if !has(samples, "docker.container.cpu.usage") {
		t.Error("the surviving container produced no samples")
	}
	for _, s := range samples {
		if s.Labels["container"] == "ghost" {
			t.Error("emitted samples for a container that could not be read")
		}
	}
}

func TestStatsReturnsNothingWhenNoContainersRun(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	c.containers[0].State = StateExited

	samples, err := newStatsCollector(c, nil, 4).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Errorf("got %d samples with nothing running", len(samples))
	}
}

func TestInventoryCollectorReturnsPartialDataOnFailure(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	c.volumesErr = errors.New("storage driver busy")

	samples, err := newInventoryCollector(c, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	assertValid(t, samples)
	if !has(samples, "docker.images.count") || !has(samples, "docker.networks.count") {
		t.Error("a volume failure cost the image and network counts")
	}
	if has(samples, "docker.volumes.count") {
		t.Error("emitted volume counts although the read failed")
	}
}

func TestEventClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		event     Event
		topic     eventbus.Topic
		chartable bool
	}{
		{Event{Type: "container", Action: "start"}, TopicContainerStarted, true},
		{Event{Type: "container", Action: "die"}, TopicContainerDied, true},
		{Event{Type: "container", Action: "oom"}, TopicContainerOOM, true},
		{Event{Type: "container", Action: "health_status: unhealthy"}, TopicContainerHealth, true},
		// A busy CI host emits thousands of these an hour; charting them would
		// bury the events that indicate a problem.
		{Event{Type: "container", Action: "create"}, TopicContainerCreated, false},
		{Event{Type: "image", Action: "pull"}, TopicImageChanged, false},
		{Event{Type: "network", Action: "connect"}, TopicNetworkChanged, false},
		{Event{Type: "daemon", Action: "reload"}, TopicDaemonEvent, false},
	}

	for _, tt := range tests {
		topic, chartable := classify(tt.event)
		if topic != tt.topic {
			t.Errorf("%s/%s topic = %q, want %q", tt.event.Type, tt.event.Action, topic, tt.topic)
		}
		if chartable != tt.chartable {
			t.Errorf("%s/%s chartable = %v, want %v", tt.event.Type, tt.event.Action, chartable, tt.chartable)
		}
	}
}

func TestEventStreamerPublishesAndCharts(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	bus := eventbus.New(eventbus.Options{BufferSize: 16})
	defer bus.Close()

	sub, err := bus.Subscribe("test", "docker.**")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan collect.Sample, 8)
	st := newEventStreamer(c, bus, "node-1", nil)
	go func() { _ = st.Stream(ctx, out) }()

	c.events <- Event{
		Type: "container", Action: "die", ActorID: "abc123", ActorName: "web",
		Attributes: map[string]string{"exitCode": "137", "image": "nginx:1.27"},
		Time:       time.Now(),
	}

	select {
	case e := <-sub.C:
		if e.Topic != TopicContainerDied {
			t.Errorf("topic = %q", e.Topic)
		}
		payload, ok := e.Payload.(ContainerEvent)
		if !ok {
			t.Fatalf("payload type = %T", e.Payload)
		}
		if payload.ExitCode != "137" || payload.Name != "web" {
			t.Errorf("payload = %+v", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no event was published to the bus")
	}

	select {
	case s := <-out:
		if s.Metric != "docker.events" || s.Labels["action"] != "die" {
			t.Errorf("sample = %+v", s)
		}
		if err := s.Validate(); err != nil {
			t.Errorf("invalid sample: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no sample was emitted for a chartable event")
	}
}

// A daemon restart closes the stream. Returning nil tells the scheduler to
// reconnect rather than recording a fault.
func TestEventStreamEndsCleanlyWhenTheDaemonCloses(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	st := newEventStreamer(c, nil, "node-1", nil)

	done := make(chan error, 1)
	go func() { done <- st.Stream(context.Background(), make(chan collect.Sample, 4)) }()

	close(c.events)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Stream() = %v, want nil so the scheduler reconnects", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stream did not return when the daemon closed the stream")
	}
}

func TestDetectReturnsFalseWhenNoDaemon(t *testing.T) {
	t.Parallel()

	p := New(Options{NewClient: func(string) (Client, error) {
		return nil, errors.New("cannot reach the docker socket")
	}})

	detected, err := p.Detect(context.Background())
	if err != nil {
		t.Errorf("Detect() error = %v; an absent daemon is not an error", err)
	}
	if detected {
		t.Error("Detect() = true with no daemon")
	}
}

// A socket that exists but does not answer must not pass detection, or every
// collection fails and Atlas reports a broken integration instead of an
// unusable one.
func TestDetectReturnsFalseWhenDaemonDoesNotAnswer(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	c.pingErr = errors.New("daemon is restarting")
	p := New(Options{NewClient: func(string) (Client, error) { return c, nil }})

	detected, err := p.Detect(context.Background())
	if err != nil {
		t.Errorf("Detect() error = %v", err)
	}
	if detected {
		t.Error("Detect() = true for a daemon that did not answer")
	}
	if !c.closed {
		t.Error("the unusable client was not closed")
	}
}

func TestPluginLifecycle(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	p := New(Options{NewClient: func(string) (Client, error) { return c, nil }})
	ctx := context.Background()

	detected, err := p.Detect(ctx)
	if err != nil || !detected {
		t.Fatalf("Detect() = %v, %v", detected, err)
	}

	if err := p.Init(ctx, plugin.Env{Config: plugin.NoConfig, NodeID: "node-1"}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := len(p.Collectors()); got != 3 {
		t.Errorf("Collectors() = %d, want 3", got)
	}
	if got := len(p.Streamers()); got != 1 {
		t.Errorf("Streamers() = %d, want 1 (the event stream)", got)
	}
	if p.Version().Version != "28.3.3" {
		t.Errorf("Version = %+v", p.Version())
	}

	if err := p.Close(ctx); err != nil {
		t.Errorf("Close() error = %v", err)
	}
	if err := p.Close(ctx); err != nil {
		t.Errorf("second Close() error = %v", err)
	}
	if !c.closed {
		t.Error("the client was not closed")
	}
}

func TestEventsCanBeDisabledByConfiguration(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	p := New(Options{NewClient: func(string) (Client, error) { return c, nil }})
	ctx := context.Background()

	if _, err := p.Detect(ctx); err != nil {
		t.Fatal(err)
	}

	off := false
	err := p.Init(ctx, plugin.Env{
		NodeID: "node-1",
		Config: func(target any) error {
			settings, ok := target.(*Settings)
			if !ok {
				t.Fatalf("Config called with %T, want *Settings", target)
			}
			settings.CollectEvents = &off
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(p.Streamers()); got != 0 {
		t.Errorf("Streamers() = %d with events disabled, want 0", got)
	}
	if got := len(p.Collectors()); got != 3 {
		t.Errorf("disabling events also removed collectors: %d", got)
	}
}

func TestInvalidConfigurationFailsInit(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	p := New(Options{NewClient: func(string) (Client, error) { return c, nil }})
	ctx := context.Background()
	if _, err := p.Detect(ctx); err != nil {
		t.Fatal(err)
	}

	err := p.Init(ctx, plugin.Env{
		NodeID: "node-1",
		Config: func(any) error { return errors.New("yaml: line 3: mapping values are not allowed") },
	})
	if err == nil {
		t.Error("Init() accepted an invalid configuration section")
	}
}

func TestCollectorsHonourCancellation(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	c.blockFor = time.Hour

	collectors := map[string]collect.Collector{
		"containers": newContainerCollector(c, nil),
		"stats":      newStatsCollector(c, nil, 4),
		"inventory":  newInventoryCollector(c, nil),
	}

	for name, collector := range collectors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()

			done := make(chan struct{})
			go func() {
				_, _ = collector.Collect(ctx)
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("collector ignored cancellation; a wedged daemon would pin a scheduler slot")
			}
		})
	}
}

func TestDescriptorsAreValidAndUnique(t *testing.T) {
	t.Parallel()

	c := newFakeClient()
	descs := []collect.Descriptor{
		newContainerCollector(c, nil).Descriptor(),
		newStatsCollector(c, nil, 4).Descriptor(),
		newInventoryCollector(c, nil).Descriptor(),
		newEventStreamer(c, nil, "n", nil).Descriptor(),
	}

	seen := map[string]bool{}
	for _, d := range descs {
		if err := d.Validate(); err != nil {
			t.Errorf("descriptor %q invalid: %v", d.ID, err)
		}
		if seen[d.ID] {
			t.Errorf("duplicate id %q", d.ID)
		}
		seen[d.ID] = true
	}
}
