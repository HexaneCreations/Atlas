package system

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
)

// findSample returns the first sample with the given metric and matching
// labels.
func findSample(t *testing.T, samples []collect.Sample, metric string, labels map[string]string) collect.Sample {
	t.Helper()
	for _, s := range samples {
		if s.Metric != metric {
			continue
		}
		match := true
		for k, v := range labels {
			if s.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return s
		}
	}
	t.Fatalf("no sample %q with labels %v among %d samples", metric, labels, len(samples))
	return collect.Sample{}
}

func hasMetric(samples []collect.Sample, metric string) bool {
	for _, s := range samples {
		if s.Metric == metric {
			return true
		}
	}
	return false
}

// Every sample entering the pipeline must be valid, or the scheduler discards
// it and the series silently has a hole.
func assertAllValid(t *testing.T, samples []collect.Sample) {
	t.Helper()
	if len(samples) == 0 {
		t.Fatal("collector produced no samples")
	}
	for _, s := range samples {
		if err := s.Validate(); err != nil {
			t.Errorf("invalid sample %+v: %v", s, err)
		}
	}
}

func TestCPUCollector(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	c := newCPUCollector(p)
	ctx := context.Background()

	// First run establishes the baseline for the state counters.
	if _, err := c.Collect(ctx); err != nil {
		t.Fatalf("priming Collect() error = %v", err)
	}

	// Advance the cumulative counters as a real host would.
	p.cpuTimes = CPUTimes{User: 1100, System: 550, Idle: 8400, IOWait: 120}

	samples, err := c.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	assertAllValid(t, samples)

	// The aggregate must equal the mean of the per-core figures, or the chart
	// and its breakdown contradict each other.
	agg := findSample(t, samples, "system.cpu.usage", nil)
	if want := 45.0; math.Abs(agg.Value-want) > 0.001 {
		t.Errorf("system.cpu.usage = %v, want the mean %v", agg.Value, want)
	}
	if agg.Unit != collect.UnitPercent || agg.Kind != collect.KindGauge {
		t.Errorf("aggregate unit/kind = %v/%v", agg.Unit, agg.Kind)
	}

	core3 := findSample(t, samples, "system.cpu.core.usage", map[string]string{"core": "3"})
	if core3.Value != 40 {
		t.Errorf("core 3 = %v, want 40", core3.Value)
	}

	// iowait is the figure that distinguishes a busy CPU from a starved one.
	if !hasMetric(samples, "system.cpu.time") {
		t.Error("no system.cpu.time breakdown was emitted")
	}
	iowait := findSample(t, samples, "system.cpu.time", map[string]string{"state": "iowait"})
	if iowait.Value <= 0 {
		t.Errorf("iowait rate = %v, want positive after the counter advanced", iowait.Value)
	}
}

// A virtualised host can report slightly over 100%; storing it makes every
// chart axis on the page auto-scale wrongly.
func TestCPUPercentagesAreClamped(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	p.cpuPct = []float64{-3, 100.4, 50}
	c := newCPUCollector(p)

	samples, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if s.Unit == collect.UnitPercent && (s.Value < 0 || s.Value > 100) {
			t.Errorf("%s = %v, outside 0-100", s.Metric, s.Value)
		}
	}
}

// Losing the state breakdown is not worth discarding the per-core figures.
func TestCPUReturnsPartialDataWhenTimesFail(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	p.cpuTimesErr = errors.New("permission denied")
	c := newCPUCollector(p)

	samples, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v; a failed breakdown must not fail the run", err)
	}
	assertAllValid(t, samples)
	if !hasMetric(samples, "system.cpu.usage") {
		t.Error("aggregate utilisation was lost along with the breakdown")
	}
	if hasMetric(samples, "system.cpu.time") {
		t.Error("a breakdown was emitted although the read failed")
	}
}

