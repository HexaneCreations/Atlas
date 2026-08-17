package agent

import (
	"context"
	"testing"
)

// TestBootstrapAllRelationshipsIsolatesFailure is the exact scenario behind
// the "local" relationship silently missing from bootstrap logs while
// "production" succeeded: one relationship's config/connectivity is broken
// while another's is healthy, both under bootstrapAllRelationships (the
// function agent.go's New calls right before this test's namesake log line,
// "resolved relationships entering bootstrap"). The healthy one must
// bootstrap successfully and the broken one must fail on its own, without
// either blocking, delaying, or corrupting the other — proving no shared
// state leaks between relationships at the one function actually
// responsible for running them.
func TestBootstrapAllRelationshipsIsolatesFailure(t *testing.T) {
	shrinkBootstrapBackoff(t)

	healthyURL, _ := testControlPlane(t)
	healthy := testConfig(t, healthyURL)
	healthy.id = "local"

	broken := testConfig(t, unreachableURL(t))
	broken.id = "production"

	relConfigs := map[string]relationshipConfig{
		healthy.id: healthy,
		broken.id:  broken,
	}

	relationships, err := bootstrapAllRelationships(context.Background(), relConfigs, "node-1", nil, discardLogger())
	if err == nil {
		t.Fatal("expected the broken relationship to produce an error")
	}

	rt, ok := relationships[healthy.id]
	if !ok {
		t.Fatalf("healthy relationship %q did not bootstrap; got relationships=%v, err=%v", healthy.id, relationships, err)
	}
	if rt.transport == nil {
		t.Error("healthy relationship bootstrapped with no transport")
	}

	if _, ok := relationships[broken.id]; ok {
		t.Errorf("broken relationship %q unexpectedly bootstrapped successfully", broken.id)
	}
}
