package v1_test

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/hexane/atlas/internal/api"
	v1 "github.com/hexane/atlas/internal/api/v1"
	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/core/scheduler"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/hostid"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/plugin/cron"
	"github.com/hexane/atlas/internal/plugin/docker"
	"github.com/hexane/atlas/internal/plugin/ports"
	"github.com/hexane/atlas/internal/plugin/process"
	"github.com/hexane/atlas/internal/plugin/service"
	"github.com/hexane/atlas/internal/plugin/system"
	"github.com/hexane/atlas/internal/storage/metric"
)

// Live log following is the one endpoint in Atlas that does not speak the
// JSON envelope every other test in this codebase can assume: the protocol
// switches to a WebSocket, and everything reportable afterward is a close
// code rather than an HTTP status. These tests exercise a real hijacked
// connection over an actual TCP listener — an httptest.ResponseRecorder
// cannot hijack — which is also what proves [httpx.StreamMiddleware] is
// correctly exempting this route from the request-wide timeout: a slow test
// server that still completes within the test's own deadline is the evidence.

const testContainerID = "c0ffee000000000000000000000000000000000000000000000000000000"

func newFollowTestServer(t *testing.T, client docker.Client) *httptest.Server {
	t.Helper()

	cfg := config.Default()
	reg := health.NewRegistry(nil)
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config:     &cfg,
		Health:     reg,
		Pool:       postgres.NewPool(cfg.Database, nil),
		Bus:        bus,
		Collection: fakeCollection{docker: client},
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + path
}

func TestContainerLogsFollowRequiresAnActualUpgrade(t *testing.T) {
	t.Parallel()

	srv := newFollowTestServer(t, &fakeDockerClient{containers: []docker.Container{{ID: testContainerID, Name: "web"}}})

	// A plain GET — no Upgrade/Connection headers — must be answered as a
	// normal JSON request, not left hanging waiting for a handshake that will
	// never come.
	resp, err := http.Get(srv.URL + "/api/v1/containers/web/logs/follow")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestContainerLogsFollowWithoutDockerIsNotImplemented(t *testing.T) {
	t.Parallel()

	srv := newFollowTestServer(t, nil) // nil DockerClient(): Docker is absent on this host

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, wsURL(srv, "/api/v1/containers/web/logs/follow"), nil)
	if err == nil {
		t.Fatal("Dial succeeded with no Docker client configured")
	}
	if resp == nil || resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %v, want %d", statusOf(resp), http.StatusNotImplemented)
	}
}

func TestContainerLogsFollowUnknownContainerIsNotFound(t *testing.T) {
	t.Parallel()

	srv := newFollowTestServer(t, &fakeDockerClient{containers: nil})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, wsURL(srv, "/api/v1/containers/does-not-exist/logs/follow"), nil)
	if err == nil {
		t.Fatal("Dial succeeded for a container that does not exist")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %v, want %d", statusOf(resp), http.StatusNotFound)
	}
}

func TestContainerLogsFollowRejectsCrossOrigin(t *testing.T) {
	t.Parallel()

	// The default configuration allows no cross-origin callers, and a
	// WebSocket handshake bypasses the browser's own same-origin enforcement
	// entirely — this check is the only thing standing between Atlas and
	// cross-site WebSocket hijacking, so it must actually reject.
	srv := newFollowTestServer(t, &fakeDockerClient{containers: []docker.Container{{ID: testContainerID, Name: "web"}}})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, wsURL(srv, "/api/v1/containers/web/logs/follow"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}},
	})
	if err == nil {
		t.Fatal("Dial succeeded from a cross-origin caller")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %v, want %d", statusOf(resp), http.StatusForbidden)
	}
}

