package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/hexane/atlas/internal/platform/log"
)

// newTestLogger returns a JSON logger and a decode helper for its output.
func newTestLogger(t *testing.T) (*slog.Logger, func() map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	l, err := log.New(log.Config{Level: "debug", Format: log.FormatJSON}, &buf)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return l, func() map[string]any {
		t.Helper()
		var rec map[string]any
		line := strings.TrimSpace(buf.String())
		if line == "" {
			t.Fatal("no log record was written")
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode record %q: %v", line, err)
		}
		return rec
	}
}

func TestRedactsSensitiveAttributes(t *testing.T) {
	t.Parallel()

	sensitive := []string{
		"password", "db_password", "PGPASSWORD",
		"token", "agent_token", "api_key", "apiKey",
		"secret", "client_secret", "authorization", "private_key",
	}

	for _, key := range sensitive {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			l, decode := newTestLogger(t)
			l.Info("connecting", key, "hunter2")

			if got := decode()[key]; got != log.Redacted {
				t.Errorf("attribute %q = %v, want %q", key, got, log.Redacted)
			}
		})
	}
}

func TestDoesNotRedactOrdinaryAttributes(t *testing.T) {
	t.Parallel()

	l, decode := newTestLogger(t)
	l.Info("connecting", "host", "db.internal", "port", 5432)

	rec := decode()
	if got := rec["host"]; got != "db.internal" {
		t.Errorf("host = %v, want db.internal", got)
	}
	if got := rec["port"]; got != float64(5432) {
		t.Errorf("port = %v, want 5432", got)
	}
}

func TestContextAttributesAppearOnRecords(t *testing.T) {
	t.Parallel()

	l, decode := newTestLogger(t)
	ctx := log.WithAttrs(context.Background(), slog.String("request_id", "req-1"))
	ctx = log.WithAttrs(ctx, slog.String("collector", "cpu"))

	l.InfoContext(ctx, "collected")

	rec := decode()
	if got := rec["request_id"]; got != "req-1" {
		t.Errorf("request_id = %v, want req-1", got)
	}
	if got := rec["collector"]; got != "cpu" {
		t.Errorf("collector = %v, want cpu", got)
	}
}

// A context is shared across goroutines by design. Deriving two children from
// one parent must not let either observe the other's attributes.
func TestWithAttrsDoesNotAliasParentAttributes(t *testing.T) {
	t.Parallel()

	parent := log.WithAttrs(context.Background(), slog.String("shared", "yes"))

	var wg sync.WaitGroup
	results := make([]map[string]any, 2)
	for i, name := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, decode := newTestLogger(t)
			l.InfoContext(log.WithAttrs(parent, slog.String("branch", name)), "msg")
			results[i] = decode()
		}()
	}
	wg.Wait()

	for i, want := range []string{"a", "b"} {
		if got := results[i]["branch"]; got != want {
			t.Errorf("branch = %v, want %v", got, want)
		}
		if got := results[i]["shared"]; got != "yes" {
			t.Errorf("shared = %v, want yes", got)
		}
	}
}

func TestFromContextFallsBackToDefault(t *testing.T) {
	t.Parallel()

	if log.FromContext(context.Background()) == nil {
		t.Fatal("FromContext returned nil for a bare context")
	}

	want := log.Discard()
	if got := log.FromContext(log.Into(context.Background(), want)); got != want {
		t.Error("FromContext did not return the logger stored by Into")
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     log.Config
		wantErr bool
	}{
		{"defaults", log.DefaultConfig(), false},
		{"text warn", log.Config{Level: "warn", Format: log.FormatText}, false},
		{"empty level means info", log.Config{Level: "", Format: log.FormatJSON}, false},
		{"bad level", log.Config{Level: "verbose", Format: log.FormatJSON}, true},
		{"bad format", log.Config{Level: "info", Format: "xml"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.cfg.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestLevelFiltering(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	l, err := log.New(log.Config{Level: "warn", Format: log.FormatJSON}, &buf)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	l.Info("dropped")
	if buf.Len() != 0 {
		t.Errorf("info record emitted at warn level: %s", buf.String())
	}
	l.Warn("kept")
	if !strings.Contains(buf.String(), "kept") {
		t.Error("warn record was not emitted at warn level")
	}
}
