package v1

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/plugin/docker"
)

// ContainerResponse is a container as the API presents it.
//
// Served live from the daemon rather than from stored samples. A container
// list is inventory, not a time series: an operator asking "what is running"
// wants the truth right now, and reconstructing it from the last collection
// would be up to thirty seconds stale and would miss anything created since.
type ContainerResponse struct {
	ID      string `json:"id"`
	ShortID string `json:"short_id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	ImageID string `json:"image_id,omitempty"`

	State  docker.State  `json:"state"`
	Health docker.Health `json:"health"`
	// ExitCode is meaningful only once the container has stopped.
	ExitCode int `json:"exit_code,omitempty"`
	// RestartCount is the signal a snapshot cannot show: a container running
	// now but restarted forty times is not healthy.
	RestartCount int `json:"restart_count"`

	CreatedAt     time.Time `json:"created_at,omitzero"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	FinishedAt    time.Time `json:"finished_at,omitzero"`
	UptimeSeconds float64   `json:"uptime_seconds,omitempty"`

	ComposeProject string            `json:"compose_project,omitempty"`
	ComposeService string            `json:"compose_service,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// ContainerDetailResponse is one container with its configuration.
//
// Returned only from the single-container endpoint, never from the list. The
// configuration comes from a per-container inspect call, and doing that for
// every container on every list request would multiply the daemon's load by
// the container count to serve fields most callers do not read.
//
// There is no environment block and there will not be one. Environments
// routinely carry database passwords, API tokens and signing keys, and a
// monitoring API that returns them turns every reader of this endpoint into a
// holder of every secret on the host.
type ContainerDetailResponse struct {
	ContainerResponse

	Entrypoint []string `json:"entrypoint,omitempty"`
	Command    []string `json:"command,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
	User       string   `json:"user,omitempty"`

	RestartPolicy RestartPolicyResponse `json:"restart_policy"`
	Limits        LimitsResponse        `json:"limits"`

	Mounts   []ContainerMountResponse   `json:"mounts,omitempty"`
	Networks []ContainerNetworkResponse `json:"networks,omitempty"`
	Ports    []ContainerPortResponse    `json:"ports,omitempty"`

	Platform string `json:"platform,omitempty"`
	Driver   string `json:"driver,omitempty"`
}

// RestartPolicyResponse is the daemon's restart configuration.
type RestartPolicyResponse struct {
	Name              string `json:"name"`
	MaximumRetryCount int    `json:"maximum_retry_count,omitempty"`
}

// LimitsResponse are the container's configured ceilings.
//
// Zero means unlimited — Docker's own convention, and a meaningful answer
// rather than absent data: a container with no memory limit can exhaust the
// host, which is worth showing rather than hiding behind an omitted field.
type LimitsResponse struct {
	NanoCPUs    int64 `json:"nano_cpus"`
	CPUShares   int64 `json:"cpu_shares"`
	MemoryBytes int64 `json:"memory_bytes"`
	MemorySwap  int64 `json:"memory_swap"`
	PidsLimit   int64 `json:"pids_limit"`
}

// ContainerMountResponse is one filesystem mounted into a container.
//
// Named apart from the host MountResponse in inventory.go: these are two
// genuinely different things — a bind or volume inside a container versus a
// mounted filesystem on the host — and one name for both invites exactly the
// confusion this page exists to remove.
type ContainerMountResponse struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination"`
	Driver      string `json:"driver,omitempty"`
	Mode        string `json:"mode,omitempty"`
	ReadWrite   bool   `json:"read_write"`
}

// ContainerNetworkResponse is one network the container is joined to.
type ContainerNetworkResponse struct {
	Name       string   `json:"name"`
	NetworkID  string   `json:"network_id,omitempty"`
	IPAddress  string   `json:"ip_address,omitempty"`
	Gateway    string   `json:"gateway,omitempty"`
	MacAddress string   `json:"mac_address,omitempty"`
	Aliases    []string `json:"aliases,omitempty"`
}

