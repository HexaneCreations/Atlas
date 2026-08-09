package id_test

import (
	"sort"
	"sync"
	"testing"

	"github.com/hexane/atlas/internal/platform/id"
)

func TestNewIsUniqueUnderConcurrency(t *testing.T) {
	t.Parallel()

	const goroutines, perGoroutine = 16, 500

	var wg sync.WaitGroup
	all := make([]string, goroutines*perGoroutine)
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perGoroutine {
				all[g*perGoroutine+i] = id.New()
			}
		}()
	}
	wg.Wait()

	seen := make(map[string]struct{}, len(all))
	for _, v := range all {
		if len(v) != id.Length {
			t.Fatalf("id %q has length %d, want %d", v, len(v), id.Length)
		}
		if _, dup := seen[v]; dup {
			t.Fatalf("duplicate id generated: %q", v)
		}
		seen[v] = struct{}{}
	}
}

func TestIdentifiersSortByCreationOrder(t *testing.T) {
	t.Parallel()

	ids := make([]string, 100)
	for i := range ids {
		ids[i] = id.New()
	}

	if !sort.StringsAreSorted(ids) {
		t.Error("identifiers from a single goroutine should be lexicographically ordered")
	}
}

func BenchmarkNew(b *testing.B) {
	for b.Loop() {
		_ = id.New()
	}
}
