package service

import (
	"slices"
	"testing"
	"time"
)

// node is a terse fixture helper.
func node(name string, state ActiveState) Node {
	return Node{Name: name, Type: unitType(name), ActiveState: state, Known: true}
}

func edge(from, to string, kind EdgeKind) Edge {
	return Edge{From: from, To: to, Kind: kind}
}

func reachNames(ns []ReachNode) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Name)
	}
	return out
}

// Ordering is not requirement. This is the distinction the whole model rests
// on: a unit ordered after another does not depend on it, and conflating them
// makes impact analysis claim a blast radius that does not exist.
func TestEdgeClassSeparatesOrderingFromRequirement(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind  EdgeKind
		class EdgeClass
		hard  bool
	}{
		{EdgeRequires, ClassRequirement, true},
		{EdgeBindsTo, ClassRequirement, true},
		{EdgePartOf, ClassRequirement, true},
		{EdgeWants, ClassRequirement, false},
		{EdgeAfter, ClassOrdering, false},
		{EdgeConflicts, ClassConflict, false},
	}

	for _, c := range cases {
		if got := c.kind.Class(); got != c.class {
			t.Errorf("%s class = %s, want %s", c.kind, got, c.class)
		}
		if got := c.kind.Hard(); got != c.hard {
			t.Errorf("%s hard = %v, want %v", c.kind, got, c.hard)
		}
	}
}

// systemd reports every relationship twice — once forward, once reversed — so
// the graph must collapse them or double every edge.
func TestNewGraphDeduplicatesEdges(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{node("a.service", ActiveStateActive), node("b.target", ActiveStateActive)},
		[]Edge{
			edge("a.service", "b.target", EdgeRequires),
			edge("a.service", "b.target", EdgeRequires), // same relation, from RequiredBy
		},
		time.Now(),
	)

	if _, edges := g.Len(); edges != 1 {
		t.Errorf("edges = %d, want 1 after deduplication", edges)
	}
}

// An edge may name a unit the manager does not have — a typo in a unit file,
// or a package removed without updating its dependents. That is a finding, so
// the node is kept and flagged rather than dropped.
func TestNewGraphKeepsUnknownReferencedUnits(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{node("a.service", ActiveStateActive)},
		[]Edge{edge("a.service", "ghost.service", EdgeRequires)},
		time.Now(),
	)

	ghost, ok := g.Node("ghost.service")
	if !ok {
		t.Fatal("referenced unit was dropped instead of being recorded")
	}
	if ghost.Known {
		t.Error("a unit the manager does not have is marked known")
	}
	if ghost.Type != "service" {
		t.Errorf("type = %q, want service", ghost.Type)
	}
}

func TestNewGraphIgnoresSelfEdges(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{node("a.service", ActiveStateActive)},
		[]Edge{edge("a.service", "a.service", EdgeAfter)},
		time.Now(),
	)
	if _, edges := g.Len(); edges != 0 {
		t.Errorf("edges = %d, want 0 — a unit cannot depend on itself", edges)
	}
}

// Real systemd graphs contain cycles; systemd-analyze reports them rather than
// preventing them. A traversal that does not guard against them never returns.
func TestTraverseTerminatesOnCycles(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{
			node("a.service", ActiveStateActive),
			node("b.service", ActiveStateActive),
			node("c.service", ActiveStateActive),
		},
		[]Edge{
			edge("a.service", "b.service", EdgeRequires),
			edge("b.service", "c.service", EdgeRequires),
			edge("c.service", "a.service", EdgeRequires),
		},
		time.Now(),
	)

	done := make(chan Reach, 1)
	go func() { done <- g.Traverse("a.service", TowardDependencies, TraverseOptions{}) }()

	select {
	case reach := <-done:
		if len(reach.Nodes) != 3 {
			t.Errorf("nodes = %d, want 3", len(reach.Nodes))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("traversal did not terminate on a cyclic graph")
	}
}

