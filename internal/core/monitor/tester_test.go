package monitor

import (
	"context"
	"sync"
	"testing"

	"github.com/hexane/atlas/internal/platform/errs"
)

type blockingChecker struct {
	mu      sync.Mutex
	seen    []Monitor
	started chan struct{}
	release chan struct{}
}

func (c *blockingChecker) Check(_ context.Context, m Monitor, _ Secret) Result {
	c.mu.Lock()
	c.seen = append(c.seen, m)
	c.mu.Unlock()
	if c.started != nil {
		c.started <- struct{}{}
		<-c.release
	}
	return Result{Up: true, StatusCode: 200, ResponseMS: 5}
}

func TestTesterCapsConcurrentTests(t *testing.T) {
	t.Parallel()
	c := &blockingChecker{started: make(chan struct{}), release: make(chan struct{})}
	tester := NewTester(c, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tester.Test(context.Background(), monitorFor("https://a.example.com/"), Secret{}); err != nil {
				t.Error(err)
			}
		}()
		<-c.started
	}
	_, err := tester.Test(context.Background(), monitorFor("https://a.example.com/"), Secret{})
	if errs.CodeOf(err) != errs.CodeRateLimited {
		t.Fatalf("a third concurrent test = %v, want rate limited", err)
	}
	close(c.release)
	wg.Wait()
	c.started = nil
	if _, err := tester.Test(context.Background(), monitorFor("https://a.example.com/"), Secret{}); err != nil {
		t.Errorf("a test after the others finished: %v", err)
	}
}

func TestTesterClampsTimeoutAndRedirects(t *testing.T) {
	t.Parallel()
	c := &blockingChecker{}
	m := monitorFor("https://a.example.com/")
	m.TimeoutSeconds, m.MaxRedirects = 3600, 99
	if _, err := NewTester(c, 1).Test(context.Background(), m, Secret{}); err != nil {
		t.Fatal(err)
	}
	if got := c.seen[0]; got.TimeoutSeconds != MaxTimeoutSeconds || got.MaxRedirects != MaxRedirects {
		t.Errorf("checked with timeout %d and %d redirects", got.TimeoutSeconds, got.MaxRedirects)
	}
}