func TestMemoryCollector(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	c := newMemoryCollector(p)
	ctx := context.Background()

	if _, err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	p.swap = SwapInfo{Total: 4 << 30, Used: 1 << 30, Free: 3 << 30, UsedPercent: 25, Sin: 5000, Sout: 9000}

	samples, err := c.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	assertAllValid(t, samples)

	for _, metric := range []string{
		"system.memory.total", "system.memory.used", "system.memory.available",
		"system.memory.usage", "system.swap.total", "system.swap.usage",
	} {
		if !hasMetric(samples, metric) {
			t.Errorf("missing %s", metric)
		}
	}

	// Available is the figure that answers "can this host take more work";
	// Free is near zero on every healthy Linux box.
	avail := findSample(t, samples, "system.memory.available", nil)
	if avail.Value != float64(8<<30) {
		t.Errorf("available = %v, want 8 GiB", avail.Value)
	}
	if avail.Unit != collect.UnitBytes {
		t.Errorf("available unit = %v, want bytes", avail.Unit)
	}

	// Swap activity, not occupancy, is the pressure signal.
	if !hasMetric(samples, "system.swap.in") || !hasMetric(samples, "system.swap.out") {
		t.Error("swap activity rates were not emitted after the counters advanced")
	}
}

// Emitting zeros for a metric the platform does not report would draw a flat
// line implying a measurement was taken.
func TestMemoryOmitsUnreportedFields(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	p.memory.Cached = 0
	p.memory.Buffers = 0
	c := newMemoryCollector(p)

	samples, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hasMetric(samples, "system.memory.cached") {
		t.Error("emitted system.memory.cached although the platform reported none")
	}
	if hasMetric(samples, "system.memory.buffers") {
		t.Error("emitted system.memory.buffers although the platform reported none")
	}
}

func TestMemoryReturnsPartialDataWhenSwapFails(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	p.swapErr = errors.New("no swap configured")
	c := newMemoryCollector(p)

	samples, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if !hasMetric(samples, "system.memory.usage") {
		t.Error("memory metrics were lost because swap failed")
	}
	if hasMetric(samples, "system.swap.usage") {
		t.Error("swap metrics were emitted although the read failed")
	}
}