// ContainerPortResponse is one port a container declares.
//
// An empty HostPort means the port is exposed by the image but not published
// to the host — a materially different exposure from one bound to an
// interface, and worth telling apart when reviewing what a host reachable.
type ContainerPortResponse struct {
	ContainerPort uint16 `json:"container_port"`
	Protocol      string `json:"protocol"`
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      string `json:"host_port,omitempty"`
}

// RemoteLogSource proxies a live container-log stream to a specific
// libp2p-connected Agent, for a node [Handler.dockerClient] refuses as
// non-local. Logs have no snapshot form the way inventory does — an
// operator asking to watch a container's output wants it live or not at
// all — so unlike [resolveRemoteInventory], there is no stored fallback:
// nil, or a node with no active libp2p session, both mean remote logs are
// simply unavailable, not degraded.
//
// Implemented by internal/app's fleetPipeline over the AgentOps libp2p
// protocol; see internal/core/transport/libp2ptransport/agentops.go.
type RemoteLogSource interface {
	// ContainerLogs returns the same two-channel shape docker.Client.Logs
	// does, so containerLogSource and followLogs below handle a local and a
	// remote session identically.
	ContainerLogs(ctx context.Context, nodeID, containerID string, opts docker.LogOptions) (<-chan docker.LogLine, <-chan error)
}

// ListContainersResponse is the container collection.
type ListContainersResponse struct {
	// NodeID names the host this inventory describes.
	NodeID     string              `json:"node_id"`
	Containers []ContainerResponse `json:"containers"`
	Total      int                 `json:"total"`
	Running    int                 `json:"running"`
	ObservedAt time.Time           `json:"observed_at"`
	Live       bool                `json:"live"`
}

// ListContainers returns every container on this host, running or not.
//
// Stopped containers are included deliberately: a container that exited an
// hour ago is frequently the most important thing on the host, and a list of
// only running ones hides exactly the failure being investigated.
//
// Unlike the other inventory endpoints in inventory.go, this does not go
// through [resolveInventory]: the local path reads through a live
// [docker.Client] rather than a plain [CollectionSource] method, because
// [GetContainer] and the log endpoints need that same client for operations a
// stored snapshot cannot serve (see the doc on [Handler.dockerClient]). The
// remote path still shares [resolveRemoteInventory] with every other subject,
// so a remote container list gets the same freshness disclosure and the same
// three-way not_found/unavailable/not_implemented distinction as the rest.
func (h *Handler) ListContainers(w http.ResponseWriter, r *http.Request) error {
	scope, err := h.requireScope(r, user.PermissionNodeRead)
	if err != nil {
		return err
	}

	var (
		containers []docker.Container
		meta       inventory.Meta
	)

	if h.deps.Collection != nil && scope.IsLocal(h.deps.Collection.Identity().NodeID) {
		client, err := h.dockerClient(scope)
		if err != nil {
			return err
		}
		containers, err = client.Containers(r.Context())
		if err != nil {
			return err
		}
		meta = inventory.LiveMeta(h.resolvedNode(scope))
	} else {
		var err error
		containers, meta, err = resolveRemoteInventory[[]docker.Container](r.Context(), h, scope.NodeID, inventory.SubjectContainers)
		if err != nil {
			return err
		}
	}

	now := time.Now()
	out := make([]ContainerResponse, 0, len(containers))
	running := 0
	for _, c := range containers {
		if c.State.Running() {
			running++
		}
		out = append(out, presentContainer(c, now))
	}

	httpx.JSON(w, r, http.StatusOK, ListContainersResponse{
		NodeID: meta.NodeID, Containers: out, Total: len(out), Running: running,
		ObservedAt: meta.ObservedAt, Live: meta.Live,
	})
	return nil
}

// GetContainer returns one container.
func (h *Handler) GetContainer(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.GetContainer"

	scope, err := h.requireScope(r, user.PermissionNodeRead)
	if err != nil {
		return err
	}
	client, err := h.dockerClient(scope)
	if err != nil {
		return err
	}

	id := r.PathValue("containerID")
	if id == "" {
		return errs.New(errs.CodeInvalidArgument, "a container id is required").
			WithOp(op).WithDetail("field", "containerID")
	}

	// One inspect answers everything. The daemon resolves a full id, the short
	// form `docker ps` prints, or a name, so there is no need to list every
	// container first to find this one — which is what this did, at a cost of
	// an inspect per container on the host to describe a single container.
	detail, err := client.Inspect(r.Context(), id)
	if err != nil {
		return err
	}

	httpx.JSON(w, r, http.StatusOK, presentDetail(presentContainer(detail.Container, time.Now()), detail))
	return nil
}

