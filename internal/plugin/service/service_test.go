package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/core/collect"
)

// systemctl's output format is the plugin's real interface, and it is not a
// stable one: columns shift between systemd versions, a failed unit may carry a
// bullet even with --plain, and `show` reports "[not set]" and "infinity" where
// a number is expected. Parsing it wrong does not fail loudly — it reports a
// running service as inactive, or a memory figure of zero, which is exactly the
// kind of quiet wrongness that makes a monitoring tool worse than none.

const listUnitsOutput = `sshd.service                loaded active   running OpenBSD Secure Shell server
nginx.service               loaded active   running A high performance web server
● postfix.service           loaded failed   failed  Postfix Mail Transport Agent
apt-daily.service           loaded inactive dead    Daily apt download activities
logrotate.service           loaded active   exited  Rotate log files
systemd-tmpfiles-clean.timer loaded active waiting File System Cleanup
`

func TestParseListUnits(t *testing.T) {
	units := parseListUnits(listUnitsOutput)

	// The .timer line is not a service and must not appear, even though
	// --type=service should have excluded it upstream.
	if len(units) != 5 {
		t.Fatalf("got %d units, want 5: %+v", len(units), units)
	}

	byName := map[string]Unit{}
	for _, u := range units {
		byName[u.Name] = u
	}

	if _, ok := byName["systemd-tmpfiles-clean.timer"]; ok {
		t.Error("a .timer was parsed as a service")
	}

	// The bullet some systemd versions prefix to a failed unit must not become
	// part of the unit name, or the unit is unmatchable by every later lookup.
	postfix, ok := byName["postfix.service"]
	if !ok {
		t.Fatalf("postfix.service missing; got names %v", names(units))
	}
	if !postfix.Failed() {
		t.Errorf("postfix ActiveState = %q, want failed", postfix.ActiveState)
	}

	sshd := byName["sshd.service"]
	if !sshd.Running() {
		t.Error("sshd not reported as running")
	}
	if got, want := sshd.Description, "OpenBSD Secure Shell server"; got != want {
		t.Errorf("description = %q, want %q", got, want)
	}

	// A oneshot unit that finished is active but not running. Collapsing the
	// two would make a completed log rotation look like a live daemon.
	logrotate := byName["logrotate.service"]
	if logrotate.ActiveState != ActiveStateActive {
		t.Errorf("logrotate ActiveState = %q, want active", logrotate.ActiveState)
	}
	if logrotate.Running() {
		t.Error("an active (exited) oneshot reported as running")
	}
}

func TestNormaliseActiveStateRejectsUnknownValues(t *testing.T) {
	// A future systemd state must land in "unknown" rather than being counted
	// as one of the known states, which would silently misreport it.
	if got := normaliseActiveState("reloading"); got != ActiveStateUnknown {
		t.Errorf("normaliseActiveState(reloading) = %q, want unknown", got)
	}
	if got := normaliseActiveState("ACTIVE"); got != ActiveStateActive {
		t.Errorf("normaliseActiveState(ACTIVE) = %q, want active", got)
	}
}