func TestTraverseRespectsDepth(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{
			node("a.service", ActiveStateActive),
			node("b.service", ActiveStateActive),
			node("c.service", ActiveStateActive),
			node("d.service", ActiveStateActive),
		},
		[]Edge{
			edge("a.service", "b.service", EdgeRequires),
			edge("b.service", "c.service", EdgeRequires),
			edge("c.service", "d.service", EdgeRequires),
		},
		time.Now(),
	)

	reach := g.Traverse("a.service", TowardDependencies, TraverseOptions{Depth: 2})
	got := reachNames(reach.Nodes)
	want := []string{"a.service", "b.service", "c.service"}
	if !slices.Equal(got, want) {
		t.Errorf("depth 2 reached %v, want %v", got, want)
	}
}

func TestTraverseFiltersByClass(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{
			node("app.service", ActiveStateActive),
			node("db.service", ActiveStateActive),
			node("network.target", ActiveStateActive),
		},
		[]Edge{
			edge("app.service", "db.service", EdgeRequires),
			edge("app.service", "network.target", EdgeAfter),
		},
		time.Now(),
	)

	reach := g.Traverse("app.service", TowardDependencies, TraverseOptions{
		Classes: []EdgeClass{ClassRequirement},
	})

	got := reachNames(reach.Nodes)
	if slices.Contains(got, "network.target") {
		t.Errorf("ordering-only edge followed under a requirement filter: %v", got)
	}
	if !slices.Contains(got, "db.service") {
		t.Errorf("requirement edge not followed: %v", got)
	}
}

func TestTraverseReportsTruncation(t *testing.T) {
	t.Parallel()

	nodes := []Node{node("root.service", ActiveStateActive)}
	var edges []Edge
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		nodes = append(nodes, node(n+".service", ActiveStateActive))
		edges = append(edges, edge("root.service", n+".service", EdgeRequires))
	}

	reach := NewGraph(nodes, edges, time.Now()).
		Traverse("root.service", TowardDependencies, TraverseOptions{MaxNodes: 3})

	if !reach.Truncated {
		t.Error("hitting the node cap was not reported as truncated")
	}
	if len(reach.Nodes) > 3 {
		t.Errorf("nodes = %d, want at most the cap of 3", len(reach.Nodes))
	}
}

// The blast radius must not include units merely ordered after the failure.
// Nearly every service on a host is transitively After=basic.target, so
// following ordering edges would report almost the whole system as affected.
func TestImpactIgnoresOrderingEdges(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{
			node("db.service", ActiveStateFailed),
			node("app.service", ActiveStateActive),
			node("unrelated.service", ActiveStateActive),
		},
		[]Edge{
			edge("app.service", "db.service", EdgeRequires),
			// Ordered after the database but does not need it.
			edge("unrelated.service", "db.service", EdgeAfter),
		},
		time.Now(),
	)

	impact := g.Impact("db.service", 0)

	if !slices.Contains(impact.Hard, "app.service") {
		t.Errorf("hard dependent missing: %+v", impact)
	}
	if slices.Contains(impact.Hard, "unrelated.service") || slices.Contains(impact.Soft, "unrelated.service") {
		t.Errorf("a unit merely ordered after the failure was reported as impacted: %+v", impact)
	}
}

// A Wants dependent keeps running when its dependency fails — that is what the
// directive means — so it must be reported as soft, and everything behind it
// insulated too.
func TestImpactSeparatesHardFromSoft(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{
			node("db.service", ActiveStateFailed),
			node("hard.service", ActiveStateActive),
			node("soft.service", ActiveStateActive),
			node("behind-soft.service", ActiveStateActive),
		},
		[]Edge{
			edge("hard.service", "db.service", EdgeRequires),
			edge("soft.service", "db.service", EdgeWants),
			// Requires a unit that only *wants* the failure, so it is
			// insulated: soft.service keeps running, so this one does too.
			edge("behind-soft.service", "soft.service", EdgeRequires),
		},
		time.Now(),
	)

	impact := g.Impact("db.service", 0)

	if !slices.Equal(impact.Hard, []string{"hard.service"}) {
		t.Errorf("hard = %v, want [hard.service]", impact.Hard)
	}
	if !slices.Contains(impact.Soft, "soft.service") {
		t.Errorf("soft = %v, want it to contain soft.service", impact.Soft)
	}
	if slices.Contains(impact.Hard, "behind-soft.service") {
		t.Errorf("a unit behind a soft link was reported as hard-impacted: %v", impact.Hard)
	}
}

