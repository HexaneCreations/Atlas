// Package log builds Atlas's structured loggers.
//
// Atlas standardises on log/slog rather than a third-party logger: it is in
// the standard library, its Handler interface is a stable extension point,
// and it keeps a monitoring product from depending on someone else's logging
// release cadence for the lifetime of the project.
//
// Two behaviours are layered on top of the stdlib handlers:
//
//   - Context propagation. Attributes attached with [WithAttrs] travel on the
//     context and appear on every record logged downstream. A request id set
//     once by middleware therefore lands on every line emitted while serving
//     that request, without any function threading a logger through its
//     signature.
//   - Redaction. Attribute values whose keys look like credentials are
//     replaced before they reach the writer. Atlas reads configuration that
//     contains database passwords and, later, agent keys; a single misplaced
//     debug line should not be able to write one to disk.
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Redacted replaces the value of any attribute whose key looks sensitive.
const Redacted = "[REDACTED]"

// Format selects the on-disk encoding of log records.
type Format string

const (
	// FormatJSON emits one JSON object per record. The default, and the only
	// format that should be used where logs are shipped to an aggregator.
	FormatJSON Format = "json"
	// FormatText emits key=value pairs, for local development.
	FormatText Format = "text"
)

// Config describes how to construct a logger. It is populated from the
// `logging` block of Atlas configuration.
type Config struct {
	// Level is the minimum severity to emit: debug, info, warn, or error.
	Level string `yaml:"level" json:"level" env:"LEVEL"`
	// Format is the record encoding. See [Format].
	Format Format `yaml:"format" json:"format" env:"FORMAT"`
	// AddSource records the file and line of the log call. Useful in
	// development; it costs a caller lookup per record in production.
	AddSource bool `yaml:"add_source" json:"add_source" env:"ADD_SOURCE"`
}

// DefaultConfig returns the production-safe logging defaults.
func DefaultConfig() Config {
	return Config{Level: "info", Format: FormatJSON, AddSource: false}
}

// Validate reports whether the configuration is usable.
func (c Config) Validate() error {
	if _, err := parseLevel(c.Level); err != nil {
		return err
	}
	switch c.Format {
	case FormatJSON, FormatText:
	default:
		return fmt.Errorf("logging.format: unknown format %q (want json or text)", c.Format)
	}
	return nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging.level: unknown level %q (want debug, info, warn, or error)", s)
	}
}

// New builds a logger writing to w.
//
// It returns an error only for invalid configuration, so a caller that has
// already called Config.Validate can treat failure as a programming bug.
func New(cfg Config, w io.Writer) (*slog.Logger, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	level, _ := parseLevel(cfg.Level)

	opts := &slog.HandlerOptions{
		Level:       level,
		AddSource:   cfg.AddSource,
		ReplaceAttr: redact,
	}

	var h slog.Handler
	if cfg.Format == FormatText {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(&contextHandler{Handler: h}), nil
}

// sensitiveKeyParts are matched as substrings against a lowercased attribute
// key. Substring matching is deliberate: it catches db_password, PGPASSWORD,
// and agent_token alike without maintaining an exhaustive list of key names.
var sensitiveKeyParts = []string{
	"password", "passwd", "passphrase",
	"secret", "token", "credential",
	"apikey", "api_key",
	"authorization", "auth_header",
	"private_key", "session_id", "cookie",
}

// IsSensitiveKey reports whether an attribute key names a value that must not
// be written to logs.
func IsSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, part := range sensitiveKeyParts {
		if strings.Contains(k, part) {
			return true
		}
	}
	return false
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	return a
}

// ctxKey is unexported so no other package can collide with or overwrite the
// values Atlas stores on a context.
type ctxKey int

const (
	loggerKey ctxKey = iota
	attrsKey
)

// contextHandler decorates records with attributes accumulated on the context.
type contextHandler struct{ slog.Handler }

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(attrsKey).([]slog.Attr); ok && len(attrs) > 0 {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}

// WithAttrs returns a context carrying attrs in addition to any already
// present. Every record logged with a logger from this package, using the
// returned context, includes them.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	existing, _ := ctx.Value(attrsKey).([]slog.Attr)
	// Copy rather than append in place: sibling goroutines may hold the
	// parent context and appending could write into shared backing array.
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, attrsKey, merged)
}

// Into returns a context carrying l as the ambient logger.
func Into(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// FromContext returns the logger carried by ctx, or the process default if
// none was set. It never returns nil, so call sites need no guard.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// Discard returns a logger that drops every record. For tests and for the
// brief window before configuration has been loaded.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