func TestApplyPropertiesHandlesSystemdSentinels(t *testing.T) {
	tests := []struct {
		name  string
		props map[string]string
		want  Unit
	}{
		{
			name: "populated",
			props: map[string]string{
				"Id": "nginx.service", "MainPID": "1234", "NRestarts": "3",
				"MemoryCurrent": "52428800", "CPUUsageNSec": "1500000000",
				"UnitFileState": "enabled",
			},
			want: Unit{MainPID: 1234, RestartCount: 3, MemoryBytes: 52428800, CPUSeconds: 1.5, Enabled: true},
		},
		{
			// systemd reports these for unlimited or unavailable values. Both
			// must leave the field zero rather than producing a garbage number.
			name: "sentinels",
			props: map[string]string{
				"Id": "app.service", "MainPID": "0", "NRestarts": "[not set]",
				"MemoryCurrent": "infinity", "CPUUsageNSec": "[not set]",
				"UnitFileState": "disabled",
			},
			want: Unit{},
		},
		{
			// A unit that is running but not enabled vanishes on the next
			// reboot — an outage invisible until it happens.
			name:  "enabled-runtime counts as enabled",
			props: map[string]string{"Id": "x.service", "UnitFileState": "enabled-runtime"},
			want:  Unit{Enabled: true},
		},
		{
			name:  "static is not enabled",
			props: map[string]string{"Id": "x.service", "UnitFileState": "static"},
			want:  Unit{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var u Unit
			applyProperties(&u, tc.props)

			if u.MainPID != tc.want.MainPID {
				t.Errorf("MainPID = %d, want %d", u.MainPID, tc.want.MainPID)
			}
			if u.RestartCount != tc.want.RestartCount {
				t.Errorf("RestartCount = %d, want %d", u.RestartCount, tc.want.RestartCount)
			}
			if u.MemoryBytes != tc.want.MemoryBytes {
				t.Errorf("MemoryBytes = %d, want %d", u.MemoryBytes, tc.want.MemoryBytes)
			}
			if u.CPUSeconds != tc.want.CPUSeconds {
				t.Errorf("CPUSeconds = %v, want %v", u.CPUSeconds, tc.want.CPUSeconds)
			}
			if u.Enabled != tc.want.Enabled {
				t.Errorf("Enabled = %v, want %v", u.Enabled, tc.want.Enabled)
			}
		})
	}
}

func TestParseSystemdTime(t *testing.T) {
	tests := []struct {
		in   string
		zero bool
	}{
		{in: "Tue 2026-08-04 09:15:32 UTC"},
		{in: "2026-08-04 09:15:32 UTC"},
		{in: "Tue 2026-08-04 09:15:32"},
		// "n/a" is what systemd reports for a unit that has never been active.
		// Parsing it as a real timestamp would produce an uptime of decades.
		{in: "n/a", zero: true},
		{in: "", zero: true},
		{in: "not a timestamp", zero: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := parseSystemdTime(tc.in)
			if got.IsZero() != tc.zero {
				t.Errorf("parseSystemdTime(%q) = %v, want zero=%v", tc.in, got, tc.zero)
			}
		})
	}
}

func TestUnitsEnrichesWithASingleShowCall(t *testing.T) {
	// One `systemctl show` for every unit rather than one per unit: on a host
	// with two hundred services that is one process spawn instead of two
	// hundred, and it is the reason this collector can run at all.
	var showCalls int

	p := &systemdProvider{
		systemctl: "systemctl",
		run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			switch args[0] {
			case "list-units":
				return []byte(listUnitsOutput), nil
			case "show":
				showCalls++
				return []byte(strings.Join([]string{
					"Id=sshd.service\nMainPID=900\nNRestarts=0\nMemoryCurrent=8388608\nUnitFileState=enabled\nActiveEnterTimestamp=Tue 2026-08-04 09:15:32 UTC",
					"Id=nginx.service\nMainPID=1100\nNRestarts=2\nMemoryCurrent=52428800\nUnitFileState=enabled\nActiveEnterTimestamp=n/a",
					"Id=postfix.service\nMainPID=0\nNRestarts=5\nMemoryCurrent=[not set]\nUnitFileState=enabled\nActiveEnterTimestamp=n/a",
				}, "\n\n")), nil
			}
			return nil, errors.New("unexpected command")
		},
	}

	units, err := p.Units(context.Background())
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	if showCalls != 1 {
		t.Errorf("systemctl show called %d times, want 1", showCalls)
	}

	byName := map[string]Unit{}
	for _, u := range units {
		byName[u.Name] = u
	}

	if got := byName["nginx.service"].RestartCount; got != 2 {
		t.Errorf("nginx restarts = %d, want 2", got)
	}
	if got := byName["sshd.service"].MemoryBytes; got != 8388608 {
		t.Errorf("sshd memory = %d, want 8388608", got)
	}
	// Units absent from the show output keep their listing state rather than
	// being dropped.
	if _, ok := byName["apt-daily.service"]; !ok {
		t.Error("a unit missing from `show` was dropped from the listing")
	}
}

