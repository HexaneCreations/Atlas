// Package config loads and validates Atlas's runtime configuration.
//
// Configuration is resolved in three layers, each overriding the last:
//
//  1. Compiled-in defaults from [Default]. Atlas starts with no configuration
//     file and no environment at all, in development mode.
//  2. An optional YAML file, for settings an operator wants under version
//     control and review.
//  3. Environment variables prefixed ATLAS_, for per-deployment values and
//     anything injected by an orchestrator.
//
// Secrets are the exception to layer 2: they belong in the environment, or
// better, in a file referenced by it (see [FileSuffix]). The YAML schema has
// no field a leaked repository would regret.
//
// Every value is validated once, at startup, before any component is
// constructed. A misconfigured Atlas fails immediately with a message naming
// the offending key — it never starts degraded and discovers the problem on
// the first request.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/log"
	"gopkg.in/yaml.v3"
)

// DefaultEnvPrefix is the prefix on every Atlas environment variable.
const DefaultEnvPrefix = "ATLAS"

// Environment names the deployment tier. It gates development-only
// affordances; nothing that weakens security may be enabled by anything other
// than an explicit setting of its own.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvStaging     Environment = "staging"
	EnvProduction  Environment = "production"
)

// IsProduction reports whether e denotes a production deployment.
func (e Environment) IsProduction() bool { return e == EnvProduction }

func (e Environment) valid() bool {
	switch e {
	case EnvDevelopment, EnvStaging, EnvProduction:
		return true
	}
	return false
}

// Config is the fully resolved configuration for an Atlas process.
//
// It is treated as immutable after [Load] returns. Components receive the
// section they need, by value, at construction time — nothing holds a pointer
// to the whole tree and reads from it later, which is what keeps
// configuration out of the set of things that can change under a running
// component.
type Config struct {
	// Environment is the deployment tier: development, staging, or production.
	Environment Environment `yaml:"environment" json:"environment" env:"ENVIRONMENT"`

	Node     Node       `yaml:"node" json:"node" env:"NODE"`
	Server   Server     `yaml:"server" json:"server" env:"SERVER"`
	Database Database   `yaml:"database" json:"database" env:"DATABASE"`
	Logging  log.Config `yaml:"logging" json:"logging" env:"LOGGING"`
	EventBus EventBus   `yaml:"event_bus" json:"event_bus" env:"EVENT_BUS"`

	// Plugins holds one opaque section per plugin, keyed by plugin ID. The
	// core never interprets these: each plugin decodes its own section into
	// its own type, so adding a technology needs no change here.
	//
	// Excluded from JSON and from environment binding. A plugin section can
	// hold anything, including a value an operator considers sensitive, and
	// the `atlas-server config` output is safe to run in production precisely
	// because it cannot print things it does not understand.
	Plugins    map[string]yaml.Node `yaml:"plugins" json:"-" env:"-"`
	Collection Collection           `yaml:"collection" json:"collection" env:"COLLECTION"`
	Fleet      Fleet                `yaml:"fleet" json:"fleet" env:"FLEET"`
}

