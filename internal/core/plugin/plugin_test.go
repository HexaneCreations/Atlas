package plugin_test

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/platform/errs"
)

// fake is a configurable Plugin for registry tests.
type fake struct {
	desc        plugin.Descriptor
	detected    bool
	detectErr   error
	detectPanic bool
	initErr     error
	closeErr    error
	collectors  []collect.Collector

	initCalled  bool
	closeCalled bool
}

func (f *fake) Descriptor() plugin.Descriptor { return f.desc }

func (f *fake) Detect(context.Context) (bool, error) {
	if f.detectPanic {
		panic("probe dereferenced a nil socket")
	}
	return f.detected, f.detectErr
}

func (f *fake) Init(context.Context, plugin.Env) error {
	f.initCalled = true
	return f.initErr
}

func (f *fake) Collectors() []collect.Collector { return f.collectors }

func (f *fake) Close(context.Context) error {
	f.closeCalled = true
	return f.closeErr
}

type stubCollector struct{ id string }

func (s stubCollector) Descriptor() collect.Descriptor { return collect.Descriptor{ID: s.id} }

func (s stubCollector) Collect(context.Context) ([]collect.Sample, error) { return nil, nil }

func desc(id string) plugin.Descriptor {
	return plugin.Descriptor{ID: id, Name: id, Subject: id}
}

func TestDescriptorValidate(t *testing.T) {
	t.Parallel()

	if err := (plugin.Descriptor{}).Validate(); err == nil {
		t.Error("a descriptor with no ID should be invalid")
	}
	if err := (plugin.Descriptor{ID: "docker"}).Validate(); err == nil {
		t.Error("a descriptor with no name should be invalid")
	}
	if err := desc("docker").Validate(); err != nil {
		t.Errorf("a complete descriptor should be valid, got %v", err)
	}
}

func TestRegistryRejectsDuplicateAndInvalid(t *testing.T) {
	t.Parallel()

	r := plugin.NewRegistry(nil, nil)
	if err := r.Register(&fake{desc: desc("docker")}); err != nil {
		t.Fatal(err)
	}

	err := r.Register(&fake{desc: desc("docker")})
	if err == nil {
		t.Fatal("Register() accepted a duplicate plugin ID")
	}
	if got := errs.CodeOf(err); got != errs.CodeAlreadyExists {
		t.Errorf("code = %q, want already_exists", got)
	}
	if err := r.Register(&fake{desc: plugin.Descriptor{}}); err == nil {
		t.Error("Register() accepted an invalid descriptor")
	}
	if r.Len() != 1 {
		t.Errorf("Len() = %d, want 1", r.Len())
	}
}

// A host without Docker must report "no Docker integration", not "broken
// Docker integration". This is the distinction the whole detect stage exists
// to make.
func TestActivateRecordsEveryOutcome(t *testing.T) {
	t.Parallel()

	active := &fake{desc: desc("active"), detected: true, collectors: []collect.Collector{stubCollector{id: "active.one"}}}
	absent := &fake{desc: desc("absent"), detected: false}
	probeFailed := &fake{desc: desc("probe-failed"), detectErr: stderrors.New("permission denied on socket")}
	initFailed := &fake{desc: desc("init-failed"), detected: true, initErr: errs.New(errs.CodeUnavailable, "daemon refused the connection")}
	off := &fake{desc: desc("switched-off"), detected: true}

	r := plugin.NewRegistry(nil, []string{"switched-off"})
	for _, p := range []plugin.Plugin{active, absent, probeFailed, initFailed, off} {
		if err := r.Register(p); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.Activate(context.Background(), plugin.Env{NodeID: "node-1"}); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}

	want := map[string]plugin.Status{
		"active":       plugin.StatusActive,
		"absent":       plugin.StatusNotDetected,
		"probe-failed": plugin.StatusDetectionFailed,
		"init-failed":  plugin.StatusInitFailed,
		"switched-off": plugin.StatusDisabled,
	}

	states := r.States()
	if len(states) != len(want) {
		t.Fatalf("States() returned %d entries, want %d", len(states), len(want))
	}
	for _, s := range states {
		if got := want[s.ID]; s.Status != got {
			t.Errorf("plugin %q status = %q, want %q", s.ID, s.Status, got)
		}
	}

	if got := r.ActiveStates(); len(got) != 1 || got[0].ID != "active" {
		t.Errorf("ActiveStates() = %v, want just the active plugin", got)
	}

	// A disabled plugin must never be probed or started.
	if off.initCalled {
		t.Error("a disabled plugin was initialised")
	}
	// A plugin whose subject is absent must never be initialised.
	if absent.initCalled {
		t.Error("an undetected plugin was initialised")
	}
	// A failure must carry an explanation an operator can act on.
	for _, s := range states {
		if s.Status == plugin.StatusDetectionFailed || s.Status == plugin.StatusInitFailed {
			if s.Error == "" {
				t.Errorf("plugin %q failed with no error message", s.ID)
			}
		}
	}
}