func presentDetail(base ContainerResponse, d docker.ContainerDetail) ContainerDetailResponse {
	out := ContainerDetailResponse{
		ContainerResponse: base,
		Entrypoint:        d.Entrypoint,
		Command:           d.Command,
		WorkingDir:        d.WorkingDir,
		User:              d.User,
		Platform:          d.Platform,
		Driver:            d.Driver,
		RestartPolicy: RestartPolicyResponse{
			Name:              d.RestartPolicy.Name,
			MaximumRetryCount: d.RestartPolicy.MaximumRetryCount,
		},
		Limits: LimitsResponse{
			NanoCPUs:    d.Limits.NanoCPUs,
			CPUShares:   d.Limits.CPUShares,
			MemoryBytes: d.Limits.MemoryBytes,
			MemorySwap:  d.Limits.MemorySwap,
			PidsLimit:   d.Limits.PidsLimit,
		},
	}
	for _, m := range d.Mounts {
		out.Mounts = append(out.Mounts, ContainerMountResponse{
			Type: m.Type, Name: m.Name, Source: m.Source, Destination: m.Destination,
			Driver: m.Driver, Mode: m.Mode, ReadWrite: m.ReadWrite,
		})
	}
	for _, n := range d.Networks {
		out.Networks = append(out.Networks, ContainerNetworkResponse{
			Name: n.Name, NetworkID: n.NetworkID, IPAddress: n.IPAddress,
			Gateway: n.Gateway, MacAddress: n.MacAddress, Aliases: n.Aliases,
		})
	}
	for _, p := range d.Ports {
		out.Ports = append(out.Ports, ContainerPortResponse{
			ContainerPort: p.ContainerPort, Protocol: p.Protocol,
			HostIP: p.HostIP, HostPort: p.HostPort,
		})
	}
	return out
}

// findContainer resolves the id an operator actually has to hand: the full
// id, the short form `docker ps` prints, or the name.
func findContainer(containers []docker.Container, id string) (docker.Container, bool) {
	for _, c := range containers {
		if c.ID == id || shortID(c.ID) == id || c.Name == id {
			return c, true
		}
	}
	return docker.Container{}, false
}

func presentContainer(c docker.Container, now time.Time) ContainerResponse {
	out := ContainerResponse{
		ID: c.ID, ShortID: shortID(c.ID), Name: c.Name,
		Image: c.Image, ImageID: c.ImageID,
		State: c.State, Health: c.Health,
		ExitCode: c.ExitCode, RestartCount: c.RestartCount,
		CreatedAt: c.CreatedAt, StartedAt: c.StartedAt, FinishedAt: c.FinishedAt,
		ComposeProject: c.ComposeProject(), ComposeService: c.ComposeService(),
		Labels: c.Labels,
	}
	if c.State.Running() && !c.StartedAt.IsZero() {
		out.UptimeSeconds = now.Sub(c.StartedAt).Seconds()
	}
	return out
}

// Log reading limits.
const (
	defaultLogTail = 200
	maxLogTail     = 5000
)

// LogLineResponse is one line of container output.
type LogLineResponse struct {
	Time    time.Time     `json:"time"`
	Stream  docker.Stream `json:"stream"`
	Message string        `json:"message"`
}

// ContainerLogsResponse is a batch of log lines.
type ContainerLogsResponse struct {
	ContainerID string            `json:"container_id"`
	Lines       []LogLineResponse `json:"lines"`
	Total       int               `json:"total"`
}

