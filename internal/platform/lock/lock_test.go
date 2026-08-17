package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAcquireFirstSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atlas-agent.lock")

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer l.Release()
}

func TestAcquireSecondFailsWithHolderPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atlas-agent.lock")

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer l.Release()

	_, err = Acquire(path)
	if err == nil {
		t.Fatal("second Acquire: expected error, got nil")
	}
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("second Acquire: expected *ErrHeld, got %T (%v)", err, err)
	}
	if held.PID != os.Getpid() {
		t.Errorf("held.PID = %d, want %d", held.PID, os.Getpid())
	}
	if held.Path != path {
		t.Errorf("held.Path = %q, want %q", held.Path, path)
	}
}

func TestReleaseThenReacquireSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atlas-agent.lock")

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	l2, err := Acquire(path)
	if err != nil {
		t.Fatalf("re-Acquire after Release: %v", err)
	}
	defer l2.Release()
}

func TestConcurrentAcquireExactlyOneWinner(t *testing.T) {
	for _, n := range []int{2, 5, 10} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "atlas-agent.lock")

			var wins int64
			var wg sync.WaitGroup
			locks := make([]*Lock, n)
			errs := make([]error, n)

			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					l, err := Acquire(path)
					locks[i] = l
					errs[i] = err
					if err == nil {
						atomic.AddInt64(&wins, 1)
					}
				}(i)
			}
			wg.Wait()

			if wins != 1 {
				t.Fatalf("expected exactly 1 winner among %d concurrent Acquire calls, got %d", n, wins)
			}
			for i, l := range locks {
				if l != nil {
					l.Release()
				} else if _, ok := errs[i].(*ErrHeld); !ok {
					t.Errorf("attempt %d: expected *ErrHeld on failure, got %T (%v)", i, errs[i], errs[i])
				}
			}
		})
	}
}

func TestStaleLockReleasedOnUncleanClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atlas-agent.lock")

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	// Simulate a crash: the fd is closed without going through the flock
	// unlock step. The kernel still tracks the lock by open file
	// description, so closing it (however abruptly) must release it.
	l.f.Close()

	l2, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after simulated crash: %v", err)
	}
	defer l2.Release()
}

func TestDifferentPathsAcquireIndependently(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.lock")
	pathB := filepath.Join(dir, "b.lock")

	la, err := Acquire(pathA)
	if err != nil {
		t.Fatalf("Acquire A: %v", err)
	}
	defer la.Release()

	lb, err := Acquire(pathB)
	if err != nil {
		t.Fatalf("Acquire B (should be independent of A): %v", err)
	}
	defer lb.Release()
}