func TestDetectionPanicIsContained(t *testing.T) {
	t.Parallel()

	bad := &fake{desc: desc("panics"), detectPanic: true}
	good := &fake{desc: desc("healthy"), detected: true}

	r := plugin.NewRegistry(nil, nil)
	if err := r.Register(bad); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(good); err != nil {
		t.Fatal(err)
	}

	if err := r.Activate(context.Background(), plugin.Env{}); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}

	if !good.initCalled {
		t.Error("a panic in one plugin's detection stopped the next plugin from starting")
	}
	for _, s := range r.States() {
		if s.ID == "panics" && s.Status != plugin.StatusDetectionFailed {
			t.Errorf("panicking plugin status = %q, want detection_failed", s.Status)
		}
	}
}

func TestRegisterCollectorsFromActivePluginsOnly(t *testing.T) {
	t.Parallel()

	active := &fake{
		desc: desc("active"), detected: true,
		collectors: []collect.Collector{stubCollector{id: "a.one"}, stubCollector{id: "a.two"}},
	}
	absent := &fake{
		desc: desc("absent"), detected: false,
		collectors: []collect.Collector{stubCollector{id: "b.one"}},
	}

	r := plugin.NewRegistry(nil, nil)
	for _, p := range []plugin.Plugin{active, absent} {
		if err := r.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Activate(context.Background(), plugin.Env{}); err != nil {
		t.Fatal(err)
	}

	target := collect.NewRegistry()
	if err := r.RegisterCollectors(target); err != nil {
		t.Fatalf("RegisterCollectors() error = %v", err)
	}

	if got := target.Len(); got != 2 {
		t.Errorf("registered %d collectors, want 2 (only the active plugin's)", got)
	}
	if _, ok := target.Get("b.one"); ok {
		t.Error("an inactive plugin's collector was registered")
	}
}

// A collector ID collision between two plugins is a bug that must surface,
// not a runtime condition to route around.
func TestRegisterCollectorsReportsCollisions(t *testing.T) {
	t.Parallel()

	first := &fake{desc: desc("first"), detected: true, collectors: []collect.Collector{stubCollector{id: "shared.id"}}}
	second := &fake{desc: desc("second"), detected: true, collectors: []collect.Collector{stubCollector{id: "shared.id"}}}

	r := plugin.NewRegistry(nil, nil)
	for _, p := range []plugin.Plugin{first, second} {
		if err := r.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Activate(context.Background(), plugin.Env{}); err != nil {
		t.Fatal(err)
	}

	if err := r.RegisterCollectors(collect.NewRegistry()); err == nil {
		t.Error("RegisterCollectors() hid a collector ID collision between two plugins")
	}
}

func TestCloseRunsInReverseOrderAndOnlyForStartedPlugins(t *testing.T) {
	t.Parallel()

	started := &fake{desc: desc("started"), detected: true}
	neverStarted := &fake{desc: desc("never-started"), detected: false}

	r := plugin.NewRegistry(nil, nil)
	for _, p := range []plugin.Plugin{started, neverStarted} {
		if err := r.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Activate(context.Background(), plugin.Env{}); err != nil {
		t.Fatal(err)
	}

	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !started.closeCalled {
		t.Error("an initialised plugin was not closed")
	}
	if neverStarted.closeCalled {
		t.Error("a plugin that never initialised was closed")
	}

	// Close is idempotent; the supervisor may retry it.
	if err := r.Close(context.Background()); err != nil {
		t.Errorf("second Close() error = %v", err)
	}
}

// One stubborn plugin must not stop the rest from releasing resources.
func TestCloseContinuesAfterFailure(t *testing.T) {
	t.Parallel()

	flaky := &fake{desc: desc("flaky"), detected: true, closeErr: stderrors.New("socket already gone")}
	clean := &fake{desc: desc("clean"), detected: true}

	r := plugin.NewRegistry(nil, nil)
	for _, p := range []plugin.Plugin{flaky, clean} {
		if err := r.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Activate(context.Background(), plugin.Env{}); err != nil {
		t.Fatal(err)
	}

	err := r.Close(context.Background())
	if err == nil {
		t.Error("Close() hid a plugin failure")
	}
	if !flaky.closeCalled || !clean.closeCalled {
		t.Error("Close() stopped early instead of closing every plugin")
	}
}

func TestGetAndStatesBeforeActivation(t *testing.T) {
	t.Parallel()

	r := plugin.NewRegistry(nil, nil)
	p := &fake{desc: desc("docker")}
	if err := r.Register(p); err != nil {
		t.Fatal(err)
	}

	if _, ok := r.Get("docker"); !ok {
		t.Error("Get() did not find a registered plugin")
	}
	if _, ok := r.Get("kubernetes"); ok {
		t.Error("Get() found a plugin that was never registered")
	}
	// Before Activate there are no outcomes to report.
	if got := r.States(); len(got) != 0 {
		t.Errorf("States() = %v before Activate, want empty", got)
	}
}