func TestContainerLogsFollowStreamsTailThenLiveLines(t *testing.T) {
	t.Parallel()

	fc := &fakeDockerClient{
		containers: []docker.Container{{ID: testContainerID, Name: "web"}},
		lines:      make(chan docker.LogLine, 4),
		logsErr:    make(chan error, 1),
	}
	fc.lines <- docker.LogLine{Stream: docker.StreamStdout, Time: time.Now(), Message: "server started"}

	srv := newFollowTestServer(t, fc)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Resolved by name, the same as a curl during an incident would use — not
	// just by the id the fake happens to carry.
	conn, resp, err := websocket.Dial(ctx, wsURL(srv, "/api/v1/containers/web/logs/follow?tail=50"), nil)
	if err != nil {
		t.Fatalf("Dial: %v (status %v)", err, statusOf(resp))
	}
	defer conn.CloseNow()

	var first v1LogLine
	if err := wsjson.Read(ctx, conn, &first); err != nil {
		t.Fatalf("reading the tail line: %v", err)
	}
	if first.Message != "server started" {
		t.Errorf("message = %q, want %q", first.Message, "server started")
	}

	// A line published after the connection is already open is what makes
	// this "follow" rather than a tail: it must arrive without a reconnect.
	fc.lines <- docker.LogLine{Stream: docker.StreamStderr, Time: time.Now(), Message: "connection refused"}
	var second v1LogLine
	if err := wsjson.Read(ctx, conn, &second); err != nil {
		t.Fatalf("reading the live line: %v", err)
	}
	if second.Message != "connection refused" || second.Stream != "stderr" {
		t.Errorf("got %+v, want the live stderr line", second)
	}

	// The daemon ending the stream cleanly (container removed, log rotated
	// out from under it) must close with a normal status, not look like a
	// failure to the client.
	close(fc.lines)
	close(fc.logsErr)

	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("close status = %v (err %v), want StatusNormalClosure", websocket.CloseStatus(err), err)
	}
}

func TestContainerLogsFollowReportsStreamFailureAsInternalError(t *testing.T) {
	t.Parallel()

	fc := &fakeDockerClient{
		containers: []docker.Container{{ID: testContainerID, Name: "web"}},
		lines:      make(chan docker.LogLine),
		logsErr:    make(chan error, 1),
	}
	srv := newFollowTestServer(t, fc)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wsURL(srv, "/api/v1/containers/web/logs/follow"), nil)
	if err != nil {
		t.Fatalf("Dial: %v (status %v)", err, statusOf(resp))
	}
	defer conn.CloseNow()

	fc.logsErr <- errFakeDockerUnavailable

	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Errorf("close status = %v (err %v), want StatusInternalError", websocket.CloseStatus(err), err)
	}
}

func TestContainerLogsFollowEndsWhenTheClientDisconnects(t *testing.T) {
	t.Parallel()

	// Nothing observes this directly — the point is that closing the client
	// side must not hang the test. If followLogs failed to notice the
	// disconnect, the fake's log goroutine would run until ctx's six-hour
	// ceiling, and this test would time out rather than fail cleanly.
	fc := &fakeDockerClient{
		containers: []docker.Container{{ID: testContainerID, Name: "web"}},
		lines:      make(chan docker.LogLine),
		logsErr:    make(chan error, 1),
	}
	srv := newFollowTestServer(t, fc)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wsURL(srv, "/api/v1/containers/web/logs/follow"), nil)
	if err != nil {
		t.Fatalf("Dial: %v (status %v)", err, statusOf(resp))
	}
	if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("client Close: %v", err)
	}
}

func statusOf(resp *http.Response) any {
	if resp == nil {
		return nil
	}
	return resp.StatusCode
}

// v1LogLine mirrors v1.LogLineResponse's wire shape without importing the
// unexported field layout — this package only asserts on JSON.
type v1LogLine struct {
	Time    time.Time `json:"time"`
	Stream  string    `json:"stream"`
	Message string    `json:"message"`
}

var errFakeDockerUnavailable = stderrors.New("simulated docker daemon failure")

