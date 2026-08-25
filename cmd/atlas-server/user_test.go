package main

import "testing"

// validateGrantScope is the CLI-layer guard against a `user grant` accepting
// an empty --node-id as an accidental fleet-wide grant. See
// docs/adr/0011-deferred-rbac.md and internal/core/user.GrantSpec.

func TestValidateGrantScopeAcceptsNodeID(t *testing.T) {
	if err := validateGrantScope("node-1", false); err != nil {
		t.Errorf("validateGrantScope(node-1, false) = %v, want nil", err)
	}
}

func TestValidateGrantScopeAcceptsFleetWide(t *testing.T) {
	if err := validateGrantScope("", true); err != nil {
		t.Errorf("validateGrantScope(\"\", true) = %v, want nil", err)
	}
}

func TestValidateGrantScopeRejectsNeither(t *testing.T) {
	if err := validateGrantScope("", false); err == nil {
		t.Error("validateGrantScope(\"\", false) succeeded, want an error — there is no default between a node and fleet-wide")
	}
}

func TestValidateGrantScopeRejectsBoth(t *testing.T) {
	if err := validateGrantScope("node-1", true); err == nil {
		t.Error("validateGrantScope(node-1, true) succeeded, want an error — a grant cannot be both scoped and fleet-wide")
	}
}