// Fleet configures the agent-facing listeners: the HTTPS/mTLS one, and
// optionally the libp2p one (see LibP2PEnabled). Disabled by default: a
// single-node deployment needs none of it.
type Fleet struct {
	Enabled bool   `yaml:"enabled" json:"enabled" env:"ENABLED"`
	Host    string `yaml:"host" json:"host" env:"HOST"`
	Port    int    `yaml:"port" json:"port" env:"PORT"`
	// DataDir holds the CA key and certificate. Never served over the API.
	DataDir string `yaml:"data_dir" json:"data_dir" env:"DATA_DIR"`
	// AdvertisedHosts are the DNS names or IPs the control plane's own TLS
	// certificate is issued for — what agents actually connect to.
	AdvertisedHosts []string `yaml:"advertised_hosts" json:"advertised_hosts" env:"ADVERTISED_HOSTS"`

	// LibP2PEnabled starts a second agent-facing listener reachable by Peer
	// ID instead of address — see docs/adr/0012-connect-by-identity.md. It
	// serves the same mux as the HTTPS listener, but authenticates
	// differently: the Noise handshake proves the caller's Peer ID and the
	// agent_peers table authorizes it, so no TLS, certificate or enrollment
	// token is involved on this listener. Off by default; the HTTPS + mTLS
	// listener is unaffected either way.
	LibP2PEnabled bool `yaml:"libp2p_enabled" json:"libp2p_enabled" env:"LIBP2P_ENABLED"`
	// LibP2PListenAddrs are the multiaddrs this control plane accepts
	// inbound libp2p streams on, e.g. "/ip4/0.0.0.0/tcp/4102". Required when
	// LibP2PEnabled is true — the control plane is the side that must be
	// reachable; agents dial out and never listen.
	LibP2PListenAddrs []string `yaml:"libp2p_listen_addrs" json:"libp2p_listen_addrs" env:"LIBP2P_LISTEN_ADDRS"`
	// LibP2PRelayAddr is Atlas Relay's full multiaddr (including its Peer
	// ID) — see docs/adr/0012-connect-by-identity.md. When set, the control
	// plane connects out to it and reserves a relay slot instead of relying
	// on LibP2PListenAddrs being directly reachable, so it can sit behind
	// NAT with no forwarded port. LibP2PListenAddrs is still bound alongside
	// it (harmless, just unreachable from outside).
	LibP2PRelayAddr string `yaml:"libp2p_relay_addr" json:"libp2p_relay_addr" env:"LIBP2P_RELAY_ADDR"`

	// MaxRequestBytes caps an agent request body. This listener is reachable
	// by every enrolled agent in the fleet, so an unbounded body is a
	// memory-exhaustion vector. The cap is larger than the browser API's
	// because a telemetry POST carries a batch (up to maxBatchSize
	// envelopes), and a spool draining after an outage sends full batches.
	MaxRequestBytes int64 `yaml:"max_request_bytes" json:"max_request_bytes" env:"MAX_REQUEST_BYTES"`
	// RequestTimeout bounds a single agent request. Enrollment signs a
	// certificate and telemetry writes a batch to Postgres; neither should
	// be able to hold a handler open indefinitely.
	RequestTimeout time.Duration `yaml:"request_timeout" json:"request_timeout" env:"REQUEST_TIMEOUT"`

	// MaxRequestsPerMinute bounds how often one enrolled node may call the
	// agent API over the HTTPS listener. A single misconfigured or
	// compromised agent must not be able to flood ingest for the whole
	// fleet. Counted per node identity (mTLS peer certificate), not per IP:
	// many agents can share an egress address, and one agent can move
	// between addresses.
	MaxRequestsPerMinute int `yaml:"max_requests_per_minute" json:"max_requests_per_minute" env:"MAX_REQUESTS_PER_MINUTE"`
}

// Addr returns the host:port the fleet listener binds.
func (f Fleet) Addr() string { return fmt.Sprintf("%s:%d", f.Host, f.Port) }

// Node identifies the machine Atlas observes.
type Node struct {
	// ID pins the node identifier. Empty derives one from the OS machine id,
	// which is what makes identity survive a hostname change. Set it only to
	// adopt an existing naming scheme; changing it later orphans a machine's
	// history.
	ID string `yaml:"id" json:"id" env:"ID"`
	// IDFile is where a derived identifier is persisted when the OS provides
	// no machine id. Empty selects the first writable default path.
	IDFile string `yaml:"id_file" json:"id_file" env:"ID_FILE"`
	// Environment tags this machine — "production", "staging", whatever an
	// operator's naming happens to be. Atlas never guesses it: nothing on a
	// host declares which environment it belongs to, so an unset value leaves
	// the node honestly untagged rather than defaulting it to something
	// plausible and wrong.
	Environment string `yaml:"environment" json:"environment" env:"ENVIRONMENT"`
}

// Server configures the HTTP API listener.
type Server struct {
	// Host is the bind address. Defaults to 127.0.0.1 rather than 0.0.0.0:
	// Atlas exposes detailed infrastructure inventory, so reaching the
	// network must be a decision an operator makes explicitly.
	Host string `yaml:"host" json:"host" env:"HOST"`
	// Port is the TCP port to bind. Zero asks the kernel for an ephemeral
	// port, which tests use to avoid collisions.
	Port int `yaml:"port" json:"port" env:"PORT"`

	// ReadHeaderTimeout bounds how long a client may take to send request
	// headers. Without it a handful of idle connections can pin server
	// resources indefinitely (Slowloris).
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout" json:"read_header_timeout" env:"READ_HEADER_TIMEOUT"`
	ReadTimeout       time.Duration `yaml:"read_timeout" json:"read_timeout" env:"READ_TIMEOUT"`
	// WriteTimeout bounds a single response. Streaming endpoints (log tails,
	// event streams) opt out per route; see internal/platform/httpx.
	WriteTimeout time.Duration `yaml:"write_timeout" json:"write_timeout" env:"WRITE_TIMEOUT"`
	IdleTimeout  time.Duration `yaml:"idle_timeout" json:"idle_timeout" env:"IDLE_TIMEOUT"`
	// ShutdownTimeout bounds graceful drain before in-flight requests are
	// cut off.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" json:"shutdown_timeout" env:"SHUTDOWN_TIMEOUT"`
	// MaxRequestBytes caps a request body. Atlas is read-only and its bodies
	// are small; a low cap costs nothing and removes a memory-exhaustion
	// vector.
	MaxRequestBytes int64 `yaml:"max_request_bytes" json:"max_request_bytes" env:"MAX_REQUEST_BYTES"`

	// AllowedOrigins lists browser origins permitted to call the API. Empty
	// means same-origin only, which is correct when the server also serves
	// the built frontend. Wildcards are rejected in production.
	AllowedOrigins []string `yaml:"allowed_origins" json:"allowed_origins" env:"ALLOWED_ORIGINS"`
}

