// Package docker observes the Docker Engine: containers, their resource use
// and health, images, networks, volumes, the daemon's event stream, and
// container logs.
//
// As with the system plugin, collectors here never touch the Docker SDK
// directly — they call [Client]. That seam pays for itself twice: it makes
// every collector testable against exact container states that are awkward to
// stage against a real daemon (a container that vanishes mid-collection, a
// health check that has never run, a stats payload from an older API version),
// and it isolates the one place that has to cope with Docker's API version
// negotiation.
package docker

import (
	"context"
	"time"
)

// Client is the Docker Engine surface Atlas needs.
//
// Every method must honour its context. A Docker daemon that is wedged — mid
// image pull, out of disk, deadlocked on a storage driver — will accept a
// connection and never answer, and a collector that waits forever on it pins a
// scheduler slot at exactly the moment an operator needs monitoring most.
type Client interface {
	// Ping verifies the daemon answers. Used for detection and health.
	Ping(ctx context.Context) (Version, error)

	// Containers lists containers, including stopped ones.
	Containers(ctx context.Context) ([]Container, error)
	// ContainerStats returns a single point-in-time resource sample.
	ContainerStats(ctx context.Context, containerID string) (Stats, error)

	// Inspect returns one container's configuration.
	//
	// Read on demand, never on a sweep: it is one API call per container, and
	// the fields it returns — mounts, published ports, restart policy — change
	// only when a container is recreated. Polling them every interval would
	// multiply the daemon's load by the container count to observe values that
	// are effectively static.
	Inspect(ctx context.Context, containerID string) (ContainerDetail, error)

	// Images, Networks, and Volumes describe the daemon's inventory.
	Images(ctx context.Context) (Inventory, error)
	Networks(ctx context.Context) (int, error)
	Volumes(ctx context.Context) (Inventory, error)

	// Events streams daemon events until ctx is cancelled. The error channel
	// receives at most one error; both channels are closed when the stream
	// ends.
	Events(ctx context.Context) (<-chan Event, <-chan error)

	// Logs streams a container's log lines. follow=false reads the tail and
	// returns; follow=true streams until ctx is cancelled.
	Logs(ctx context.Context, containerID string, opts LogOptions) (<-chan LogLine, <-chan error)

	// Close releases the underlying connection.
	Close() error
}

// Version identifies the daemon.
type Version struct {
	Version       string
	APIVersion    string
	OS            string
	Arch          string
	KernelVersion string
}

// State is a container's lifecycle state, normalised to Docker's own set.
type State string

const (
	StateCreated    State = "created"
	StateRunning    State = "running"
	StatePaused     State = "paused"
	StateRestarting State = "restarting"
	StateRemoving   State = "removing"
	StateExited     State = "exited"
	StateDead       State = "dead"
)

// Running reports whether the container is executing.
func (s State) Running() bool { return s == StateRunning }

// Health is the result of a container's HEALTHCHECK.
//
// Distinct from [State] because they answer different questions: a container
// can be running and unhealthy, which is the single most useful signal Docker
// exposes and the one a plain `docker ps` glance misses.
type Health string

const (
	// HealthNone means the image declares no health check. Not a failure —
	// most images do not, and reporting it as unhealthy would make the signal
	// useless.
	HealthNone Health = "none"
	// HealthStarting means the check is inside its start period.
	HealthStarting  Health = "starting"
	HealthHealthy   Health = "healthy"
	HealthUnhealthy Health = "unhealthy"
)

// Container is one container as Atlas models it.
type Container struct {
	ID    string
	Name  string
	Image string
	// ImageID is the resolved digest. Two containers can report the same
	// image tag while running different builds of it, which is exactly the
	// confusion an incident needs to resolve quickly.
	ImageID string

	State  State
	Health Health
	// ExitCode is meaningful only once the container has exited.
	ExitCode int

	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time

	// RestartCount is the daemon's own counter. A container that is running
	// now but has restarted forty times is not healthy, and no instantaneous
	// state reveals that.
	RestartCount int

	// Labels carry Compose project and service names, Kubernetes pod
	// metadata, and whatever else an operator set. They are the raw material
	// for the Tier 2 service catalog, but only a curated few become metric
	// labels — see labelsForMetrics.
	Labels map[string]string
}