func TestUnitsReturnsTheListingWhenEnrichmentFails(t *testing.T) {
	// The listing carries the states an operator most needs. Losing all of it
	// because one optional command failed would turn a partial outage in
	// Atlas's own data collection into a total one.
	p := &systemdProvider{
		systemctl: "systemctl",
		run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "list-units" {
				return []byte(listUnitsOutput), nil
			}
			return nil, errors.New("show failed")
		},
	}

	units, err := p.Units(context.Background())
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	if len(units) != 5 {
		t.Fatalf("got %d units, want 5", len(units))
	}
	for _, u := range units {
		if u.Name == "postfix.service" && !u.Failed() {
			t.Error("failed state lost when enrichment failed")
		}
	}
}

func TestAvailableIsFalseWithoutSystemd(t *testing.T) {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		t.Skip("this host is systemd-booted")
	}

	// systemctl can be installed in a container that systemd does not manage,
	// where every command fails with a confusing error. The marker directory is
	// the reliable signal, so the binary must not even be consulted.
	ran := false
	p := &systemdProvider{
		systemctl: "systemctl",
		run: func(context.Context, string, ...string) ([]byte, error) {
			ran = true
			return nil, nil
		},
	}

	if p.Available(context.Background()) {
		t.Error("Available on a host without systemd, want false")
	}
	if ran {
		t.Error("systemctl was run on a host with no systemd marker directory")
	}
}

func TestAvailableToleratesDegraded(t *testing.T) {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("this host is not systemd-booted")
	}

	// `is-system-running` exits non-zero for "degraded" — a running system with
	// a failed unit, which is precisely the case worth monitoring. Treating
	// that exit code as unavailable would disable the plugin exactly when it
	// has something to report.
	p := &systemdProvider{
		systemctl: "systemctl",
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("degraded\n"), &exec.ExitError{}
		},
	}

	if !p.Available(context.Background()) {
		t.Error("Available on a degraded system, want true")
	}
}

func TestCollectorGivesPerUnitSeriesOnlyToFailedAndWatchedUnits(t *testing.T) {
	// The cardinality trade. A host can have three hundred units, and a
	// permanent series per unit saying "still fine" is a great deal of storage
	// for very little signal — so only failures and units an operator named
	// get their own series.
	provider := &fakeProvider{units: []Unit{
		{Name: "sshd.service", ActiveState: ActiveStateActive, SubState: "running", Enabled: true, MemoryBytes: 4096},
		{Name: "postfix.service", ActiveState: ActiveStateFailed, SubState: "failed", RestartCount: 5},
		{Name: "packagekit.service", ActiveState: ActiveStateActive, SubState: "running", MemoryBytes: 999},
		{Name: "man-db.service", ActiveState: ActiveStateInactive, SubState: "dead"},
	}}

	c := newServiceCollector(provider, []string{"sshd.service"}, nil)
	samples, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	perUnit := map[string]bool{}
	for _, s := range samples {
		if unit, ok := s.Labels["unit"]; ok {
			perUnit[unit] = true
		}
	}

	if !perUnit["sshd.service"] {
		t.Error("watched unit has no per-unit series")
	}
	if !perUnit["postfix.service"] {
		t.Error("failed unit has no per-unit series")
	}
	if perUnit["packagekit.service"] || perUnit["man-db.service"] {
		t.Errorf("healthy unwatched units got per-unit series: %v", perUnit)
	}
}