func TestDiskCollector(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	samples, err := newDiskCollector(p, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	assertAllValid(t, samples)

	labels := map[string]string{"mountpoint": "/"}
	usage := findSample(t, samples, "system.disk.usage", labels)
	if usage.Value != 50 {
		t.Errorf("disk usage = %v, want 50", usage.Value)
	}
	if usage.Labels["device"] != "/dev/sda1" || usage.Labels["fstype"] != "ext4" {
		t.Errorf("labels = %v", usage.Labels)
	}

	// Inode exhaustion produces "no space left on device" on a filesystem with
	// plenty of free bytes; without this series the symptom is unexplainable.
	if !hasMetric(samples, "system.disk.inodes.usage") {
		t.Error("no inode usage series")
	}
}

// One unreadable mount must not discard every other filesystem on the host.
func TestDiskSkipsUnreadableMounts(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	p.parts = []Partition{
		{Device: "/dev/sda1", Mountpoint: "/", Fstype: "ext4"},
		{Device: "nfs:/vol", Mountpoint: "/mnt/wedged", Fstype: "nfs"},
	}
	p.usageErr["/mnt/wedged"] = errors.New("permission denied")

	samples, err := newDiskCollector(p, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if !hasMetric(samples, "system.disk.usage") {
		t.Fatal("the readable filesystem was lost")
	}
	for _, s := range samples {
		if s.Labels["mountpoint"] == "/mnt/wedged" {
			t.Error("emitted samples for a filesystem that could not be read")
		}
	}
}

// A zero-capacity mount would divide by zero and produce NaN, which the
// pipeline rejects — losing the whole batch rather than one value.
func TestDiskSkipsZeroCapacityMounts(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	p.parts = append(p.parts, Partition{Device: "none", Mountpoint: "/empty", Fstype: "tmpfs"})
	p.usage["/empty"] = DiskUsage{Path: "/empty", Total: 0}

	samples, err := newDiskCollector(p, nil).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertAllValid(t, samples)
	for _, s := range samples {
		if s.Labels["mountpoint"] == "/empty" {
			t.Error("emitted samples for a zero-capacity mount")
		}
	}
}

func TestDiskIOEmitsRatesNotTotals(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	c := newDiskIOCollector(p)
	ctx := context.Background()

	// The first run has nothing to compare against, so it must emit nothing
	// rather than report the cumulative total as though it were a rate.
	first, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 0 {
		t.Errorf("first run emitted %d samples; there is no rate to report yet", len(first))
	}

	time.Sleep(20 * time.Millisecond)
	p.diskIO = []DiskIOCounters{
		{Device: "sda", ReadCount: 200, WriteCount: 400, ReadBytes: 3 << 20, WriteBytes: 6 << 20, IoTime: 5100},
	}

	second, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertAllValid(t, second)

	read := findSample(t, second, "system.diskio.read.bytes", map[string]string{"device": "sda"})
	if read.Value <= 0 {
		t.Errorf("read rate = %v, want positive", read.Value)
	}
	if read.Unit != collect.UnitBytesPerSecond {
		t.Errorf("read unit = %v, want bytes_per_second", read.Unit)
	}
	if !hasMetric(second, "system.diskio.utilisation") {
		t.Error("no utilisation series; it is the earliest saturation signal")
	}
}

func TestNetworkCollectorSkipsVirtualInterfaces(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	p.networkIO = []NetworkIOCounters{
		{Name: "eth0", BytesRecv: 100, BytesSent: 200},
		{Name: "lo", BytesRecv: 999, BytesSent: 999},
		{Name: "veth1a2b3c", BytesRecv: 50, BytesSent: 50},
		{Name: "docker0", BytesRecv: 10, BytesSent: 10},
		{Name: "br-abc123", BytesRecv: 10, BytesSent: 10},
	}
	c := newNetworkCollector(p)
	ctx := context.Background()

	if _, err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	for i := range p.networkIO {
		p.networkIO[i].BytesRecv += 1000
		p.networkIO[i].BytesSent += 1000
	}

	samples, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertAllValid(t, samples)

	for _, s := range samples {
		switch s.Labels["interface"] {
		case "lo", "veth1a2b3c", "docker0", "br-abc123":
			// A container host creates a veth per container; reporting them is
			// unbounded cardinality for signal already visible on the bridge.
			t.Errorf("emitted a series for virtual interface %q", s.Labels["interface"])
		}
	}
	if !hasMetric(samples, "system.network.rx.bytes") {
		t.Error("the physical interface produced no series")
	}
}

// Errors reported as a cumulative total draw a staircase that says nothing
// about whether the problem is happening now.
func TestNetworkEmitsErrorRates(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	p.networkIO = []NetworkIOCounters{{Name: "eth0", BytesRecv: 1000, BytesSent: 500, Errin: 5, Dropin: 3}}
	c := newNetworkCollector(p)
	ctx := context.Background()

	if _, err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	p.networkIO = []NetworkIOCounters{{Name: "eth0", BytesRecv: 9000, BytesSent: 4000, Errin: 25, Dropin: 13}}

	samples, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	errRate := findSample(t, samples, "system.network.rx.errors", map[string]string{"interface": "eth0"})
	if errRate.Value <= 0 {
		t.Errorf("error rate = %v, want positive", errRate.Value)
	}
}

func TestLoadCollectorNormalisesByCores(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	samples, err := newLoadCollector(p, 8).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertAllValid(t, samples)

	raw := findSample(t, samples, "system.load.average", map[string]string{"window": "1m"})
	if raw.Value != 1.5 {
		t.Errorf("load1 = %v, want 1.5", raw.Value)
	}

	// Raw load is not comparable across machines: 8 is idle on a 64-core host
	// and a crisis on a 2-core one.
	perCore := findSample(t, samples, "system.load.per_core", map[string]string{"window": "1m"})
	if want := 1.5 / 8; math.Abs(perCore.Value-want) > 1e-9 {
		t.Errorf("per-core load = %v, want %v", perCore.Value, want)
	}
}

func TestLoadCollectorOmitsPerCoreWhenCoreCountUnknown(t *testing.T) {
	t.Parallel()

	samples, err := newLoadCollector(newFakeProvider(), 0).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hasMetric(samples, "system.load.per_core") {
		t.Error("normalised load was emitted although the core count is unknown")
	}
	if !hasMetric(samples, "system.load.average") {
		t.Error("raw load average was lost")
	}
}

func TestHostCollectorRecordsFactsAndUptime(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	rec := &fakeRecorder{}
	c := newHostCollector(p, rec, nil, "node-1", nil)

	samples, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	assertAllValid(t, samples)

	uptime := findSample(t, samples, "system.uptime", nil)
	if uptime.Unit != collect.UnitSeconds {
		t.Errorf("uptime unit = %v, want seconds", uptime.Unit)
	}
	if uptime.Value < 71*3600 || uptime.Value > 73*3600 {
		t.Errorf("uptime = %v seconds, want roughly 72 hours", uptime.Value)
	}

	// Descriptive facts go to storage, not into a time series: a kernel
	// version is not something to record once a minute forever.
	if rec.nodeID != "node-1" {
		t.Errorf("recorded nodeID = %q", rec.nodeID)
	}
	if rec.facts.Kernel != "6.8.0" || rec.facts.OS != "linux" {
		t.Errorf("facts = %+v", rec.facts)
	}
	if rec.facts.Platform != "ubuntu 24.04" {
		t.Errorf("platform = %q, want the distribution and version", rec.facts.Platform)
	}
	if rec.facts.CPUCores != 8 {
		t.Errorf("cores = %d, want 8", rec.facts.CPUCores)
	}
	if rec.facts.HardwareUUID != "4c4c4544-0034-3910-8053-b4c04f303232" {
		t.Errorf("hardware uuid = %q, want it carried from HostInfo into NodeFacts", rec.facts.HardwareUUID)
	}
	if hasMetric(samples, "system.kernel") {
		t.Error("a descriptive fact was emitted as a time series")
	}
}

// A failure to persist facts must not cost the uptime series.
func TestHostCollectorSurvivesRecorderFailure(t *testing.T) {
	t.Parallel()

	rec := &fakeRecorder{err: errors.New("database unavailable")}
	c := newHostCollector(newFakeProvider(), rec, nil, "node-1", nil)

	samples, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if !hasMetric(samples, "system.uptime") {
		t.Error("uptime was lost because the recorder failed")
	}
}

func TestCollectorsHonourCancellation(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	p.blockFor = time.Hour

	collectors := map[string]collect.Collector{
		"cpu":     newCPUCollector(p),
		"memory":  newMemoryCollector(p),
		"disk":    newDiskCollector(p, nil),
		"diskio":  newDiskIOCollector(p),
		"network": newNetworkCollector(p),
		"load":    newLoadCollector(p, 8),
		"host":    newHostCollector(p, nil, nil, "node-1", nil),
	}

	for name, c := range collectors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()

			done := make(chan struct{})
			go func() {
				_, _ = c.Collect(ctx)
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(3 * time.Second):
				// A collector that ignores cancellation pins a scheduler slot
				// on a host with a wedged filesystem — exactly when monitoring
				// must keep working.
				t.Fatal("collector did not return when its context was cancelled")
			}
		})
	}
}

