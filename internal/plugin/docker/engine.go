package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/hexane/atlas/internal/platform/errs"
)

// engineClient talks to a real Docker daemon.
type engineClient struct {
	api *client.Client
}

// NewEngineClient connects to the Docker Engine.
//
// API version negotiation is enabled deliberately. Atlas will run beside
// daemons spanning several years of releases, and pinning a version means
// either failing on older hosts or forgoing newer fields. Negotiation costs one
// round trip at startup and removes an entire class of deployment failure.
func NewEngineClient(host string) (Client, error) {
	const op = "docker.NewEngineClient"

	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}

	api, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not create a Docker client").WithOp(op)
	}
	return &engineClient{api: api}, nil
}

func (c *engineClient) Ping(ctx context.Context) (Version, error) {
	const op = "docker.Client.Ping"

	v, err := c.api.ServerVersion(ctx)
	if err != nil {
		return Version{}, errs.Wrap(err, errs.CodeUnavailable, "the Docker daemon did not respond").WithOp(op)
	}
	return Version{
		Version: v.Version, APIVersion: v.APIVersion,
		OS: v.Os, Arch: v.Arch, KernelVersion: v.KernelVersion,
	}, nil
}

func (c *engineClient) Containers(ctx context.Context) ([]Container, error) {
	const op = "docker.Client.Containers"

	// All=true includes stopped containers. A container that exited an hour
	// ago is often the most important thing on the host, and listing only
	// running ones would hide exactly the failure being investigated.
	summaries, err := c.api.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list containers").WithOp(op)
	}

	// The summary lacks health, restart count and timestamps, so each container
	// needs an inspect. Run concurrently: serially this was one round trip per
	// container end to end, which on a host with fifteen containers made a
	// three-second request out of work the daemon answers in fifty
	// milliseconds. Concurrency is bounded so a host with hundreds of
	// containers cannot open hundreds of simultaneous connections to a daemon
	// that is often already the thing under stress.
	results := make([]Container, len(summaries))
	found := make([]bool, len(summaries))

	sem := make(chan struct{}, inspectConcurrency)
	var wg sync.WaitGroup

	for i, s := range summaries {
		wg.Add(1)
		go func() {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			inspected, err := c.api.ContainerInspect(ctx, s.ID)
			if err != nil {
				// A container removed between the list and the inspect is
				// normal on a busy host, not an error worth failing the whole
				// run for. Its slot is simply left unfilled.
				return
			}
			// Each goroutine owns one index, so no lock is needed.
			results[i] = toContainer(inspected, s.Image)
			found[i] = true
		}()
	}
	wg.Wait()

	// Preserving the daemon's ordering keeps the list stable between polls.
	out := make([]Container, 0, len(summaries))
	for i, ok := range found {
		if ok {
			out = append(out, results[i])
		}
	}
	return out, nil
}

// inspectConcurrency bounds simultaneous inspect calls.
//
// Eight is chosen to be well clear of the daemon's default connection limits
// while still collapsing the common case — a host with a few dozen containers
// — into a handful of round trips.
const inspectConcurrency = 8

func toContainer(in container.InspectResponse, imageTag string) Container {
	c := Container{
		ID:      in.ID,
		Name:    strings.TrimPrefix(in.Name, "/"),
		Image:   imageTag,
		ImageID: in.Image,
	}
	if in.Config != nil {
		c.Labels = in.Config.Labels
		if imageTag == "" {
			c.Image = in.Config.Image
		}
	}
	if in.State != nil {
		c.State = State(in.State.Status)
		c.ExitCode = in.State.ExitCode
		c.StartedAt = parseDockerTime(in.State.StartedAt)
		c.FinishedAt = parseDockerTime(in.State.FinishedAt)
		c.Health = HealthNone
		if in.State.Health != nil {
			c.Health = Health(in.State.Health.Status)
		}
	}
	c.RestartCount = in.RestartCount
	c.CreatedAt = parseDockerTime(in.Created)
	return c
}

