package app

import (
	"context"
	"testing"

	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/hostid"
	"github.com/hexane/atlas/internal/platform/log"
)

// Inventory is scoped to a node, and the pipeline is the last place that scope
// is checked before a plugin is asked to read the local machine.
//
// The guard runs before any plugin is touched, so these need no database and
// no active plugins — which is also the point: a host with nothing configured
// must still refuse a remote question rather than fall through to an empty
// answer.

const testNodeID = "8d7dc1c1d52274c74cb0a569e7774a31"

func newTestPipeline(t *testing.T) *collectionPipeline {
	t.Helper()

	cfg := config.Default()
	return newCollectionPipeline(
		&cfg,
		log.Discard(),
		nil, // bus: unused by the inventory guard
		nil, // pool: unused by the inventory guard
		hostid.Identity{NodeID: testNodeID, Hostname: "test-host"},
		nil, // onEvent: unused by the inventory guard
	)
}

// Each inventory method must refuse a node it cannot read. Returning an empty
// slice instead would tell a caller that the remote host has no processes, no
// containers and no listening ports — three claims Atlas has no basis for.
func TestInventoryMethodsRefuseRemoteScopes(t *testing.T) {
	t.Parallel()

	p := newTestPipeline(t)
	ctx := context.Background()
	remote := inventory.Scope{NodeID: "some-other-host"}

	calls := map[string]func() error{
		"Processes": func() error { _, err := p.Processes(ctx, remote); return err },
		"Services":  func() error { _, err := p.Services(ctx, remote); return err },
		"CronJobs":  func() error { _, err := p.CronJobs(ctx, remote); return err },
		"Ports":     func() error { _, err := p.Ports(ctx, remote); return err },
		"Mounts":    func() error { _, err := p.Mounts(ctx, remote); return err },
		"ServiceGraph": func() error {
			_, err := p.ServiceGraph(ctx, remote)
			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("a remote scope was answered rather than refused")
			}

			var apiErr *errs.Error
			if !errs.As(err, &apiErr) {
				t.Fatalf("error is not typed: %T", err)
			}
			// `unavailable`, not `not_implemented`: reading another node is a
			// capability Atlas gains with agents, not one it lacks forever.
			if apiErr.Code != errs.CodeUnavailable {
				t.Errorf("code = %s, want %s", apiErr.Code, errs.CodeUnavailable)
			}
			if apiErr.Details["node"] != "some-other-host" {
				t.Errorf("details.node = %v", apiErr.Details["node"])
			}
		})
	}
}

// The local node must pass the guard whether addressed implicitly or by id.
// With no plugins started the methods return nil — the absence of a plugin,
// not a refusal — which is what distinguishes "not configured here" from
// "cannot answer for that host".
func TestInventoryMethodsAcceptLocalScopes(t *testing.T) {
	t.Parallel()

	p := newTestPipeline(t)
	ctx := context.Background()

	for _, scope := range []inventory.Scope{
		inventory.Local,
		{NodeID: testNodeID},
	} {
		if _, err := p.Processes(ctx, scope); err != nil {
			t.Errorf("scope %+v was refused: %v", scope, err)
		}
		if _, err := p.Ports(ctx, scope); err != nil {
			t.Errorf("scope %+v was refused: %v", scope, err)
		}
	}
}
