package inventory_test

import (
	"testing"

	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/platform/errs"
)

const localNode = "8d7dc1c1d52274c74cb0a569e7774a31"

// An empty scope means "this host". Callers that never learned about node
// scoping keep working, which is what makes the parameter addable without a
// breaking change.
func TestZeroScopeIsLocal(t *testing.T) {
	t.Parallel()

	if !inventory.Local.IsLocal(localNode) {
		t.Error("the zero scope is not local")
	}
	if !(inventory.Scope{}).IsLocal(localNode) {
		t.Error("an empty Scope is not local")
	}
}

// Naming the local node explicitly is what a node picker does on every
// request, and it must not be treated as a remote host.
func TestExplicitLocalNodeIsLocal(t *testing.T) {
	t.Parallel()

	if !(inventory.Scope{NodeID: localNode}).IsLocal(localNode) {
		t.Error("the local node addressed by id is not recognised as local")
	}
}

func TestOtherNodeIsNotLocal(t *testing.T) {
	t.Parallel()

	if (inventory.Scope{NodeID: "some-other-node"}).IsLocal(localNode) {
		t.Error("a different node id was treated as local")
	}
}

// The error has to be `unavailable`, not `not_implemented`: the latter means
// "this host cannot do this at all", which the frontend renders as a permanent
// absence. Reading another node's inventory is a capability Atlas will gain,
// so it is temporarily unavailable rather than absent.
func TestRemoteUnavailableCarriesCodeAndContext(t *testing.T) {
	t.Parallel()

	err := inventory.ErrRemoteUnavailable("test.op", "remote-node", "process inventory")

	var apiErr *errs.Error
	if !errs.As(err, &apiErr) {
		t.Fatalf("error is not a typed errs.Error: %T", err)
	}
	if apiErr.Code != errs.CodeUnavailable {
		t.Errorf("code = %s, want %s", apiErr.Code, errs.CodeUnavailable)
	}

	details := apiErr.Details
	if details["node"] != "remote-node" {
		t.Errorf("node detail = %v, want remote-node", details["node"])
	}
	if details["subject"] != "process inventory" {
		t.Errorf("subject detail = %v", details["subject"])
	}
	// The reason is what a UI shows instead of an empty table.
	if details["reason"] == nil {
		t.Error("no reason recorded; a caller cannot explain the gap")
	}
}