// ContainerLogs returns the tail of a container's logs.
//
// This is the non-streaming form: read the last N lines and return. Live
// following arrives over WebSocket, but a plain request that returns promptly
// is what a `curl` during an incident needs, and what the UI uses to populate
// a log view before subscribing to updates.
func (h *Handler) ContainerLogs(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ContainerLogs"

	// node.logs.read, not node.read: docs/adr/0011-deferred-rbac.md names
	// container log content as uniquely sensitive — the single most
	// sensitive thing the API serves — so it is gated separately from the
	// rest of a node's inventory.
	scope, err := h.requireScope(r, user.PermissionNodeLogsRead)
	if err != nil {
		return err
	}
	id := r.PathValue("containerID")
	if id == "" {
		return errs.New(errs.CodeInvalidArgument, "a container id is required").
			WithOp(op).WithDetail("field", "containerID")
	}

	tail := defaultLogTail
	if raw := r.URL.Query().Get("tail"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n <= 0 {
			return errs.New(errs.CodeInvalidArgument, "tail must be a positive number").
				WithOp(op).WithDetail("field", "tail")
		}
		// A cap, not an error: an operator asking for everything gets as much
		// as is safe to serve rather than a rejection.
		tail = min(n, maxLogTail)
	}

	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, parseErr := time.Parse(time.RFC3339, raw)
		if parseErr != nil {
			return errs.New(errs.CodeInvalidArgument, "since must be an RFC 3339 timestamp").
				WithOp(op).WithDetail("field", "since")
		}
		since = t
	}

	lines, errCh, err := h.containerLogSource(r.Context(), scope, id, docker.LogOptions{
		Follow: false, Tail: tail, Since: since,
	})
	if err != nil {
		return err
	}

	out := make([]LogLineResponse, 0, tail)
	for line := range lines {
		out = append(out, LogLineResponse{
			Time: line.Time, Stream: line.Stream, Message: line.Message,
		})
	}
	// The error arrives after the channel closes; a failure part-way through
	// still returns the lines already read, which during an incident is more
	// useful than an error page.
	if streamErr := <-errCh; streamErr != nil && len(out) == 0 {
		return streamErr
	}

	httpx.JSON(w, r, http.StatusOK, ContainerLogsResponse{
		ContainerID: id, Lines: out, Total: len(out),
	})
	return nil
}

// Tuning for the live-follow WebSocket.
const (
	// followPingInterval is how often the server probes a silent connection.
	// A container that has gone quiet is the common case, not the exception,
	// and without a ping there is no way to distinguish "nothing to say" from
	// "the network path died" until the OS eventually times out the socket.
	followPingInterval = 20 * time.Second
	followPingTimeout  = 10 * time.Second
	// followWriteTimeout bounds one line's delivery. A browser tab put to
	// sleep by the OS stops reading but does not close the socket, and
	// without a write deadline that leaves the goroutine — and the Docker log
	// stream feeding it — running for a tab nobody will ever look at again.
	followWriteTimeout = 10 * time.Second
	// maxFollowDuration bounds a single session. An operator who is still
	// watching after this long just reconnects, which the frontend does
	// without asking; a WebSocket with no upper bound on its lifetime is a
	// resource leak with extra steps, the same reasoning behind the
	// cardinality budget elsewhere in Atlas.
	maxFollowDuration = 6 * time.Hour
)