// ContainerDetail is one container's configuration, read on demand.
//
// Environment variables are deliberately absent and will stay absent. They
// routinely carry database passwords, API tokens and signing keys; a
// monitoring tool that reads them turns every dashboard viewer into a holder
// of every secret on the host. The same decision governs process environments
// elsewhere in Atlas. This is a boundary, not a gap.
type ContainerDetail struct {
	// Container is the same inventory the list endpoint returns. It is
	// embedded rather than fetched separately because a single inspect call
	// already carries all of it — resolving a container's identity by listing
	// every container first, as this once did, cost an inspect per container
	// on the host to answer a question about one of them.
	Container

	// Command and Entrypoint are what the container actually runs. Useful
	// during an incident and free of credentials in the way an environment
	// block is not — though an operator can still put a secret in an argument,
	// which is why neither is treated as safe to log.
	Entrypoint []string
	Command    []string
	WorkingDir string
	User       string

	RestartPolicy RestartPolicy
	Limits        Limits

	Mounts   []Mount
	Networks []NetworkAttachment
	Ports    []PortBinding

	// SizeRw and SizeRootFs are populated only when the caller asks for them;
	// computing them walks the container's layers and is expensive enough that
	// Atlas does not.
	Platform string
	Driver   string
}

// RestartPolicy is the daemon's restart configuration for a container.
type RestartPolicy struct {
	// Name is Docker's policy: "no", "always", "unless-stopped", "on-failure".
	Name string
	// MaximumRetryCount applies to on-failure only.
	MaximumRetryCount int
}

// Limits are the resource ceilings configured on a container.
//
// Zero means unlimited, which is Docker's own convention and a meaningful
// answer rather than missing data: a container with no memory limit can take
// the host down, and that is worth showing as "unlimited" rather than as "0".
type Limits struct {
	NanoCPUs    int64
	CPUShares   int64
	MemoryBytes int64
	MemorySwap  int64
	PidsLimit   int64
}

// Mount is one filesystem mounted into a container.
type Mount struct {
	// Type is "bind", "volume", "tmpfs", or "npipe".
	Type        string
	Name        string
	Source      string
	Destination string
	Driver      string
	Mode        string
	ReadWrite   bool
}

// NetworkAttachment is one network a container is joined to.
type NetworkAttachment struct {
	Name       string
	NetworkID  string
	IPAddress  string
	Gateway    string
	MacAddress string
	Aliases    []string
}

// PortBinding is one published port.
//
// HostIP and HostPort are empty for a port that is exposed but not published,
// which is a different thing from a port bound to every interface and worth
// telling apart when reviewing exposure.
type PortBinding struct {
	ContainerPort uint16
	Protocol      string
	HostIP        string
	HostPort      string
}

// ComposeProject returns the Docker Compose project this container belongs to,
// if any.
func (c Container) ComposeProject() string { return c.Labels["com.docker.compose.project"] }

// ComposeService returns the Compose service name, if any.
func (c Container) ComposeService() string { return c.Labels["com.docker.compose.service"] }

// Stats is one resource sample for a container.
//
// Values are pre-computed rather than raw cgroup counters: CPU percentage in
// particular requires two readings and knowledge of the host's core count, and
// doing that arithmetic in the client keeps every collector from repeating it.
type Stats struct {
	ContainerID string

	CPUPercent float64
	// MemoryUsage excludes page cache. Docker's raw usage figure includes it,
	// which makes a container look near its limit when it is merely caching
	// file reads — a reliably misleading number.
	MemoryUsage   uint64
	MemoryLimit   uint64
	MemoryPercent float64

	NetworkRxBytes  uint64
	NetworkTxBytes  uint64
	BlockReadBytes  uint64
	BlockWriteBytes uint64

	PIDs uint64
}

// Inventory counts a resource and the space it occupies.
type Inventory struct {
	Count int
	// SizeBytes is the reclaimable or total size where the daemon reports it,
	// and zero where it does not.
	SizeBytes uint64
}

// Event is one daemon event.
type Event struct {
	// Type is the object kind: container, image, network, volume, daemon.
	Type string
	// Action is what happened: start, die, health_status, oom.
	Action string
	// ActorID is the object's id.
	ActorID string
	// ActorName is its human-readable name where the daemon supplies one.
	ActorName  string
	Attributes map[string]string
	Time       time.Time
}

// LogOptions controls a log read.
type LogOptions struct {
	// Follow streams new lines until the context is cancelled.
	Follow bool
	// Tail is how many trailing lines to read first. Zero means none.
	Tail int
	// Since restricts to lines after this time.
	Since time.Time
	// Timestamps prefixes each line with the daemon's timestamp.
	Timestamps bool
}

// Stream is which output a log line came from.
type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

// LogLine is one line of container output.
type LogLine struct {
	ContainerID string
	Stream      Stream
	Time        time.Time
	Message     string
}