// parseDockerTime tolerates the daemon's zero value, which is a formatted
// timestamp rather than an empty string.
func parseDockerTime(s string) time.Time {
	if s == "" || strings.HasPrefix(s, "0001-01-01") {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (c *engineClient) Inspect(ctx context.Context, containerID string) (ContainerDetail, error) {
	const op = "docker.Client.Inspect"

	raw, err := c.api.ContainerInspect(ctx, containerID)
	if err != nil {
		if client.IsErrNotFound(err) {
			return ContainerDetail{}, errs.Wrap(err, errs.CodeNotFound, "no such container").
				WithOp(op).WithDetail("container_id", shortID(containerID))
		}
		return ContainerDetail{}, errs.Wrap(err, errs.CodeUnavailable, "could not inspect container").
			WithOp(op).WithDetail("container_id", shortID(containerID))
	}
	return toDetail(raw), nil
}

// toDetail maps the daemon's inspect payload onto Atlas's model.
//
// Every section is nil-guarded. The inspect response is one of the widest
// structs in the Docker API and older daemons omit whole branches of it; a
// direct dereference here would panic on exactly the heterogeneous fleet the
// version negotiation exists to support.
func toDetail(raw container.InspectResponse) ContainerDetail {
	// The image tag is empty here: toContainer falls back to Config.Image,
	// which is what an inspect carries. Only the list has a separate summary
	// with the resolved tag.
	d := ContainerDetail{
		Container: toContainer(raw, ""),
		Platform:  raw.Platform,
		Driver:    raw.Driver,
	}

	if raw.Config != nil {
		d.Entrypoint = raw.Config.Entrypoint
		d.Command = raw.Config.Cmd
		d.WorkingDir = raw.Config.WorkingDir
		d.User = raw.Config.User
		// Config.Env is deliberately not read. See ContainerDetail.
	}

	if raw.HostConfig != nil {
		d.RestartPolicy = RestartPolicy{
			Name:              string(raw.HostConfig.RestartPolicy.Name),
			MaximumRetryCount: raw.HostConfig.RestartPolicy.MaximumRetryCount,
		}
		d.Limits = Limits{
			NanoCPUs:    raw.HostConfig.NanoCPUs,
			CPUShares:   raw.HostConfig.CPUShares,
			MemoryBytes: raw.HostConfig.Memory,
			MemorySwap:  raw.HostConfig.MemorySwap,
			PidsLimit:   derefInt64(raw.HostConfig.PidsLimit),
		}
	}

	for _, m := range raw.Mounts {
		d.Mounts = append(d.Mounts, Mount{
			Type:        string(m.Type),
			Name:        m.Name,
			Source:      m.Source,
			Destination: m.Destination,
			Driver:      m.Driver,
			Mode:        m.Mode,
			ReadWrite:   m.RW,
		})
	}

	if raw.NetworkSettings != nil {
		for name, n := range raw.NetworkSettings.Networks {
			if n == nil {
				continue
			}
			d.Networks = append(d.Networks, NetworkAttachment{
				Name:       name,
				NetworkID:  n.NetworkID,
				IPAddress:  n.IPAddress,
				Gateway:    n.Gateway,
				MacAddress: n.MacAddress,
				Aliases:    n.Aliases,
			})
		}
		sort.Slice(d.Networks, func(i, j int) bool { return d.Networks[i].Name < d.Networks[j].Name })

		for port, bindings := range raw.NetworkSettings.Ports {
			// A port with no bindings is exposed but not published. That is a
			// real and different state from one bound to a host interface, so
			// it is recorded rather than skipped.
			if len(bindings) == 0 {
				d.Ports = append(d.Ports, PortBinding{
					ContainerPort: uint16(port.Int()), //nolint:gosec // port numbers are 0-65535
					Protocol:      port.Proto(),
				})
				continue
			}
			for _, b := range bindings {
				d.Ports = append(d.Ports, PortBinding{
					ContainerPort: uint16(port.Int()), //nolint:gosec // port numbers are 0-65535
					Protocol:      port.Proto(),
					HostIP:        b.HostIP,
					HostPort:      b.HostPort,
				})
			}
		}
		sort.Slice(d.Ports, func(i, j int) bool {
			if d.Ports[i].ContainerPort != d.Ports[j].ContainerPort {
				return d.Ports[i].ContainerPort < d.Ports[j].ContainerPort
			}
			return d.Ports[i].Protocol < d.Ports[j].Protocol
		})
	}

	return d
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (c *engineClient) ContainerStats(ctx context.Context, containerID string) (Stats, error) {
	const op = "docker.Client.ContainerStats"

	// OneShot avoids the streaming endpoint, which holds the connection open
	// for a second to compute a delta. With many containers that would serialise
	// into a multi-second run; computing the CPU delta from the daemon's
	// PreCPUStats gives the same answer immediately.
	resp, err := c.api.ContainerStatsOneShot(ctx, containerID)
	if err != nil {
		return Stats{}, errs.Wrap(err, errs.CodeUnavailable, "could not read container stats").
			WithOp(op).WithDetail("container_id", shortID(containerID))
	}
	defer resp.Body.Close()

	var raw container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return Stats{}, errs.Wrap(err, errs.CodeInternal, "could not decode container stats").
			WithOp(op).WithDetail("container_id", shortID(containerID))
	}
	return toStats(containerID, raw), nil
}

func toStats(containerID string, raw container.StatsResponse) Stats {
	s := Stats{ContainerID: containerID, PIDs: raw.PidsStats.Current}

	// CPU percentage is the container's delta over the system's delta, scaled
	// by the number of cores it may use. Without the core scaling a container
	// pinned to one core of sixteen reads as 6%, which is technically the
	// share of the machine but not the number an operator is looking for.
	cpuDelta := float64(raw.CPUStats.CPUUsage.TotalUsage) - float64(raw.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(raw.CPUStats.SystemUsage) - float64(raw.PreCPUStats.SystemUsage)
	if cpuDelta > 0 && systemDelta > 0 {
		cores := float64(raw.CPUStats.OnlineCPUs)
		if cores == 0 {
			cores = float64(len(raw.CPUStats.CPUUsage.PercpuUsage))
		}
		if cores == 0 {
			cores = 1
		}
		s.CPUPercent = (cpuDelta / systemDelta) * cores * 100
	}

	// Docker's Usage includes page cache. Subtracting it is what makes the
	// figure comparable to the container's memory limit — otherwise a
	// container doing heavy file I/O appears to be at its ceiling when it is
	// merely caching, and would trigger a false alert every time.
	s.MemoryUsage = raw.MemoryStats.Usage
	if cache, ok := raw.MemoryStats.Stats["inactive_file"]; ok && cache < s.MemoryUsage {
		s.MemoryUsage -= cache
	} else if cache, ok := raw.MemoryStats.Stats["total_inactive_file"]; ok && cache < s.MemoryUsage {
		s.MemoryUsage -= cache
	}
	s.MemoryLimit = raw.MemoryStats.Limit
	if s.MemoryLimit > 0 {
		s.MemoryPercent = float64(s.MemoryUsage) / float64(s.MemoryLimit) * 100
	}

	for _, n := range raw.Networks {
		s.NetworkRxBytes += n.RxBytes
		s.NetworkTxBytes += n.TxBytes
	}
	for _, io := range raw.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(io.Op) {
		case "read":
			s.BlockReadBytes += io.Value
		case "write":
			s.BlockWriteBytes += io.Value
		}
	}
	return s
}

func (c *engineClient) Images(ctx context.Context) (Inventory, error) {
	const op = "docker.Client.Images"

	list, err := c.api.ImageList(ctx, image.ListOptions{All: false})
	if err != nil {
		return Inventory{}, errs.Wrap(err, errs.CodeUnavailable, "could not list images").WithOp(op)
	}

	inv := Inventory{Count: len(list)}
	for _, img := range list {
		if img.Size > 0 {
			inv.SizeBytes += uint64(img.Size)
		}
	}
	return inv, nil
}

func (c *engineClient) Networks(ctx context.Context) (int, error) {
	const op = "docker.Client.Networks"

	list, err := c.api.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeUnavailable, "could not list networks").WithOp(op)
	}
	return len(list), nil
}