// ContainerLogsFollow streams a container's log lines as they are written.
//
// This is the live counterpart to [Handler.ContainerLogs]: that endpoint
// answers "what did this container just say", and this one keeps the answer
// current for as long as an operator is watching. It requires a genuine
// WebSocket upgrade — see [httpx.IsWebSocketUpgrade] — and is dispatched
// through [httpx.StreamMiddleware] rather than the timeout-bounded base
// chain; see internal/api/router.go.
//
// Every failure that can still be reported as a normal JSON error is checked
// before the handshake: an invalid tail parameter, an unknown container,
// Docker being absent. Once [websocket.Accept] succeeds the protocol has
// switched, and everything from there on is reported, if at all, as a
// WebSocket close code rather than an HTTP status.
func (h *Handler) ContainerLogsFollow(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ContainerLogsFollow"

	if !httpx.IsWebSocketUpgrade(r) {
		return errs.New(errs.CodeInvalidArgument, "this endpoint requires a WebSocket upgrade").
			WithOp(op)
	}

	// node.logs.read — see the same check and its doc in [Handler.ContainerLogs].
	// This is the endpoint docs/adr/0011-deferred-rbac.md calls out by name
	// as the highest-risk one if authentication were ever missing from the
	// streaming chain: it dispatches through [httpx.StreamMiddleware], not
	// [httpx.BaseMiddleware], and a browser's WebSocket handshake has no
	// Authorization header for a fallback check to use — the session cookie
	// [session.AuthMiddleware] resolves ahead of this handler is the only
	// mechanism available to it.
	scope, err := h.requireScope(r, user.PermissionNodeLogsRead)
	if err != nil {
		return err
	}
	id := r.PathValue("containerID")
	if id == "" {
		return errs.New(errs.CodeInvalidArgument, "a container id is required").
			WithOp(op).WithDetail("field", "containerID")
	}

	tail := defaultLogTail
	if raw := r.URL.Query().Get("tail"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n <= 0 {
			return errs.New(errs.CodeInvalidArgument, "tail must be a positive number").
				WithOp(op).WithDetail("field", "tail")
		}
		tail = min(n, maxLogTail)
	}

	// Pre-upgrade container resolution only happens for the local case: it
	// needs a synchronous Containers() call this Handler can make directly,
	// and it is what lets an operator pass a short id or name. A remote
	// session has no equivalent cheap round trip before opening the real
	// one, so the Agent resolves (or rejects) the id itself, as the first
	// thing that can go wrong in the stream — see containerLogSource.
	var client docker.Client
	isLocal := h.deps.Collection != nil && scope.IsLocal(h.deps.Collection.Identity().NodeID)
	if isLocal {
		var err error
		client, err = h.dockerClient(scope)
		if err != nil {
			return err
		}
		containers, err := client.Containers(r.Context())
		if err != nil {
			return err
		}
		target, ok := findContainer(containers, id)
		if !ok {
			return errs.New(errs.CodeNotFound, "container not found").
				WithOp(op).WithDetail("container", id)
		}
		id = target.ID
	} else if h.deps.RemoteLogs == nil {
		return errs.New(errs.CodeUnavailable, "remote container logs are not available on this control plane").WithOp(op)
	}

	conn, err := websocket.Accept(w, r, websocketAcceptOptions(h.deps.Config.Server.AllowedOrigins))
	if err != nil {
		// Accept has already written the failed handshake response; there is
		// nothing left for httpx's normal error path to add.
		return nil
	}
	// Always safe, and the only backstop against every early return below —
	// a peer that vanishes mid-Ping, a write that fails, ctx expiring —
	// leaving the connection open.
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), maxFollowDuration)
	defer cancel()
	// This handler never expects an incoming message; CloseRead both answers
	// control frames (ping/pong/close) on our behalf and cancels ctx the
	// moment the peer disconnects, which is how a closed browser tab is
	// noticed at all — and, for a remote session, is what propagates all the
	// way to the Agent's Docker call; see RequestContainerLogs.
	ctx = conn.CloseRead(ctx)

	var lines <-chan docker.LogLine
	var errCh <-chan error
	if isLocal {
		lines, errCh = client.Logs(ctx, id, docker.LogOptions{Follow: true, Tail: tail})
	} else {
		lines, errCh = h.deps.RemoteLogs.ContainerLogs(ctx, scope.NodeID, id, docker.LogOptions{Follow: true, Tail: tail})
	}
	h.followLogs(ctx, conn, lines, errCh)
	return nil
}

// containerLogSource resolves where containerID's logs come from: the local
// Docker client, or a remote Agent via RemoteLogs. Mirrors
// resolveInventory's local/remote split, but proxies live rather than
// reading a stored snapshot — see RemoteLogSource's doc.
func (h *Handler) containerLogSource(ctx context.Context, scope inventory.Scope, containerID string, opts docker.LogOptions) (<-chan docker.LogLine, <-chan error, error) {
	const op = "v1.Handler.containerLogSource"

	if h.deps.Collection != nil && scope.IsLocal(h.deps.Collection.Identity().NodeID) {
		client, err := h.dockerClient(scope)
		if err != nil {
			return nil, nil, err
		}
		lines, errCh := client.Logs(ctx, containerID, opts)
		return lines, errCh, nil
	}

	if h.deps.RemoteLogs == nil {
		return nil, nil, errs.New(errs.CodeUnavailable, "remote container logs are not available on this control plane").WithOp(op)
	}
	lines, errCh := h.deps.RemoteLogs.ContainerLogs(ctx, scope.NodeID, containerID, opts)
	return lines, errCh, nil
}