// fakeDockerClient implements [docker.Client] with just enough behaviour to
// drive the follow handler: a fixed container list, and test-controlled
// lines/errCh for Logs.
func TestGetContainerIncludesConfiguration(t *testing.T) {
	t.Parallel()

	fc := &fakeDockerClient{
		containers: []docker.Container{{ID: "abc123def456", Name: "web", State: docker.StateRunning}},
		detail: docker.ContainerDetail{
			Container:     docker.Container{ID: "abc123def456", Name: "web", State: docker.StateRunning},
			Command:       []string{"nginx", "-g", "daemon off;"},
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
			Limits:        docker.Limits{MemoryBytes: 512 << 20},
			Mounts:        []docker.Mount{{Type: "volume", Name: "data", Destination: "/var/lib/data", ReadWrite: true}},
			Networks:      []docker.NetworkAttachment{{Name: "bridge", IPAddress: "172.17.0.2"}},
			Ports:         []docker.PortBinding{{ContainerPort: 80, Protocol: "tcp", HostIP: "0.0.0.0", HostPort: "8080"}},
		},
	}
	srv := newFollowTestServer(t, fc)

	resp, err := http.Get(srv.URL + "/api/v1/containers/web")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got v1.ContainerDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Name != "web" {
		t.Errorf("name = %q, want web", got.Name)
	}
	if len(got.Command) != 3 {
		t.Errorf("command = %v, want 3 elements", got.Command)
	}
	if got.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("restart policy = %q, want unless-stopped", got.RestartPolicy.Name)
	}
	if got.Limits.MemoryBytes != 512<<20 {
		t.Errorf("memory limit = %d, want %d", got.Limits.MemoryBytes, 512<<20)
	}
	if len(got.Mounts) != 1 || got.Mounts[0].Destination != "/var/lib/data" {
		t.Errorf("mounts = %+v", got.Mounts)
	}
	if len(got.Networks) != 1 || got.Networks[0].IPAddress != "172.17.0.2" {
		t.Errorf("networks = %+v", got.Networks)
	}
	if len(got.Ports) != 1 || got.Ports[0].HostPort != "8080" {
		t.Errorf("ports = %+v", got.Ports)
	}
}

// An inspect that fails must surface as an error rather than a half-empty
// container. There is no list to fall back on now that the endpoint resolves
// the container with a single call.
func TestGetContainerReportsInspectFailure(t *testing.T) {
	t.Parallel()

	fc := &fakeDockerClient{
		containers: []docker.Container{{ID: "abc123def456", Name: "web", State: docker.StateRunning}},
		detailErr:  stderrors.New("no such container"),
	}
	srv := newFollowTestServer(t, fc)

	resp, err := http.Get(srv.URL + "/api/v1/containers/web")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200, want an error status when inspect fails")
	}
}

// Environment variables must never reach the API. They carry credentials,
// and this response shape is the defense regardless of who is authenticated
// — a container's env block is not something any role should be handed.
func TestContainerDetailHasNoEnvironmentField(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(v1.ContainerDetailResponse{})
	for i := range typ.NumField() {
		name := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(name, "env") {
			t.Fatalf("ContainerDetailResponse exposes %q; environments carry secrets and must not be served",
				typ.Field(i).Name)
		}
	}
}

type fakeDockerClient struct {
	containers []docker.Container
	detail     docker.ContainerDetail
	detailErr  error
	lines      chan docker.LogLine
	logsErr    chan error
}

func (f *fakeDockerClient) Ping(context.Context) (docker.Version, error) {
	return docker.Version{}, nil
}

func (f *fakeDockerClient) Containers(context.Context) ([]docker.Container, error) {
	return f.containers, nil
}

func (f *fakeDockerClient) ContainerStats(context.Context, string) (docker.Stats, error) {
	return docker.Stats{}, nil
}

func (f *fakeDockerClient) Inspect(context.Context, string) (docker.ContainerDetail, error) {
	return f.detail, f.detailErr
}