func TestAllDescriptorsAreValidAndUnique(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	collectors := []collect.Collector{
		newHostCollector(p, nil, nil, "node-1", nil),
		newCPUCollector(p),
		newMemoryCollector(p),
		newLoadCollector(p, 8),
		newDiskCollector(p, nil),
		newDiskIOCollector(p),
		newNetworkCollector(p),
	}

	seen := map[string]bool{}
	for _, c := range collectors {
		d := c.Descriptor()
		if err := d.Validate(); err != nil {
			t.Errorf("descriptor %q is invalid: %v", d.ID, err)
		}
		if seen[d.ID] {
			t.Errorf("duplicate collector id %q", d.ID)
		}
		seen[d.ID] = true
		if d.Name == "" || d.Description == "" {
			t.Errorf("collector %q has no name or description for the UI", d.ID)
		}
	}
}

type fakeRecorder struct {
	nodeID string
	facts  NodeFacts
	err    error
}

func (r *fakeRecorder) UpdateNodeFacts(_ context.Context, nodeID string, facts NodeFacts) error {
	r.nodeID, r.facts = nodeID, facts
	return r.err
}

// A host presents many interfaces that have never carried traffic — tunnels,
// AWDL, unused NICs. Each would otherwise consume eight series of permanent
// zeroes out of the collector's cardinality budget.
func TestNetworkSkipsInterfacesThatNeverCarriedTraffic(t *testing.T) {
	t.Parallel()

	p := newFakeProvider()
	p.networkIO = []NetworkIOCounters{
		{Name: "en0", BytesRecv: 1000, BytesSent: 500},
		{Name: "utun0"}, // never used
		{Name: "awdl0"}, // never used
		{Name: "gif0"},  // never used
	}
	c := newNetworkCollector(p)
	ctx := context.Background()

	if _, err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	p.networkIO[0].BytesRecv += 5000
	p.networkIO[0].BytesSent += 2000

	samples, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if iface := s.Labels["interface"]; iface != "en0" {
			t.Errorf("emitted a series for idle interface %q", iface)
		}
	}
	if !hasMetric(samples, "system.network.rx.bytes") {
		t.Error("the active interface produced no series")
	}
}