func TestImpactIsTransitive(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{
			node("base.service", ActiveStateFailed),
			node("mid.service", ActiveStateActive),
			node("top.service", ActiveStateActive),
		},
		[]Edge{
			edge("mid.service", "base.service", EdgeRequires),
			edge("top.service", "mid.service", EdgeRequires),
		},
		time.Now(),
	)

	impact := g.Impact("base.service", 0)
	if !slices.Equal(impact.Hard, []string{"mid.service", "top.service"}) {
		t.Errorf("hard = %v, want both levels", impact.Hard)
	}
}

func TestPropagateMarksDegradedOnFailedDependency(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{
			node("app.service", ActiveStateActive),
			node("db.service", ActiveStateFailed),
		},
		[]Edge{edge("app.service", "db.service", EdgeRequires)},
		time.Now(),
	)

	got := g.Propagate("app.service")
	if got.Health != HealthDegraded {
		t.Errorf("health = %s, want degraded", got.Health)
	}
	if !slices.Equal(got.FailedDependencies, []string{"db.service"}) {
		t.Errorf("failed dependencies = %v", got.FailedDependencies)
	}
}

// A unit ordered after a failed unit is not impaired by it. Propagating along
// ordering edges would mark most of the host degraded whenever anything failed.
func TestPropagateIgnoresOrderingEdges(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{
			node("app.service", ActiveStateActive),
			node("broken.service", ActiveStateFailed),
		},
		[]Edge{edge("app.service", "broken.service", EdgeAfter)},
		time.Now(),
	)

	if got := g.Propagate("app.service"); got.Health != HealthHealthy {
		t.Errorf("health = %s, want healthy — ordering is not requirement", got.Health)
	}
}

func TestPropagateReportsOwnFailureFirst(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{
			node("app.service", ActiveStateFailed),
			node("db.service", ActiveStateFailed),
		},
		[]Edge{edge("app.service", "db.service", EdgeRequires)},
		time.Now(),
	)

	if got := g.Propagate("app.service"); got.Health != HealthFailed {
		t.Errorf("health = %s, want failed — its own state dominates", got.Health)
	}
}

// Structure is collected slowly; state is read on every request. The overlay
// must replace states without disturbing the shape.
func TestWithStateOverlaysWithoutChangingShape(t *testing.T) {
	t.Parallel()

	g := NewGraph(
		[]Node{node("app.service", ActiveStateActive), node("db.service", ActiveStateActive)},
		[]Edge{edge("app.service", "db.service", EdgeRequires)},
		time.Now(),
	)
	nodesBefore, edgesBefore := g.Len()

	fresh := g.WithState([]Unit{
		{Name: "db.service", ActiveState: ActiveStateFailed, SubState: "failed", LoadState: "loaded"},
	})

	n, e := fresh.Len()
	if n != nodesBefore || e != edgesBefore {
		t.Errorf("shape changed: %d/%d, want %d/%d", n, e, nodesBefore, edgesBefore)
	}
	if db, _ := fresh.Node("db.service"); db.ActiveState != ActiveStateFailed {
		t.Errorf("state not overlaid: %s", db.ActiveState)
	}
	// The original must be untouched — it is the shared cached structure.
	if db, _ := g.Node("db.service"); db.ActiveState != ActiveStateActive {
		t.Error("overlay mutated the cached graph")
	}
}