// Addr returns the host:port the server binds.
func (s Server) Addr() string { return fmt.Sprintf("%s:%d", s.Host, s.Port) }

// Database configures the PostgreSQL/TimescaleDB connection pool.
type Database struct {
	Host string `yaml:"host" json:"host" env:"HOST"`
	Port int    `yaml:"port" json:"port" env:"PORT"`
	Name string `yaml:"name" json:"name" env:"NAME"`
	User string `yaml:"user" json:"user" env:"USER"`
	// Password should be supplied as ATLAS_DATABASE_PASSWORD_FILE pointing at
	// a mounted secret. It is excluded from YAML deserialisation entirely so
	// that a credential cannot be committed to a configuration repository.
	Password string `yaml:"-" json:"-" env:"PASSWORD"`
	// SSLMode is a libpq sslmode. Anything weaker than verify-full is
	// rejected in production.
	SSLMode string `yaml:"ssl_mode" json:"ssl_mode" env:"SSL_MODE"`

	MaxConns        int32         `yaml:"max_conns" json:"max_conns" env:"MAX_CONNS"`
	MinConns        int32         `yaml:"min_conns" json:"min_conns" env:"MIN_CONNS"`
	MaxConnLifetime time.Duration `yaml:"max_conn_lifetime" json:"max_conn_lifetime" env:"MAX_CONN_LIFETIME"`
	MaxConnIdleTime time.Duration `yaml:"max_conn_idle_time" json:"max_conn_idle_time" env:"MAX_CONN_IDLE_TIME"`
	ConnectTimeout  time.Duration `yaml:"connect_timeout" json:"connect_timeout" env:"CONNECT_TIMEOUT"`

	// MigrateOnStart applies pending migrations during startup. Convenient
	// for single-node deployments; operators running several replicas may
	// prefer to gate migrations behind a deliberate `atlas-server migrate`
	// step. The migrator takes an advisory lock either way, so concurrent
	// starts are safe rather than merely unlikely.
	MigrateOnStart bool `yaml:"migrate_on_start" json:"migrate_on_start" env:"MIGRATE_ON_START"`
}

// EventBus configures the in-process event bus.
type EventBus struct {
	// BufferSize is the queue depth per subscriber. A slow subscriber fills
	// its buffer and then drops events; it can never block a publisher. See
	// internal/platform/eventbus for why that trade is the right one here.
	BufferSize int `yaml:"buffer_size" json:"buffer_size" env:"BUFFER_SIZE"`
}

// Collection configures the collector scheduler.
type Collection struct {
	// DefaultInterval is used by collectors that do not specify their own.
	DefaultInterval time.Duration `yaml:"default_interval" json:"default_interval" env:"DEFAULT_INTERVAL"`
	// Timeout bounds a single collection run. A collector that hangs on a
	// wedged filesystem or an unresponsive Docker socket is cancelled rather
	// than allowed to stall its schedule.
	Timeout time.Duration `yaml:"timeout" json:"timeout" env:"TIMEOUT"`
	// MaxConcurrent caps simultaneously running collectors, bounding the
	// observer's own footprint on a loaded host.
	MaxConcurrent int `yaml:"max_concurrent" json:"max_concurrent" env:"MAX_CONCURRENT"`

	// MaxSeriesPerCollector bounds how many distinct label combinations one
	// collector may produce. Exceeding it drops new series, keeps existing
	// ones, and marks the collector unhealthy.
	//
	// This is the guard against the most common way a metrics platform is
	// destroyed: a collector labelling by process id, container id, or
	// request path turns a handful of series into millions and makes the
	// datastore unusable. Set to a negative value to disable, which should be
	// a deliberate decision with the consequences understood.
	MaxSeriesPerCollector int `yaml:"max_series_per_collector" json:"max_series_per_collector" env:"MAX_SERIES_PER_COLLECTOR"`

	// SeriesWindow is how long a series is remembered after it stops
	// reporting. It lets legitimate churn — an unmounted filesystem, a renamed
	// interface — release budget rather than consuming it forever.
	SeriesWindow time.Duration `yaml:"series_window" json:"series_window" env:"SERIES_WINDOW"`
}

