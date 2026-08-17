package redact_test

import (
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/platform/redact"
)

// Exact examples from the local verification checklist (goal §9), run
// individually so the report can quote each input/output pair.
func TestLocalVerifyRedactionExamples(t *testing.T) {
	examples := []struct {
		name   string
		in     string
		secret string
	}{
		{"password", "--password=secret123", "secret123"},
		{"token", "--token=abc123", "abc123"},
		{"api-key", "--api-key=abc123", "abc123"},
		{"secret", "--secret=value", "value"},
		{"authorization bearer", `Authorization: Bearer abc123`, "abc123"},
	}
	for _, ex := range examples {
		t.Run(ex.name, func(t *testing.T) {
			out := redact.String(ex.in)
			t.Logf("IN:  %s\nOUT: %s", ex.in, out)
			if strings.Contains(out, ex.secret) {
				t.Errorf("secret %q still present in output %q", ex.secret, out)
			}
			if !strings.Contains(out, redact.Placeholder) {
				t.Errorf("output %q has no redaction marker", out)
			}
		})
	}

	preserved := []string{
		"--port 8080",
		"-Xmx4g",
		"-jar app.jar",
	}
	for _, in := range preserved {
		t.Run("preserved:"+in, func(t *testing.T) {
			out := redact.String(in)
			t.Logf("IN:  %s\nOUT: %s (unchanged)", in, out)
			if out != in {
				t.Errorf("normal argument %q was altered to %q", in, out)
			}
		})
	}
}
