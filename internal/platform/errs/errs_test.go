package errs_test

import (
	stderrors "errors"
	"fmt"
	"testing"

	"github.com/hexane/atlas/internal/platform/errs"
)

func TestWrapNilReturnsNil(t *testing.T) {
	t.Parallel()

	if got := errs.Wrap(nil, errs.CodeInternal, "boom"); got != nil {
		t.Fatalf("Wrap(nil) = %v, want nil", got)
	}
}

func TestCodeOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want errs.Code
	}{
		{"nil", nil, ""},
		{"typed", errs.New(errs.CodeNotFound, "no host"), errs.CodeNotFound},
		{"foreign", stderrors.New("raw"), errs.CodeInternal},
		{
			name: "wrapped by fmt",
			err:  fmt.Errorf("layer: %w", errs.New(errs.CodeRateLimited, "slow down")),
			want: errs.CodeRateLimited,
		},
		{
			name: "outermost code wins",
			err:  errs.Wrap(errs.New(errs.CodeNotFound, "inner"), errs.CodeUnavailable, "outer"),
			want: errs.CodeUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := errs.CodeOf(tt.err); got != tt.want {
				t.Errorf("CodeOf() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The redaction boundary is the whole point of the package: an internal or
// unclassified error must never surface its cause to a caller.
func TestMessageRedactsInternalAndForeignErrors(t *testing.T) {
	t.Parallel()

	const secret = "password=hunter2 host=db.internal"

	tests := []struct {
		name string
		err  error
	}{
		{"foreign", stderrors.New(secret)},
		{"explicitly internal", errs.New(errs.CodeInternal, secret)},
		{"internal wrapping a cause", errs.Wrap(stderrors.New(secret), errs.CodeInternal, "connect failed")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := errs.Message(tt.err)
			if got != "An internal error occurred." {
				t.Errorf("Message() = %q, want the generic message", got)
			}
			if errs.Details(tt.err) != nil {
				t.Error("Details() leaked details for an internal error")
			}
		})
	}
}

func TestMessagePassesThroughClientSafeErrors(t *testing.T) {
	t.Parallel()

	err := errs.New(errs.CodeInvalidArgument, "interval must be positive").
		WithDetail("field", "collector.interval")

	if got, want := errs.Message(err), "interval must be positive"; got != want {
		t.Errorf("Message() = %q, want %q", got, want)
	}
	if got := errs.Details(err)["field"]; got != "collector.interval" {
		t.Errorf("Details()[field] = %v, want collector.interval", got)
	}
}

func TestWrapInheritsDetailsFromInnerError(t *testing.T) {
	t.Parallel()

	inner := errs.New(errs.CodeInvalidArgument, "bad port").WithDetail("port", 70000)
	outer := errs.Wrap(inner, errs.CodeInvalidArgument, "invalid server configuration")

	if got := errs.Details(outer)["port"]; got != 70000 {
		t.Errorf("Details()[port] = %v, want 70000", got)
	}
	// Inheritance must copy, not alias, or an outer annotation would mutate
	// the error a caller still holds.
	outer.WithDetail("port", 1)
	if got := inner.Details["port"]; got != 70000 {
		t.Errorf("inner details mutated by outer: got %v, want 70000", got)
	}
}

func TestUnwrapPreservesSentinelMatching(t *testing.T) {
	t.Parallel()

	sentinel := stderrors.New("connection refused")
	err := errs.Wrap(sentinel, errs.CodeUnavailable, "database unreachable").
		WithOp("postgres.Pool.Ping")

	if !errs.Is(err, sentinel) {
		t.Error("Is() lost the wrapped sentinel")
	}
	if got := err.Error(); got == errs.Message(err) {
		t.Error("operator string and client message should differ")
	}
}

func TestCodeValid(t *testing.T) {
	t.Parallel()

	if !errs.CodeNotFound.Valid() {
		t.Error("CodeNotFound should be valid")
	}
	if errs.Code("teapot").Valid() {
		t.Error("unknown code should be invalid")
	}
}