// Defaults for the cardinality budget.
const (
	// DefaultMaxSeriesPerCollector is generous for a well-behaved collector —
	// the system plugin produces roughly 60 on a typical host — and far below
	// the point at which a runaway one causes damage.
	DefaultMaxSeriesPerCollector = 1000
	// DefaultSeriesWindow is how long an unreported series holds its budget.
	DefaultSeriesWindow = time.Hour
)

// Default returns the compiled-in configuration.
//
// The defaults describe a working single-node development deployment: Atlas
// started with no file and no environment should run against a local
// Postgres, bound to loopback.
func Default() Config {
	return Config{
		Environment: EnvDevelopment,
		Server: Server{
			Host:              "127.0.0.1",
			Port:              8080,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			ShutdownTimeout:   20 * time.Second,
			MaxRequestBytes:   1 << 20, // 1 MiB
			AllowedOrigins:    nil,
		},
		Database: Database{
			Host:            "127.0.0.1",
			Port:            5432,
			Name:            "atlas",
			User:            "atlas",
			SSLMode:         "prefer",
			MaxConns:        16,
			MinConns:        2,
			MaxConnLifetime: time.Hour,
			MaxConnIdleTime: 30 * time.Minute,
			ConnectTimeout:  10 * time.Second,
			MigrateOnStart:  true,
		},
		Node:     Node{},
		Logging:  log.DefaultConfig(),
		EventBus: EventBus{BufferSize: 256},
		Collection: Collection{
			DefaultInterval:       15 * time.Second,
			Timeout:               10 * time.Second,
			MaxConcurrent:         8,
			MaxSeriesPerCollector: DefaultMaxSeriesPerCollector,
			SeriesWindow:          DefaultSeriesWindow,
		},
		Fleet: Fleet{
			Enabled:              false,
			Host:                 "0.0.0.0",
			Port:                 8443,
			DataDir:              "/var/lib/atlas/fleet",
			MaxRequestBytes:      8 << 20, // 8 MiB — a full telemetry batch, not a browser request
			RequestTimeout:       30 * time.Second,
			MaxRequestsPerMinute: 240, // 4/s sustained: well above a 15s collection interval, even with spool replay
		},
	}
}

// Options controls how [Load] resolves configuration.
type Options struct {
	// Path is the YAML file to read. Empty means consult ATLAS_CONFIG_FILE,
	// and if that is unset, skip the file layer entirely.
	Path string
	// EnvPrefix defaults to [DefaultEnvPrefix].
	EnvPrefix string
	// Lookup defaults to os.LookupEnv. Injected for testing.
	Lookup LookupFunc
	// ReadFile defaults to os.ReadFile. Injected for testing.
	ReadFile ReadFileFunc
}

// ConfigFileEnvVar names the variable that points at the YAML file.
const ConfigFileEnvVar = DefaultEnvPrefix + "_CONFIG_FILE"

// Load resolves configuration through all three layers and validates the
// result.
//
// A returned error is fatal and already phrased for an operator: it names the
// key at fault and what was expected.
func Load(opts Options) (*Config, error) {
	if opts.EnvPrefix == "" {
		opts.EnvPrefix = DefaultEnvPrefix
	}
	if opts.Lookup == nil {
		opts.Lookup = os.LookupEnv
	}
	if opts.ReadFile == nil {
		opts.ReadFile = os.ReadFile
	}

	cfg := Default()

	path := opts.Path
	if path == "" {
		path, _ = opts.Lookup(ConfigFileEnvVar)
	}
	if path != "" {
		data, err := opts.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
		// KnownFields makes a typo in a config file a startup failure rather
		// than a setting that silently does nothing.
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	if err := bindEnv(&cfg, opts.EnvPrefix, opts.Lookup, opts.ReadFile); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// EnvVars returns every environment variable Atlas recognises, derived from
// the configuration struct itself.
func EnvVars() []string {
	cfg := Default()
	return envNames(&cfg, DefaultEnvPrefix)
}