// The dash-prefixed root mount exists on every systemd host and is exactly the
// kind of name that breaks naive argument handling.
func TestParseUnitNamesKeepsAllTypesIncludingDashPrefixed(t *testing.T) {
	t.Parallel()

	out := `nginx.service loaded active running   A web server
-.mount          loaded active mounted Root Mount
dbus.socket      loaded active running D-Bus Socket
● broken.service loaded failed failed  Broken
`
	got := parseUnitNames(out)
	want := []string{"nginx.service", "-.mount", "dbus.socket", "broken.service"}
	if !slices.Equal(got, want) {
		t.Errorf("parsed %v, want %v", got, want)
	}
}

func TestParseStructureCanonicalisesReverseProperties(t *testing.T) {
	t.Parallel()

	// nginx declares Before=multi-user.target; canonically that is
	// multi-user.target ordered After nginx.
	out := `Id=nginx.service
Description=A web server
LoadState=loaded
ActiveState=active
SubState=running
Requires=sysinit.target
Wants=
BindsTo=
PartOf=
After=network.target
Before=multi-user.target
Conflicts=shutdown.target
RequiredBy=
WantedBy=multi-user.target
BoundBy=
ConsistsOf=
`
	nodes, edges := parseStructure(out)

	if len(nodes) != 1 || nodes[0].Name != "nginx.service" {
		t.Fatalf("nodes = %+v", nodes)
	}
	if nodes[0].Description != "A web server" {
		t.Errorf("description = %q", nodes[0].Description)
	}

	has := func(from, to string, kind EdgeKind) bool {
		return slices.Contains(edges, Edge{From: from, To: to, Kind: kind})
	}

	if !has("nginx.service", "sysinit.target", EdgeRequires) {
		t.Error("Requires edge missing")
	}
	if !has("nginx.service", "network.target", EdgeAfter) {
		t.Error("After edge missing")
	}
	// Before is rewritten: the *other* unit is ordered after this one.
	if !has("multi-user.target", "nginx.service", EdgeAfter) {
		t.Error("Before was not canonicalised into a reversed After edge")
	}
	// WantedBy is rewritten: the target wants nginx, not the other way round.
	if !has("multi-user.target", "nginx.service", EdgeWants) {
		t.Error("WantedBy was not canonicalised")
	}
	if has("nginx.service", "multi-user.target", EdgeWants) {
		t.Error("WantedBy was read as a forward Wants edge, reversing its meaning")
	}
}

func TestParseStructureHandlesEmptyProperties(t *testing.T) {
	t.Parallel()

	// systemd emits empty properties rather than omitting them.
	nodes, edges := parseStructure("Id=idle.service\nRequires=\nWants=\nAfter=\n")
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(nodes))
	}
	if len(edges) != 0 {
		t.Errorf("edges = %v, want none from empty properties", edges)
	}
}

// Neither systemd listing is complete on its own: `list-units` omits installed
// units that are inactive and unreferenced, and `list-unit-files` omits
// everything with no file on disk. Collecting only one produces a graph that
// silently loses whole classes of unit.
func TestMergeUnitNamesUnionsBothListings(t *testing.T) {
	t.Parallel()

	loaded := []string{"-.mount", "nginx.service", "multi-user.target"}
	files := []string{"nginx.service", "stopped.service", "multi-user.target"}

	got := mergeUnitNames(loaded, files)
	want := []string{"-.mount", "nginx.service", "multi-user.target", "stopped.service"}

	if !slices.Equal(got, want) {
		t.Errorf("merged %v, want %v", got, want)
	}
}

// Templates are patterns rather than units — they have no state and cannot be
// started — while their instances are real.
func TestParseUnitNamesSkipsTemplatesButKeepsInstances(t *testing.T) {
	t.Parallel()

	out := `getty@.service        loaded inactive dead  Getty template
getty@tty1.service    loaded active   running Getty on tty1
blockdev@.target      loaded inactive dead  Block device template
nginx.service         loaded active   running A web server
`
	got := parseUnitNames(out)
	want := []string{"getty@tty1.service", "nginx.service"}
	if !slices.Equal(got, want) {
		t.Errorf("parsed %v, want %v", got, want)
	}
}
