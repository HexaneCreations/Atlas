package docker

import (
	"context"
	"errors"
	"sync"
	"time"
)

// fakeClient is a Docker daemon whose every response the test controls.
//
// The states that matter most are the ones a real daemon will not produce on
// demand: a container that disappears between the list and the stats read, an
// image with no health check, a memory limit of zero, a stats payload whose
// CPU counters have not advanced. Each of those is a division by zero or a
// misleading number if handled wrongly, and each is trivial here.
type fakeClient struct {
	mu sync.Mutex

	version    Version
	containers []Container
	stats      map[string]Stats
	images     Inventory
	networks   int
	volumes    Inventory

	pingErr       error
	containersErr error
	statsErr      map[string]error
	details       map[string]ContainerDetail
	detailErr     map[string]error
	imagesErr     error
	networksErr   error
	volumesErr    error

	// events is delivered on Events; closing it ends the stream.
	events    chan Event
	eventsErr chan error

	blockFor time.Duration
	closed   bool
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		version: Version{Version: "28.3.3", APIVersion: "1.51", OS: "linux", Arch: "arm64"},
		containers: []Container{{
			ID: "abc123def456789", Name: "web", Image: "nginx:1.27", ImageID: "sha256:aaa",
			State: StateRunning, Health: HealthHealthy,
			CreatedAt:    time.Now().Add(-48 * time.Hour),
			StartedAt:    time.Now().Add(-2 * time.Hour),
			RestartCount: 3,
			Labels: map[string]string{
				"com.docker.compose.project": "shop",
				"com.docker.compose.service": "web",
			},
		}},
		stats: map[string]Stats{
			"abc123def456789": {
				ContainerID: "abc123def456789",
				CPUPercent:  12.5,
				MemoryUsage: 256 << 20, MemoryLimit: 512 << 20, MemoryPercent: 50,
				NetworkRxBytes: 1000, NetworkTxBytes: 2000,
				BlockReadBytes: 4096, BlockWriteBytes: 8192,
				PIDs: 12,
			},
		},
		images:    Inventory{Count: 14, SizeBytes: 3 << 30},
		networks:  5,
		volumes:   Inventory{Count: 3, SizeBytes: 1 << 30},
		statsErr:  map[string]error{},
		details:   map[string]ContainerDetail{},
		detailErr: map[string]error{},
		events:    make(chan Event, 16),
		eventsErr: make(chan error, 1),
	}
}

func (f *fakeClient) block(ctx context.Context) error {
	f.mu.Lock()
	d := f.blockFor
	f.mu.Unlock()
	if d == 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeClient) Ping(ctx context.Context) (Version, error) {
	if err := f.block(ctx); err != nil {
		return Version{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, f.pingErr
}

func (f *fakeClient) Containers(ctx context.Context) ([]Container, error) {
	if err := f.block(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.containersErr != nil {
		return nil, f.containersErr
	}
	return append([]Container(nil), f.containers...), nil
}

func (f *fakeClient) ContainerStats(ctx context.Context, id string) (Stats, error) {
	if err := f.block(ctx); err != nil {
		return Stats{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.statsErr[id]; ok {
		return Stats{}, err
	}
	s, ok := f.stats[id]
	if !ok {
		return Stats{}, errors.New("no such container")
	}
	return s, nil
}

func (f *fakeClient) Inspect(ctx context.Context, id string) (ContainerDetail, error) {
	if err := f.block(ctx); err != nil {
		return ContainerDetail{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.detailErr[id]; ok {
		return ContainerDetail{}, err
	}
	d, ok := f.details[id]
	if !ok {
		return ContainerDetail{}, errors.New("no such container")
	}
	return d, nil
}

func (f *fakeClient) Images(ctx context.Context) (Inventory, error) {
	if err := f.block(ctx); err != nil {
		return Inventory{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.images, f.imagesErr
}

func (f *fakeClient) Networks(ctx context.Context) (int, error) {
	if err := f.block(ctx); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.networks, f.networksErr
}

func (f *fakeClient) Volumes(ctx context.Context) (Inventory, error) {
	if err := f.block(ctx); err != nil {
		return Inventory{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.volumes, f.volumesErr
}

func (f *fakeClient) Events(context.Context) (<-chan Event, <-chan error) {
	return f.events, f.eventsErr
}

func (f *fakeClient) Logs(ctx context.Context, id string, _ LogOptions) (<-chan LogLine, <-chan error) {
	out := make(chan LogLine, 4)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		for i := range 3 {
			select {
			case out <- LogLine{ContainerID: id, Stream: StreamStdout, Time: time.Now(), Message: "line"}:
				_ = i
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, errCh
}

func (f *fakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

var _ Client = (*fakeClient)(nil)