func (f *fakeDockerClient) Images(context.Context) (docker.Inventory, error) {
	return docker.Inventory{}, nil
}
func (f *fakeDockerClient) Networks(context.Context) (int, error) { return 0, nil }
func (f *fakeDockerClient) Volumes(context.Context) (docker.Inventory, error) {
	return docker.Inventory{}, nil
}

func (f *fakeDockerClient) Events(context.Context) (<-chan docker.Event, <-chan error) {
	return nil, nil
}

func (f *fakeDockerClient) Logs(ctx context.Context, _ string, _ docker.LogOptions) (<-chan docker.LogLine, <-chan error) {
	return f.lines, f.logsErr
}

func (f *fakeDockerClient) Close() error { return nil }

var _ docker.Client = (*fakeDockerClient)(nil)

// fakeCollection implements [v1.CollectionSource] with zero values for
// everything this test package does not exercise. Only DockerClient carries
// real behaviour.
type fakeCollection struct {
	serviceGraph  *service.Graph
	activePlugins []string
	docker        docker.Client
}

func (f fakeCollection) Repository() *metric.Repository               { return nil }
func (f fakeCollection) CollectorHealth() []scheduler.CollectorHealth { return nil }
func (f fakeCollection) PluginStates() []plugin.State                 { return nil }
func (f fakeCollection) SchedulerStats() scheduler.Stats              { return scheduler.Stats{} }
func (f fakeCollection) IngestStats() metric.Stats                    { return metric.Stats{} }
func (f fakeCollection) Identity() hostid.Identity                    { return hostid.Identity{} }
func (f fakeCollection) DockerClient() docker.Client                  { return f.docker }
func (f fakeCollection) PluginActive(id string) bool {
	if slices.Contains(f.activePlugins, id) {
		return true
	}
	return f.docker != nil
}

func (f fakeCollection) Processes(context.Context, inventory.Scope) ([]process.Process, error) {
	return nil, nil
}
func (f fakeCollection) Services(context.Context, inventory.Scope) ([]service.Unit, error) {
	return nil, nil
}

func (f fakeCollection) ServiceGraph(context.Context, inventory.Scope) (*service.Graph, error) {
	return f.serviceGraph, nil
}
func (f fakeCollection) CronJobs(context.Context, inventory.Scope) ([]cron.Job, error) {
	return nil, nil
}
func (f fakeCollection) Ports(context.Context, inventory.Scope) ([]ports.Listener, error) {
	return nil, nil
}
func (f fakeCollection) Mounts(context.Context, inventory.Scope) ([]system.MountInfo, error) {
	return nil, nil
}

// --- Remote container logs (RemoteLogSource) --------------------------
//
// fakeCollection.Identity() always returns an empty NodeID, so any request
// carrying a non-empty ?node= is remote per Scope.IsLocal — no separate
// "remote fake" is needed, only a RemoteLogSource.

type fakeRemoteLogSource struct {
	mu     sync.Mutex
	lines  chan docker.LogLine
	errCh  chan error
	ctx    context.Context
	nodeID string
}

func (f *fakeRemoteLogSource) ContainerLogs(ctx context.Context, nodeID, containerID string, opts docker.LogOptions) (<-chan docker.LogLine, <-chan error) {
	f.mu.Lock()
	f.ctx, f.nodeID = ctx, nodeID
	f.mu.Unlock()
	return f.lines, f.errCh
}

func (f *fakeRemoteLogSource) calledCtx() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctx
}