// followLogs streams lines to conn until the connection, the log stream, or
// ctx ends. lines/errCh use the same two-channel shape docker.Client.Logs
// and RemoteLogSource.ContainerLogs both produce, so this one loop serves a
// local and a remote session identically.
func (h *Handler) followLogs(ctx context.Context, conn *websocket.Conn, lines <-chan docker.LogLine, errCh <-chan error) {
	ping := time.NewTicker(followPingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "closing")
			return

		case <-ping.C:
			pingCtx, cancel := context.WithTimeout(ctx, followPingTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return // the peer is gone; Ping has already closed the connection
			}

		case streamErr, open := <-errCh:
			if open && streamErr != nil {
				_ = conn.Close(websocket.StatusInternalError, "the log stream failed")
			} else {
				_ = conn.Close(websocket.StatusNormalClosure, "the container's log stream ended")
			}
			return

		case line, open := <-lines:
			if !open {
				// errCh is the authority on why the stream ended; deferring to
				// it avoids a race between the two channels closing together.
				continue
			}
			writeCtx, cancel := context.WithTimeout(ctx, followWriteTimeout)
			err := wsjson.Write(writeCtx, conn, LogLineResponse{
				Time: line.Time, Stream: line.Stream, Message: line.Message,
			})
			cancel()
			if err != nil {
				return // a slow or gone client; Write has already closed the connection
			}
		}
	}
}

// websocketAcceptOptions translates Atlas's CORS allow-list into the
// WebSocket library's origin-checking convention, which matches on the
// origin's host rather than the full origin string CORS uses.
//
// A WebSocket handshake bypasses the browser's same-origin policy entirely —
// that is what makes cross-site WebSocket hijacking a real attack — so this
// check is not optional the way CORS headers are; [websocket.Accept] enforces
// it itself rather than merely advising the browser.
func websocketAcceptOptions(allowedOrigins []string) *websocket.AcceptOptions {
	if slices.Contains(allowedOrigins, "*") {
		// The library deliberately has no wildcard pattern, to force this
		// choice to be visible in the code rather than hidden in a config
		// value. Passing it through as a pattern would silently no-op.
		return &websocket.AcceptOptions{InsecureSkipVerify: true}
	}

	patterns := make([]string, 0, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		if u, err := url.Parse(origin); err == nil && u.Host != "" {
			patterns = append(patterns, u.Host)
		}
	}
	return &websocket.AcceptOptions{OriginPatterns: patterns}
}

// dockerClient returns the live Docker client, or a typed error explaining why
// containers cannot be served.
//
// `not_implemented` rather than `unavailable` when Docker is absent: the
// endpoint exists in the API surface but this deployment has no Docker to
// describe, which is a permanent condition on that host and not something a
// retry will fix.
// dockerClient resolves the Docker client for a scope.
//
// Scoped like every other inventory: the daemon Atlas talks to is the one on
// its own host, so any other node is refused rather than silently answered
// with local containers. Once agents push container inventory, this is where a
// remote lookup goes.
func (h *Handler) dockerClient(scope inventory.Scope) (docker.Client, error) {
	const op = "v1.Handler.dockerClient"

	if h.deps.Collection == nil {
		return nil, errs.New(errs.CodeUnavailable, "collection is not configured").WithOp(op)
	}
	if !scope.IsLocal(h.deps.Collection.Identity().NodeID) {
		return nil, inventory.ErrRemoteUnavailable(op, scope.NodeID, "container inventory")
	}
	client := h.deps.Collection.DockerClient()
	if client == nil {
		return nil, errs.New(errs.CodeNotImplemented,
			"Docker is not available on this host").WithOp(op)
	}
	return client, nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
