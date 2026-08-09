package config

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// violations accumulates every configuration problem found in one pass.
//
// Reporting all faults at once matters more than it looks: an operator
// bringing up Atlas in a pipeline otherwise pays a full restart cycle per
// typo. Fail once, with the complete list.
type violations struct {
	items []string
}

func (v *violations) addf(key, format string, args ...any) {
	v.items = append(v.items, fmt.Sprintf("%s: %s", key, fmt.Sprintf(format, args...)))
}

func (v *violations) err() error {
	if len(v.items) == 0 {
		return nil
	}
	slices.Sort(v.items)
	return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(v.items, "\n  - "))
}

// validSSLModes are libpq's sslmode values, ordered weakest to strongest.
var validSSLModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// verifyingSSLModes actually authenticate the server certificate. The others
// encrypt at best, and `require` in particular is widely and wrongly believed
// to be safe: it accepts any certificate, so it stops passive sniffing but
// not an active machine-in-the-middle.
var verifyingSSLModes = []string{"verify-ca", "verify-full"}

// Validate checks the whole configuration and returns every problem found.
//
// Production deployments are held to stricter rules than development ones.
// Those rules are enforced here rather than left to a deployment checklist,
// because a checklist is not a control: it cannot fail a rollout.
func (c *Config) Validate() error {
	v := &violations{}
	prod := c.Environment.IsProduction()

	if !c.Environment.valid() {
		v.addf("environment", "unknown environment %q (want development, staging, or production)", c.Environment)
	}

	c.Server.validate(v, prod)
	c.Database.validate(v, prod)
	c.EventBus.validate(v)
	c.Collection.validate(v)
	c.Fleet.validate(v)

	if err := c.Logging.Validate(); err != nil {
		v.addf("logging", "%s", err)
	}

	return v.err()
}

func (f Fleet) validate(v *violations) {
	if !f.Enabled {
		return
	}
	if f.Port <= 0 || f.Port > 65535 {
		v.addf("fleet.port", "must be between 1 and 65535, got %d", f.Port)
	}
	if f.DataDir == "" {
		v.addf("fleet.data_dir", "must not be empty when fleet is enabled")
	}
	if f.LibP2PEnabled && len(f.LibP2PListenAddrs) == 0 {
		v.addf("fleet.libp2p_listen_addrs", "must list at least one multiaddr when libp2p is enabled")
	}
}

func (s Server) validate(v *violations, prod bool) {
	if s.Host == "" {
		v.addf("server.host", "must not be empty")
	}
	// Port 0 is meaningful, not missing: it asks the kernel for an ephemeral
	// port. Tests and sidecar deployments rely on it, and httpx.Server.Addr
	// reports whatever was actually bound.
	if s.Port < 0 || s.Port > 65535 {
		v.addf("server.port", "must be between 0 and 65535, got %d", s.Port)
	}

	if s.ReadHeaderTimeout <= 0 {
		v.addf("server.read_header_timeout", "must be positive")
	}
	if s.ReadTimeout <= 0 {
		v.addf("server.read_timeout", "must be positive")
	}
	if s.WriteTimeout <= 0 {
		v.addf("server.write_timeout", "must be positive")
	}
	if s.IdleTimeout <= 0 {
		v.addf("server.idle_timeout", "must be positive")
	}
	if s.ShutdownTimeout <= 0 {
		v.addf("server.shutdown_timeout", "must be positive")
	}
	if s.ReadHeaderTimeout > s.ReadTimeout {
		v.addf("server.read_header_timeout", "must not exceed server.read_timeout (%s)", s.ReadTimeout)
	}
	if s.MaxRequestBytes <= 0 {
		v.addf("server.max_request_bytes", "must be positive")
	}

	for _, origin := range s.AllowedOrigins {
		if origin == "*" {
			if prod {
				v.addf("server.allowed_origins",
					"wildcard \"*\" is not permitted in production; list the exact origins that may call the API")
			}
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			v.addf("server.allowed_origins", "%q is not a valid origin (want scheme://host[:port])", origin)
			continue
		}
		if u.Path != "" || u.RawQuery != "" {
			v.addf("server.allowed_origins", "%q must be an origin only, without a path or query", origin)
		}
		if prod && u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
			v.addf("server.allowed_origins", "%q uses plaintext http in production", origin)
		}
	}
}

func (d Database) validate(v *violations, prod bool) {
	if d.Host == "" {
		v.addf("database.host", "must not be empty")
	}
	if d.Port < 1 || d.Port > 65535 {
		v.addf("database.port", "must be between 1 and 65535, got %d", d.Port)
	}
	if d.Name == "" {
		v.addf("database.name", "must not be empty")
	}
	if d.User == "" {
		v.addf("database.user", "must not be empty")
	}

	if !slices.Contains(validSSLModes, d.SSLMode) {
		v.addf("database.ssl_mode", "unknown mode %q (want one of %s)", d.SSLMode, strings.Join(validSSLModes, ", "))
	} else if prod && !slices.Contains(verifyingSSLModes, d.SSLMode) {
		v.addf("database.ssl_mode",
			"%q does not verify the server certificate; production requires verify-ca or verify-full", d.SSLMode)
	}

	if prod && d.Password == "" {
		v.addf("database.password",
			"must be set in production; supply %s_DATABASE_PASSWORD_FILE pointing at a mounted secret", DefaultEnvPrefix)
	}

	if d.MaxConns < 1 {
		v.addf("database.max_conns", "must be at least 1, got %d", d.MaxConns)
	}
	if d.MinConns < 0 {
		v.addf("database.min_conns", "must not be negative, got %d", d.MinConns)
	}
	if d.MinConns > d.MaxConns {
		v.addf("database.min_conns", "must not exceed database.max_conns (%d), got %d", d.MaxConns, d.MinConns)
	}
	if d.MaxConnLifetime <= 0 {
		v.addf("database.max_conn_lifetime", "must be positive")
	}
	if d.MaxConnIdleTime <= 0 {
		v.addf("database.max_conn_idle_time", "must be positive")
	}
	if d.MaxConnIdleTime > d.MaxConnLifetime {
		v.addf("database.max_conn_idle_time", "must not exceed database.max_conn_lifetime (%s)", d.MaxConnLifetime)
	}
	if d.ConnectTimeout <= 0 {
		v.addf("database.connect_timeout", "must be positive")
	}
}

func (e EventBus) validate(v *violations) {
	if e.BufferSize < 1 {
		v.addf("event_bus.buffer_size", "must be at least 1, got %d", e.BufferSize)
	}
}

func (c Collection) validate(v *violations) {
	if c.DefaultInterval <= 0 {
		v.addf("collection.default_interval", "must be positive")
	}
	if c.Timeout <= 0 {
		v.addf("collection.timeout", "must be positive")
	}
	if c.Timeout > c.DefaultInterval {
		// A run that outlives its interval means schedules overlap forever
		// and the observer becomes a load source of its own.
		v.addf("collection.timeout", "must not exceed collection.default_interval (%s)", c.DefaultInterval)
	}
	if c.MaxConcurrent < 1 {
		v.addf("collection.max_concurrent", "must be at least 1, got %d", c.MaxConcurrent)
	}
	if c.MaxSeriesPerCollector == 0 {
		v.addf("collection.max_series_per_collector",
			"must be positive, or negative to disable the budget deliberately")
	}
	if c.SeriesWindow < 0 {
		v.addf("collection.series_window", "must not be negative")
	}
}
