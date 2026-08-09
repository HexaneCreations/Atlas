package collect_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/platform/errs"
)

// stub is a minimal Collector for registry tests.
type stub struct {
	desc    collect.Descriptor
	samples []collect.Sample
	err     error
}

func (s stub) Descriptor() collect.Descriptor { return s.desc }

func (s stub) Collect(context.Context) ([]collect.Sample, error) { return s.samples, s.err }

func validSample() collect.Sample {
	return collect.Sample{
		Metric: "system.cpu.usage",
		Value:  42.5,
		Unit:   collect.UnitPercent,
		Kind:   collect.KindGauge,
		Time:   time.Now(),
	}
}

func TestSampleValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*collect.Sample)
		valid  bool
	}{
		{"valid", func(*collect.Sample) {}, true},
		{"no metric", func(s *collect.Sample) { s.Metric = "" }, false},
		{"no unit", func(s *collect.Sample) { s.Unit = "" }, false},
		{"no kind", func(s *collect.Sample) { s.Kind = "" }, false},
		{"no timestamp", func(s *collect.Sample) { s.Time = time.Time{} }, false},
		{"NaN value", func(s *collect.Sample) { s.Value = math.NaN() }, false},
		{"positive infinity", func(s *collect.Sample) { s.Value = math.Inf(1) }, false},
		{"negative infinity", func(s *collect.Sample) { s.Value = math.Inf(-1) }, false},
		{"zero is valid", func(s *collect.Sample) { s.Value = 0 }, true},
		{"negative is valid", func(s *collect.Sample) { s.Value = -12.5 }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := validSample()
			tt.mutate(&s)

			err := s.Validate()
			if tt.valid && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !tt.valid {
				if err == nil {
					t.Fatal("Validate() = nil, want an error")
				}
				if got := errs.CodeOf(err); got != errs.CodeInvalidArgument {
					t.Errorf("code = %q, want invalid_argument", got)
				}
			}
		})
	}
}

func TestDescriptorValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		desc  collect.Descriptor
		valid bool
	}{
		{"minimal", collect.Descriptor{ID: "system.cpu"}, true},
		{"with interval and timeout", collect.Descriptor{
			ID: "system.cpu", Interval: 30 * time.Second, Timeout: 5 * time.Second,
		}, true},
		{"no ID", collect.Descriptor{}, false},
		{"negative interval", collect.Descriptor{ID: "x", Interval: -time.Second}, false},
		{"negative timeout", collect.Descriptor{ID: "x", Timeout: -time.Second}, false},
		{
			// Runs would overlap forever, making the collector a permanent
			// load source.
			name:  "timeout exceeds interval",
			desc:  collect.Descriptor{ID: "x", Interval: 5 * time.Second, Timeout: 30 * time.Second},
			valid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.desc.Validate(); (err == nil) != tt.valid {
				t.Errorf("Validate() = %v, want valid = %v", err, tt.valid)
			}
		})
	}
}

func TestBatchDuration(t *testing.T) {
	t.Parallel()

	start := time.Now()
	b := collect.Batch{StartedAt: start, CompletedAt: start.Add(250 * time.Millisecond)}
	if got := b.Duration(); got != 250*time.Millisecond {
		t.Errorf("Duration() = %v, want 250ms", got)
	}
}

func TestRegistryRegisterAndRetrieve(t *testing.T) {
	t.Parallel()

	r := collect.NewRegistry()
	cpu := stub{desc: collect.Descriptor{ID: "system.cpu", Name: "CPU"}}
	mem := stub{desc: collect.Descriptor{ID: "system.memory", Name: "Memory"}}

	if err := r.Register(cpu); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := r.Register(mem); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if got := r.Len(); got != 2 {
		t.Errorf("Len() = %d, want 2", got)
	}
	if _, ok := r.Get("system.cpu"); !ok {
		t.Error("Get(system.cpu) not found")
	}
	if _, ok := r.Get("system.disk"); ok {
		t.Error("Get returned a collector that was never registered")
	}

	// Registration order must be preserved so startup logs are diffable.
	descs := r.Descriptors()
	if len(descs) != 2 || descs[0].ID != "system.cpu" || descs[1].ID != "system.memory" {
		t.Errorf("Descriptors() = %v, want registration order", descs)
	}
}

// Two collectors claiming one ID would silently halve the sample rate of
// whichever lost, which looks like a monitoring outage rather than a bug.
func TestRegistryRejectsDuplicateID(t *testing.T) {
	t.Parallel()

	r := collect.NewRegistry()
	if err := r.Register(stub{desc: collect.Descriptor{ID: "system.cpu"}}); err != nil {
		t.Fatal(err)
	}

	err := r.Register(stub{desc: collect.Descriptor{ID: "system.cpu", Name: "Other"}})
	if err == nil {
		t.Fatal("Register() accepted a duplicate ID")
	}
	if got := errs.CodeOf(err); got != errs.CodeAlreadyExists {
		t.Errorf("code = %q, want already_exists", got)
	}
	if r.Len() != 1 {
		t.Errorf("Len() = %d, want 1; the duplicate must not replace the original", r.Len())
	}
}

func TestRegistryRejectsInvalidDescriptor(t *testing.T) {
	t.Parallel()

	r := collect.NewRegistry()
	if err := r.Register(stub{desc: collect.Descriptor{}}); err == nil {
		t.Error("Register() accepted a collector with no ID")
	}
}

func TestRegistryUnregister(t *testing.T) {
	t.Parallel()

	r := collect.NewRegistry()
	if err := r.Register(stub{desc: collect.Descriptor{ID: "docker.containers"}}); err != nil {
		t.Fatal(err)
	}

	if !r.Unregister("docker.containers") {
		t.Error("Unregister() reported nothing removed")
	}
	if r.Unregister("docker.containers") {
		t.Error("Unregister() removed the same collector twice")
	}
	if r.Len() != 0 {
		t.Errorf("Len() = %d, want 0", r.Len())
	}
	// Order tracking must be cleaned up too, or All would panic on a nil entry.
	if got := r.All(); len(got) != 0 {
		t.Errorf("All() = %v, want empty", got)
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	t.Parallel()

	r := collect.NewRegistry()
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := range 100 {
			_ = r.Register(stub{desc: collect.Descriptor{ID: string(rune('a'+i%26)) + string(rune('0'+i/26))}})
		}
	}()
	for range 100 {
		_ = r.All()
		_ = r.Len()
		_, _ = r.Get("a0")
	}
	<-done

	if r.Len() == 0 {
		t.Error("no collectors survived the concurrent run")
	}
}

func TestCloneLabels(t *testing.T) {
	t.Parallel()

	if got := collect.CloneLabels(nil); got != nil {
		t.Errorf("CloneLabels(nil) = %v, want nil", got)
	}

	original := map[string]string{"device": "sda"}
	clone := collect.CloneLabels(original)
	clone["device"] = "sdb"

	if original["device"] != "sda" {
		t.Error("CloneLabels returned an alias, not a copy")
	}
}