func newRemoteFollowTestServer(t *testing.T, remote v1.RemoteLogSource) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	reg := health.NewRegistry(nil)
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })
	handler := api.New(api.Deps{
		Config:     &cfg,
		Health:     reg,
		Pool:       postgres.NewPool(cfg.Database, nil),
		Bus:        bus,
		Collection: fakeCollection{},
		RemoteLogs: remote,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func waitForCall(t *testing.T, remote *fakeRemoteLogSource) context.Context {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ctx := remote.calledCtx(); ctx != nil {
			return ctx
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("RemoteLogSource.ContainerLogs was never called")
	return nil
}

// #13: closing the browser's WebSocket must cancel the context handed to
// RemoteLogSource — the first hop of Browser -> Server -> Agent -> Docker
// cancellation.
func TestContainerLogsFollowRemoteCancellationReachesRemoteLogSource(t *testing.T) {
	t.Parallel()

	remote := &fakeRemoteLogSource{lines: make(chan docker.LogLine), errCh: make(chan error, 1)}
	srv := newRemoteFollowTestServer(t, remote)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL(srv, "/api/v1/containers/c1/logs/follow?node=remote-node"), nil)
	if err != nil {
		t.Fatalf("Dial: %v (status %v)", err, statusOf(resp))
	}

	remoteCtx := waitForCall(t, remote)
	if remote.nodeID != "remote-node" {
		t.Errorf("nodeID passed to RemoteLogSource = %q, want %q", remote.nodeID, "remote-node")
	}
	if remoteCtx.Err() != nil {
		t.Fatal("context was already cancelled before the browser closed anything")
	}

	conn.Close(websocket.StatusNormalClosure, "done")

	select {
	case <-remoteCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("closing the browser WebSocket never cancelled the RemoteLogSource context")
	}
}

// #16: an abrupt disconnect (no clean close frame) must cancel the same way
// a graceful close does.
func TestContainerLogsFollowRemoteAbruptDisconnectCancels(t *testing.T) {
	t.Parallel()

	remote := &fakeRemoteLogSource{lines: make(chan docker.LogLine), errCh: make(chan error, 1)}
	srv := newRemoteFollowTestServer(t, remote)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL(srv, "/api/v1/containers/c1/logs/follow?node=remote-node"), nil)
	if err != nil {
		t.Fatalf("Dial: %v (status %v)", err, statusOf(resp))
	}

	remoteCtx := waitForCall(t, remote)
	conn.CloseNow() // abrupt: no close handshake

	select {
	case <-remoteCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("an abrupt disconnect never cancelled the RemoteLogSource context")
	}
}

// Remote lines delivered by RemoteLogSource must reach the browser
// unchanged — proves the wiring, not just the cancellation path.
func TestContainerLogsFollowRemoteDeliversLines(t *testing.T) {
	t.Parallel()

	remote := &fakeRemoteLogSource{lines: make(chan docker.LogLine, 1), errCh: make(chan error, 1)}
	remote.lines <- docker.LogLine{Stream: docker.StreamStdout, Time: time.Now(), Message: "remote line"}
	srv := newRemoteFollowTestServer(t, remote)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL(srv, "/api/v1/containers/c1/logs/follow?node=remote-node"), nil)
	if err != nil {
		t.Fatalf("Dial: %v (status %v)", err, statusOf(resp))
	}
	defer conn.CloseNow()

	var line v1LogLine
	if err := wsjson.Read(ctx, conn, &line); err != nil {
		t.Fatalf("reading the remote line: %v", err)
	}
	if line.Message != "remote line" {
		t.Errorf("message = %q, want %q", line.Message, "remote line")
	}
}

// No RemoteLogSource configured (nil, the same "not wired in" convention
// every other optional Deps field uses) — a remote node's logs must report
// unavailable, not panic or hang. This is also the HTTPS-only/legacy-agent
// case: no libp2p session means no RemoteLogs entry ever reaches this node.
func TestContainerLogsFollowRemoteUnavailableWithNoRemoteLogSource(t *testing.T) {
	t.Parallel()

	srv := newRemoteFollowTestServer(t, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(srv, "/api/v1/containers/c1/logs/follow?node=remote-node"), nil)
	if err == nil {
		t.Fatal("Dial succeeded with no RemoteLogSource configured")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %v, want %d", statusOf(resp), http.StatusServiceUnavailable)
	}
}