func (c *engineClient) Volumes(ctx context.Context) (Inventory, error) {
	const op = "docker.Client.Volumes"

	resp, err := c.api.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return Inventory{}, errs.Wrap(err, errs.CodeUnavailable, "could not list volumes").WithOp(op)
	}

	inv := Inventory{Count: len(resp.Volumes)}
	for _, v := range resp.Volumes {
		if v.UsageData != nil && v.UsageData.Size > 0 {
			inv.SizeBytes += uint64(v.UsageData.Size)
		}
	}
	return inv, nil
}

func (c *engineClient) Events(ctx context.Context) (<-chan Event, <-chan error) {
	out := make(chan Event)
	errCh := make(chan error, 1)

	msgs, dockerErrs := c.api.Events(ctx, events.ListOptions{Filters: filters.NewArgs()})

	go func() {
		defer close(out)
		defer close(errCh)

		for {
			select {
			case <-ctx.Done():
				return
			case err, ok := <-dockerErrs:
				if !ok {
					return
				}
				if err != nil && !errs.Is(err, io.EOF) && ctx.Err() == nil {
					errCh <- errs.Wrap(err, errs.CodeUnavailable, "the Docker event stream failed").
						WithOp("docker.Client.Events")
				}
				return
			case msg, ok := <-msgs:
				if !ok {
					return
				}
				select {
				case out <- toEvent(msg):
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, errCh
}

func toEvent(m events.Message) Event {
	e := Event{
		Type:       string(m.Type),
		Action:     string(m.Action),
		ActorID:    m.Actor.ID,
		Attributes: m.Actor.Attributes,
		Time:       time.Unix(0, m.TimeNano),
	}
	if name, ok := m.Actor.Attributes["name"]; ok {
		e.ActorName = name
	}
	return e
}

func (c *engineClient) Logs(ctx context.Context, containerID string, opts LogOptions) (<-chan LogLine, <-chan error) {
	out := make(chan LogLine)
	errCh := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errCh)

		dockerOpts := container.LogsOptions{
			ShowStdout: true, ShowStderr: true,
			Follow:     opts.Follow,
			Timestamps: true, // always requested; the reader strips them
		}
		if opts.Tail > 0 {
			dockerOpts.Tail = itoa(opts.Tail)
		}
		if !opts.Since.IsZero() {
			dockerOpts.Since = opts.Since.Format(time.RFC3339Nano)
		}

		reader, err := c.api.ContainerLogs(ctx, containerID, dockerOpts)
		if err != nil {
			errCh <- errs.Wrap(err, errs.CodeUnavailable, "could not read container logs").
				WithOp("docker.Client.Logs").WithDetail("container_id", shortID(containerID))
			return
		}
		defer reader.Close()

		if err := demultiplex(ctx, reader, containerID, out); err != nil && ctx.Err() == nil {
			errCh <- err
		}
	}()

	return out, errCh
}

// demultiplex decodes Docker's multiplexed log stream.
//
// When a container has no TTY the daemon interleaves stdout and stderr in one
// connection, framed by an 8-byte header: one byte of stream id, three
// reserved, then a big-endian length. Reading it as plain text yields those
// header bytes as mojibake at the start of every line — the classic symptom of
// getting this wrong.
//
// A container *with* a TTY sends raw bytes and no frames, so the reader falls
// back to line-oriented reading when the header does not look like a frame.
func demultiplex(ctx context.Context, r io.Reader, containerID string, out chan<- LogLine) error {
	const op = "docker.demultiplex"

	br := bufio.NewReaderSize(r, 64*1024)
	header := make([]byte, 8)

	for {
		if ctx.Err() != nil {
			return nil
		}

		if _, err := io.ReadFull(br, header); err != nil {
			if errs.Is(err, io.EOF) || errs.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return errs.Wrap(err, errs.CodeUnavailable, "the log stream failed").WithOp(op)
		}

		streamID := header[0]
		if streamID > 2 {
			// Not a frame header: this is a TTY stream. Everything read so far
			// plus the rest of the line is content.
			line, _ := br.ReadString('\n')
			emit(ctx, out, LogLine{
				ContainerID: containerID, Stream: StreamStdout,
				Time: time.Now(), Message: strings.TrimRight(string(header)+line, "\r\n"),
			})
			continue
		}

		size := binary.BigEndian.Uint32(header[4:8])
		if size == 0 {
			continue
		}
		// A malformed or hostile length must not allocate without bound.
		if size > 1<<20 {
			size = 1 << 20
		}

		payload := make([]byte, size)
		if _, err := io.ReadFull(br, payload); err != nil {
			return nil
		}

		stream := StreamStdout
		if streamID == 2 {
			stream = StreamStderr
		}

		for _, raw := range strings.Split(strings.TrimRight(string(payload), "\n"), "\n") {
			ts, msg := splitTimestamp(raw)
			emit(ctx, out, LogLine{
				ContainerID: containerID, Stream: stream, Time: ts, Message: msg,
			})
		}
	}
}

func emit(ctx context.Context, out chan<- LogLine, line LogLine) {
	select {
	case out <- line:
	case <-ctx.Done():
	}
}

// splitTimestamp separates the daemon-prefixed RFC 3339 timestamp from the
// line, so the consumer gets a real time rather than text it must re-parse.
func splitTimestamp(raw string) (time.Time, string) {
	prefix, rest, found := strings.Cut(raw, " ")
	if !found {
		return time.Now(), raw
	}
	ts, err := time.Parse(time.RFC3339Nano, prefix)
	if err != nil {
		return time.Now(), raw
	}
	return ts, rest
}

func (c *engineClient) Close() error {
	if c.api == nil {
		return nil
	}
	return c.api.Close()
}

// shortID truncates a container id for display. Docker's own tooling shows
// twelve characters and operators recognise that form.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

var _ Client = (*engineClient)(nil)