func TestCollectorEmitsEveryStateIncludingZero(t *testing.T) {
	provider := &fakeProvider{units: []Unit{
		{Name: "a.service", ActiveState: ActiveStateActive, SubState: "running"},
		{Name: "b.service", ActiveState: ActiveStateActive, SubState: "running"},
		{Name: "c.service", ActiveState: ActiveStateFailed, SubState: "failed"},
	}}

	samples, err := newServiceCollector(provider, nil, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	values := map[string]float64{}
	for _, s := range samples {
		key := s.Metric
		if state, ok := s.Labels["state"]; ok {
			key += "{" + state + "}"
		}
		if _, isUnit := s.Labels["unit"]; isUnit {
			continue
		}
		values[key] = s.Value
	}

	want := map[string]float64{
		"service.total":         3,
		"service.failed":        1,
		"service.count{active}": 2,
		"service.count{failed}": 1,
		// Emitted at zero, so a chart shows a count falling to zero rather than
		// the series simply disappearing — which is indistinguishable from the
		// collector having stopped.
		"service.count{inactive}":     0,
		"service.count{activating}":   0,
		"service.count{deactivating}": 0,
	}
	for metric, wantValue := range want {
		got, ok := values[metric]
		if !ok {
			t.Errorf("%s not emitted", metric)
			continue
		}
		if got != wantValue {
			t.Errorf("%s = %v, want %v", metric, got, wantValue)
		}
	}
}

func TestCollectorReportsEnabledSeparatelyFromRunning(t *testing.T) {
	provider := &fakeProvider{units: []Unit{
		{Name: "nginx.service", ActiveState: ActiveStateActive, SubState: "running", Enabled: false},
	}}

	samples, err := newServiceCollector(provider, []string{"nginx.service"}, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	byMetric := map[string]float64{}
	for _, s := range samples {
		if s.Labels["unit"] == "nginx.service" {
			byMetric[s.Metric] = s.Value
		}
	}

	if byMetric["service.up"] != 1 {
		t.Errorf("service.up = %v, want 1", byMetric["service.up"])
	}
	// Running but not enabled: the service is up now and will be gone after
	// the next reboot. Reporting only "up" would hide that entirely.
	if byMetric["service.enabled"] != 0 {
		t.Errorf("service.enabled = %v, want 0", byMetric["service.enabled"])
	}
}

func TestInventorySortsFailedUnitsFirst(t *testing.T) {
	// A failed unit is the reason someone opened the page. It should not be
	// three hundred alphabetical entries down.
	p := &Plugin{provider: &fakeProvider{units: []Unit{
		{Name: "apache2.service", ActiveState: ActiveStateActive, SubState: "running"},
		{Name: "zzz.service", ActiveState: ActiveStateFailed, SubState: "failed"},
		{Name: "bbb.service", ActiveState: ActiveStateActive, SubState: "running"},
		{Name: "aaa-failing.service", ActiveState: ActiveStateFailed, SubState: "failed"},
	}}}

	units, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}

	if !units[0].Failed() || !units[1].Failed() {
		t.Fatalf("failed units not sorted first: %v", names(units))
	}
	// Within each group, alphabetical, so the list is stable between refreshes.
	if units[0].Name != "aaa-failing.service" || units[1].Name != "zzz.service" {
		t.Errorf("failed units not alphabetical: %v", names(units))
	}
	if units[2].Name != "apache2.service" {
		t.Errorf("healthy units not alphabetical: %v", names(units))
	}
}

func TestCollectorDescriptorLeavesHeadroom(t *testing.T) {
	// Every run spawns systemctl twice. The timeout must sit inside the
	// interval, or a slow host stacks collections it can never finish.
	d := newServiceCollector(&fakeProvider{}, nil, nil).Descriptor()
	if d.Timeout >= d.Interval {
		t.Errorf("timeout %v >= interval %v", d.Timeout, d.Interval)
	}
}

func names(units []Unit) []string {
	out := make([]string, 0, len(units))
	for _, u := range units {
		out = append(out, u.Name)
	}
	return out
}

// fakeProvider returns units a test set.
type fakeProvider struct {
	units     []Unit
	structure Structure
	err       error
	structErr error
	available bool
}

func (f *fakeProvider) Available(context.Context) bool { return f.available }

func (f *fakeProvider) Units(context.Context) ([]Unit, error) {
	if f.err != nil {
		return nil, f.err
	}
	// Returned by value so a collector cannot mutate the fixture between
	// assertions.
	return append([]Unit(nil), f.units...), nil
}

func (f *fakeProvider) Structure(context.Context) (Structure, error) {
	if f.structErr != nil {
		return Structure{}, f.structErr
	}
	return f.structure, nil
}

var (
	_ Provider          = (*fakeProvider)(nil)
	_ collect.Collector = (*serviceCollector)(nil)
)
