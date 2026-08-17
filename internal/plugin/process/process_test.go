package process

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/plugin"
)

// The invariant this package exists to hold: no PID ever becomes a label.
//
// A PID is the obvious identifier and the wrong one. A busy host churns through
// thousands an hour, each producing a series written once and never again, and
// the damage is permanent — the samples are already stored by the time anyone
// notices the datastore growing. Every test below that asserts on labels is
// really asserting that.

func TestNoSampleIsLabelledByPID(t *testing.T) {
	provider := &fakeProvider{procs: manyDistinctPIDs(400)}

	samples, err := newProcessCollector(provider, 10, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for _, s := range samples {
		for key, value := range s.Labels {
			switch key {
			case "state", "process":
			default:
				t.Errorf("metric %s carries unexpected label %q", s.Metric, key)
			}
			// Belt and braces: even under an allowed key, a bare integer would
			// mean a PID leaked in through the name field.
			if _, err := strconv.Atoi(value); err == nil {
				t.Errorf("metric %s has numeric label %s=%q, which looks like a PID",
					s.Metric, key, value)
			}
		}
	}
}

func TestSeriesCountStaysBoundedAsPIDsChurn(t *testing.T) {
	// Four hundred processes, then four hundred entirely different ones, as
	// happens on a host running short-lived jobs. The number of distinct series
	// must not grow with the number of PIDs seen.
	c := newProcessCollector(&fakeProvider{procs: manyDistinctPIDs(400)}, 10, nil)

	first, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	c.provider = &fakeProvider{procs: manyDistinctPIDs(400)}
	second, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	series := map[string]bool{}
	for _, s := range append(first, second...) {
		series[seriesKey(s)] = true
	}

	// Six states + three totals + at most topN each of cpu/instances/memory.
	const ceiling = 6 + 3 + 10*3
	if len(series) > ceiling {
		t.Errorf("got %d distinct series across two sweeps, want at most %d", len(series), ceiling)
	}
}

func TestTopConsumersAggregateByNameAcrossInstances(t *testing.T) {
	// A host running forty workers of one program should show one line for the
	// program, not forty that individually look idle.
	provider := &fakeProvider{procs: []Process{
		{PID: 1, Name: "php-fpm", CPUPercent: 3, MemoryRSS: 100, State: StateRunning},
		{PID: 2, Name: "php-fpm", CPUPercent: 4, MemoryRSS: 200, State: StateRunning},
		{PID: 3, Name: "php-fpm", CPUPercent: 5, MemoryRSS: 300, State: StateSleeping},
		{PID: 4, Name: "postgres", CPUPercent: 2, MemoryRSS: 5000, State: StateSleeping},
	}}

	samples, err := newProcessCollector(provider, 10, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	values := valuesByMetricAndLabel(samples, "process")

	if got, want := values["process.top.cpu"]["php-fpm"], 12.0; got != want {
		t.Errorf("php-fpm cpu = %v, want %v (summed across instances)", got, want)
	}
	if got, want := values["process.top.memory"]["php-fpm"], 600.0; got != want {
		t.Errorf("php-fpm memory = %v, want %v", got, want)
	}
	// The instance count is what tells an operator the CPU figure is a sum of
	// three, not one process at 12%.
	if got, want := values["process.instances"]["php-fpm"], 3.0; got != want {
		t.Errorf("php-fpm instances = %v, want %v", got, want)
	}
	if got, want := values["process.top.memory"]["postgres"], 5000.0; got != want {
		t.Errorf("postgres memory = %v, want %v", got, want)
	}
}

func TestTopConsumersHonourTheLimit(t *testing.T) {
	procs := make([]Process, 0, 50)
	for i := range 50 {
		procs = append(procs, Process{
			PID: int32(i + 1), Name: fmt.Sprintf("worker-%02d", i),
			CPUPercent: float64(i + 1), MemoryRSS: uint64(i+1) * 1024,
			State: StateRunning,
		})
	}

	samples, err := newProcessCollector(&fakeProvider{procs: procs}, 5, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	values := valuesByMetricAndLabel(samples, "process")

	if got := len(values["process.top.cpu"]); got != 5 {
		t.Errorf("got %d top-cpu series, want 5", got)
	}
	if got := len(values["process.top.memory"]); got != 5 {
		t.Errorf("got %d top-memory series, want 5", got)
	}
	// The heaviest, not an arbitrary five.
	if _, ok := values["process.top.cpu"]["worker-49"]; !ok {
		t.Error("the busiest process is missing from the top-cpu series")
	}
	if _, ok := values["process.top.cpu"]["worker-00"]; ok {
		t.Error("the idlest process appeared in the top-cpu series")
	}
}

func TestIdleProcessesGetNoSeries(t *testing.T) {
	// On a quiet host most processes sit at zero. Emitting a series for each
	// would spend the cardinality budget on processes that have nothing to say.
	provider := &fakeProvider{procs: []Process{
		{PID: 1, Name: "busy", CPUPercent: 10, MemoryRSS: 1024, State: StateRunning},
		{PID: 2, Name: "idle-a", CPUPercent: 0, MemoryRSS: 0, State: StateSleeping},
		{PID: 3, Name: "idle-b", CPUPercent: 0, MemoryRSS: 0, State: StateSleeping},
	}}

	samples, err := newProcessCollector(provider, 10, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	values := valuesByMetricAndLabel(samples, "process")
	if got := len(values["process.top.cpu"]); got != 1 {
		t.Errorf("got %d top-cpu series, want 1 — idle processes should be skipped", got)
	}
	if got := len(values["process.top.memory"]); got != 1 {
		t.Errorf("got %d top-memory series, want 1", got)
	}
}

func TestEveryStateIsEmittedIncludingZero(t *testing.T) {
	provider := &fakeProvider{procs: []Process{
		{PID: 1, Name: "a", State: StateRunning, Threads: 4},
		{PID: 2, Name: "b", State: StateSleeping, Threads: 2},
		{PID: 3, Name: "c", State: StateZombie},
		{PID: 4, Name: "d", State: StateZombie},
	}}

	samples, err := newProcessCollector(provider, 10, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	byState := map[string]float64{}
	totals := map[string]float64{}
	for _, s := range samples {
		if state, ok := s.Labels["state"]; ok {
			byState[state] = s.Value
			continue
		}
		if len(s.Labels) == 0 {
			totals[s.Metric] = s.Value
		}
	}

	// A count falling to zero must show as zero, not as the series vanishing —
	// which is indistinguishable from the collector having stopped.
	for _, state := range []State{StateRunning, StateSleeping, StateStopped, StateZombie, StateIdle, StateOther} {
		if _, ok := byState[string(state)]; !ok {
			t.Errorf("state %q not emitted", state)
		}
	}
	if byState["stopped"] != 0 {
		t.Errorf("stopped = %v, want 0", byState["stopped"])
	}

	if totals["process.total"] != 4 {
		t.Errorf("process.total = %v, want 4", totals["process.total"])
	}
	if totals["process.threads"] != 6 {
		t.Errorf("process.threads = %v, want 6", totals["process.threads"])
	}
	// Zombies get a metric of their own: a rising count means a parent is not
	// reaping children, which ends in PID exhaustion and deserves its own alert
	// rather than being one label value among six.
	if totals["process.zombies"] != 2 {
		t.Errorf("process.zombies = %v, want 2", totals["process.zombies"])
	}
}

func TestUnnamedProcessesAreSkipped(t *testing.T) {
	// A process that exits mid-read can come back with an empty name. An empty
	// label is worse than no series — it is a series nobody can interpret.
	provider := &fakeProvider{procs: []Process{
		{PID: 1, Name: "", CPUPercent: 90, MemoryRSS: 1 << 30, State: StateRunning},
		{PID: 2, Name: "real", CPUPercent: 1, MemoryRSS: 1024, State: StateRunning},
	}}

	samples, err := newProcessCollector(provider, 10, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for _, s := range samples {
		if name, ok := s.Labels["process"]; ok && name == "" {
			t.Errorf("metric %s emitted with an empty process label", s.Metric)
		}
	}
	// The unnamed process still counts towards the totals — it exists.
	values := valuesByMetricAndLabel(samples, "process")
	if len(values["process.top.cpu"]) != 1 {
		t.Errorf("got %d top-cpu series, want 1", len(values["process.top.cpu"]))
	}
}

func TestInventorySortsByLoadAndHonoursTheLimit(t *testing.T) {
	p := &Plugin{provider: &fakeProvider{procs: []Process{
		{PID: 1, Name: "quiet", CPUPercent: 0, MemoryRSS: 100},
		{PID: 2, Name: "hot", CPUPercent: 80, MemoryRSS: 50},
		{PID: 3, Name: "warm", CPUPercent: 20, MemoryRSS: 10},
		// Equal CPU, so memory breaks the tie — otherwise the order flips
		// between refreshes and the list appears to jump around.
		{PID: 4, Name: "big", CPUPercent: 0, MemoryRSS: 9999},
	}}}
	p.settings = Settings{InventoryLimit: 3}

	procs, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}

	if len(procs) != 3 {
		t.Fatalf("got %d processes, want 3 (the limit)", len(procs))
	}
	want := []string{"hot", "warm", "big"}
	for i, name := range want {
		if procs[i].Name != name {
			t.Errorf("position %d = %q, want %q", i, procs[i].Name, name)
		}
	}
}

func TestDetectFailsWhenEnumerationIsRefused(t *testing.T) {
	// Every host has processes, so an empty list means the enumeration is being
	// refused — a hardened container or a restricted sandbox. Reporting the
	// plugin as active there would show "0 processes", which reads as a claim
	// about the host rather than about Atlas's own visibility.
	p := New(Options{Provider: &fakeProvider{}})

	ok, err := p.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if ok {
		t.Error("Detect true with no visible processes, want false")
	}

	p = New(Options{Provider: &fakeProvider{err: errors.New("permission denied")}})
	if _, err := p.Detect(context.Background()); err == nil {
		t.Error("Detect swallowed a provider error")
	}
}

func TestSettingsDefaults(t *testing.T) {
	var s Settings
	if got := s.topN(); got != 10 {
		t.Errorf("default topN = %d, want 10", got)
	}
	if got := s.inventoryLimit(); got != 500 {
		t.Errorf("default inventoryLimit = %d, want 500", got)
	}

	s = Settings{TopN: 3, InventoryLimit: 50}
	if got := s.topN(); got != 3 {
		t.Errorf("topN = %d, want 3", got)
	}
	if got := s.inventoryLimit(); got != 50 {
		t.Errorf("inventoryLimit = %d, want 50", got)
	}
}

func TestInitAppliesConfiguredTopN(t *testing.T) {
	p := New(Options{Provider: &fakeProvider{procs: manyDistinctPIDs(100)}})

	err := p.Init(context.Background(), plugin.Env{
		Config: func(target any) error {
			settings, ok := target.(*Settings)
			if !ok {
				return fmt.Errorf("unexpected target %T", target)
			}
			settings.TopN = 3
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	collectors := p.Collectors()
	if len(collectors) != 1 {
		t.Fatalf("got %d collectors, want 1", len(collectors))
	}

	samples, err := collectors[0].Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := len(valuesByMetricAndLabel(samples, "process")["process.top.cpu"]); got != 3 {
		t.Errorf("got %d top-cpu series, want the configured 3", got)
	}
}

func TestCollectorDescriptorLeavesHeadroom(t *testing.T) {
	// Enumerating processes is thousands of small reads. The timeout must sit
	// inside the interval, or a busy host stacks sweeps it cannot finish — and
	// Atlas becomes the load it was installed to measure.
	d := newProcessCollector(&fakeProvider{}, 10, nil).Descriptor()
	if d.Timeout >= d.Interval {
		t.Errorf("timeout %v >= interval %v", d.Timeout, d.Interval)
	}
}

func TestRunningFor(t *testing.T) {
	if got := (Process{}).RunningFor(); got != 0 {
		t.Errorf("RunningFor with no creation time = %v, want 0", got)
	}
	p := Process{CreatedAt: time.Now().Add(-time.Hour)}
	if got := p.RunningFor(); got < 59*time.Minute || got > 61*time.Minute {
		t.Errorf("RunningFor = %v, want about an hour", got)
	}
}

// manyDistinctPIDs builds processes with unique PIDs but a small set of names,
// which is what a real host looks like: PIDs churn, names repeat.
func manyDistinctPIDs(n int) []Process {
	names := []string{"nginx", "postgres", "php-fpm", "node", "sshd", "systemd"}
	procs := make([]Process, 0, n)
	for i := range n {
		procs = append(procs, Process{
			// Deliberately unique and never previously seen, so a PID label
			// would show up as unbounded growth.
			PID:        int32(100000 + i),
			Name:       names[i%len(names)],
			CPUPercent: float64(i % 17),
			MemoryRSS:  uint64(i%23) * 1024 * 1024,
			Threads:    int32(i%8 + 1),
			State:      StateSleeping,
		})
	}
	return procs
}

func valuesByMetricAndLabel(samples []collect.Sample, label string) map[string]map[string]float64 {
	out := map[string]map[string]float64{}
	for _, s := range samples {
		value, ok := s.Labels[label]
		if !ok {
			continue
		}
		if out[s.Metric] == nil {
			out[s.Metric] = map[string]float64{}
		}
		out[s.Metric][value] = s.Value
	}
	return out
}

func seriesKey(s collect.Sample) string {
	key := s.Metric
	for _, label := range []string{"state", "process"} {
		if value, ok := s.Labels[label]; ok {
			key += "|" + label + "=" + value
		}
	}
	return key
}

// fakeProvider returns processes a test set. The situations that matter — a
// host churning through PIDs, a process that vanishes mid-read, an enumeration
// that is refused outright — cannot be produced on demand against a real
// machine, and are exactly the ones where a collector misbehaves.
type fakeProvider struct {
	procs []Process
	err   error
}

func (f *fakeProvider) Processes(context.Context) ([]Process, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]Process(nil), f.procs...), nil
}

var _ Provider = (*fakeProvider)(nil)

// A command line routinely carries a credential passed as an argument. The
// default must withhold it, and must keep everything else that makes the
// process identifiable.
func TestInventoryRedactsCommandLineSecretsByDefault(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{procs: []Process{
		{PID: 1, Name: "mysqldump", Cmdline: "mysqldump -uroot -pSuperSecret atlas"},
		{PID: 2, Name: "java", Cmdline: "java -Xmx4g -jar app.jar --port 8080"},
	}}
	p := New(Options{Provider: provider})

	got, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}

	var mysqldump, java Process
	for _, proc := range got {
		switch proc.Name {
		case "mysqldump":
			mysqldump = proc
		case "java":
			java = proc
		}
	}

	if strings.Contains(mysqldump.Cmdline, "SuperSecret") {
		t.Errorf("cmdline = %q, still carries the password", mysqldump.Cmdline)
	}
	if !strings.Contains(mysqldump.Cmdline, "mysqldump") || !strings.Contains(mysqldump.Cmdline, "-uroot") {
		t.Errorf("cmdline = %q, want the process still identifiable", mysqldump.Cmdline)
	}
	if java.Cmdline != "java -Xmx4g -jar app.jar --port 8080" {
		t.Errorf("cmdline = %q, want ordinary arguments untouched", java.Cmdline)
	}
}

func TestInventoryRedactionCanBeDisabledExplicitly(t *testing.T) {
	t.Parallel()

	const raw = "mysqldump -uroot -pSuperSecret atlas"
	provider := &fakeProvider{procs: []Process{{PID: 1, Name: "mysqldump", Cmdline: raw}}}
	p := New(Options{Provider: provider, DisableSecretRedaction: true})

	got, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if got[0].Cmdline != raw {
		t.Errorf("cmdline = %q, want %q when redaction is explicitly disabled", got[0].Cmdline, raw)
	}
}
