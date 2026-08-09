package service

import (
	"sort"
	"strings"
	"time"
)

// The systemd dependency graph.
//
// systemd expresses relationships in both directions: a unit with
// `Before=multi-user.target` causes multi-user.target to report
// `After=<unit>`, and `Requires=` has a matching `RequiredBy=` on the other
// side. Reading both without care produces every edge twice.
//
// This model stores one canonical edge per relationship, always pointing from
// the *dependent* to the *dependency* — "A requires B" and "A starts after B"
// are both A→B. Reverse properties are rewritten into that form on the way in,
// which both deduplicates and means a single adjacency lookup answers either
// question.
//
// The distinction that matters most here is between requirement and ordering.
// `After=` is *only* about start sequence: a unit can be ordered after
// something it does not need at all, and treating that as a dependency makes
// impact analysis confidently wrong. The two are separate edge classes
// throughout, and failure propagation walks requirement edges only.

// EdgeKind is the systemd directive a relationship came from.
type EdgeKind string

const (
	// EdgeRequires is a hard requirement: if it fails to start, the dependent
	// is not started either.
	EdgeRequires EdgeKind = "requires"
	// EdgeWants is a soft requirement: the dependent starts regardless.
	EdgeWants EdgeKind = "wants"
	// EdgeBindsTo is Requires plus lifecycle: the dependent stops if the
	// dependency stops for any reason, including a device disappearing.
	EdgeBindsTo EdgeKind = "binds_to"
	// EdgePartOf propagates stop and restart, but not start.
	EdgePartOf EdgeKind = "part_of"
	// EdgeAfter is ordering only, carrying no requirement whatsoever.
	EdgeAfter EdgeKind = "after"
	// EdgeConflicts is mutual exclusion: starting one stops the other.
	EdgeConflicts EdgeKind = "conflicts"
)

// EdgeClass groups kinds by what they mean for failure analysis.
type EdgeClass string

const (
	// ClassRequirement is a relationship through which failure propagates.
	ClassRequirement EdgeClass = "requirement"
	// ClassOrdering describes start sequence and nothing else.
	ClassOrdering EdgeClass = "ordering"
	// ClassConflict is mutual exclusion.
	ClassConflict EdgeClass = "conflict"
)

// Class reports which class this kind belongs to.
func (k EdgeKind) Class() EdgeClass {
	switch k {
	case EdgeRequires, EdgeWants, EdgeBindsTo, EdgePartOf:
		return ClassRequirement
	case EdgeAfter:
		return ClassOrdering
	case EdgeConflicts:
		return ClassConflict
	default:
		return ClassOrdering
	}
}

// Hard reports whether failure of the dependency prevents the dependent from
// running. `Wants` is deliberately excluded: a wanted unit that fails leaves
// the dependent running, which is the entire point of the directive.
func (k EdgeKind) Hard() bool {
	// PartOf counts: systemd propagates stop and restart along it, so a unit
	// that is PartOf a failed one is taken down with it. Wants does not — a
	// wanted unit failing leaves its dependent running, which is the entire
	// point of the directive.
	return k == EdgeRequires || k == EdgeBindsTo || k == EdgePartOf
}

// Edge is one canonical relationship, pointing dependent → dependency.
type Edge struct {
	From string   `json:"from"`
	To   string   `json:"to"`
	Kind EdgeKind `json:"kind"`
}

// Node is one unit in the graph.
type Node struct {
	Name string `json:"name"`
	// Type is the unit suffix: service, target, socket, mount, timer, slice.
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`

	LoadState   string      `json:"load_state,omitempty"`
	ActiveState ActiveState `json:"active_state,omitempty"`
	SubState    SubState    `json:"sub_state,omitempty"`

	// Known is false for a unit that appears only as the target of an edge.
	//
	// That happens when a unit file references something the manager does not
	// have — a typo, or a package removed without its dependents being
	// updated. It is a real finding rather than something to drop silently, so
	// the node is kept and flagged.
	Known bool `json:"known"`
}

// Failed reports whether this node is in a failed state.
func (n Node) Failed() bool { return n.ActiveState == ActiveStateFailed }

// Graph is an immutable dependency graph with adjacency in both directions.
type Graph struct {
	nodes map[string]Node
	// out maps a unit to the things it depends on or is ordered after.
	out map[string][]Edge
	// in maps a unit to the things that depend on it.
	in map[string][]Edge

	edges       []Edge
	collectedAt time.Time
}

// NewGraph builds a graph from nodes and canonical edges.
//
// Edges naming a unit not present in nodes still produce a node, marked
// unknown, so no edge in the graph can dangle.
func NewGraph(nodes []Node, edges []Edge, collectedAt time.Time) *Graph {
	g := &Graph{
		nodes:       make(map[string]Node, len(nodes)),
		out:         make(map[string][]Edge),
		in:          make(map[string][]Edge),
		collectedAt: collectedAt,
	}

	for _, n := range nodes {
		n.Known = true
		if n.Type == "" {
			n.Type = unitType(n.Name)
		}
		g.nodes[n.Name] = n
	}

	seen := make(map[Edge]struct{}, len(edges))
	for _, e := range edges {
		if e.From == "" || e.To == "" || e.From == e.To {
			continue
		}
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}

		g.ensure(e.From)
		g.ensure(e.To)
		g.out[e.From] = append(g.out[e.From], e)
		g.in[e.To] = append(g.in[e.To], e)
		g.edges = append(g.edges, e)
	}

	// Deterministic ordering everywhere: two collections of the same host must
	// produce the same payload, or every poll looks like a change.
	for name := range g.out {
		sortEdges(g.out[name])
	}
	for name := range g.in {
		sortEdges(g.in[name])
	}
	sortEdges(g.edges)

	return g
}

func (g *Graph) ensure(name string) {
	if _, ok := g.nodes[name]; ok {
		return
	}
	g.nodes[name] = Node{Name: name, Type: unitType(name), Known: false}
}

func sortEdges(es []Edge) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].From != es[j].From {
			return es[i].From < es[j].From
		}
		if es[i].To != es[j].To {
			return es[i].To < es[j].To
		}
		return es[i].Kind < es[j].Kind
	})
}

// unitType extracts the suffix that classifies a unit.
func unitType(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 && i < len(name)-1 {
		return name[i+1:]
	}
	return "unknown"
}

// CollectedAt reports when the structure was read.
func (g *Graph) CollectedAt() time.Time { return g.collectedAt }

// Len reports the node and edge counts.
func (g *Graph) Len() (nodes, edges int) { return len(g.nodes), len(g.edges) }

// Node returns one unit.
func (g *Graph) Node(name string) (Node, bool) {
	n, ok := g.nodes[name]
	return n, ok
}

// Nodes returns every unit, name-ordered.
func (g *Graph) Nodes() []Node {
	out := make([]Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Edges returns every canonical edge, ordered.
func (g *Graph) Edges() []Edge { return g.edges }

// Dependencies returns what this unit depends on or is ordered after.
func (g *Graph) Dependencies(name string) []Edge { return g.out[name] }

// Dependents returns the units that depend on this one.
func (g *Graph) Dependents(name string) []Edge { return g.in[name] }

// WithState returns a copy carrying live unit state over the cached structure.
//
// Structure changes only when unit files change; state changes constantly.
// Keeping them apart is what lets the graph be collected on a slow interval
// while the states shown against it are current.
func (g *Graph) WithState(units []Unit) *Graph {
	out := &Graph{
		nodes:       make(map[string]Node, len(g.nodes)),
		out:         g.out,
		in:          g.in,
		edges:       g.edges,
		collectedAt: g.collectedAt,
	}
	for name, n := range g.nodes {
		out.nodes[name] = n
	}
	for _, u := range units {
		n, ok := out.nodes[u.Name]
		if !ok {
			// A unit that appeared since the structure was collected. Its
			// state is still worth reporting; it simply has no edges yet.
			n = Node{Name: u.Name, Type: unitType(u.Name), Known: true}
		}
		n.ActiveState = u.ActiveState
		n.SubState = u.SubState
		n.LoadState = u.LoadState
		if u.Description != "" {
			n.Description = u.Description
		}
		out.nodes[u.Name] = n
	}
	return out
}

// Direction selects which way a traversal walks.
type Direction string

const (
	// TowardDependencies walks what a unit needs.
	TowardDependencies Direction = "dependencies"
	// TowardDependents walks what needs a unit — the blast radius.
	TowardDependents Direction = "dependents"
)

// TraverseOptions bounds a walk.
type TraverseOptions struct {
	// Depth caps how many hops out from the root. Zero means unbounded, which
	// on a real host still terminates because the visited set is bounded by
	// the node count.
	Depth int
	// Classes restricts which edge classes are followed. Empty follows all.
	Classes []EdgeClass
	// MaxNodes caps the result. Reaching it sets truncated.
	MaxNodes int
}

// Reach is the result of a traversal.
type Reach struct {
	// Nodes includes the root, each with its hop distance.
	Nodes []ReachNode
	Edges []Edge
	// Truncated reports that MaxNodes stopped the walk before it completed,
	// so the caller knows the answer is partial rather than final.
	Truncated bool
}

// ReachNode is a node with its distance from the traversal root.
type ReachNode struct {
	Node
	Depth int `json:"depth"`
}

// Traverse walks the graph from root, breadth-first.
//
// Breadth-first so `Depth` means hops rather than an arbitrary branch length,
// and so a truncated result is the *nearest* part of the graph, which is the
// part that matters.
//
// Cycle safety is not optional: systemd graphs genuinely contain cycles —
// `systemd-analyze verify` reports them rather than preventing them — and a
// naive walk would not terminate.
func (g *Graph) Traverse(root string, dir Direction, opts TraverseOptions) Reach {
	out := Reach{}
	if _, ok := g.nodes[root]; !ok {
		return out
	}

	allowed := classSet(opts.Classes)
	maxNodes := opts.MaxNodes
	if maxNodes <= 0 {
		maxNodes = len(g.nodes)
	}

	visited := map[string]int{root: 0}
	queue := []string{root}
	seenEdge := map[Edge]struct{}{}

	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		depth := visited[name]

		node := g.nodes[name]
		out.Nodes = append(out.Nodes, ReachNode{Node: node, Depth: depth})

		if opts.Depth > 0 && depth >= opts.Depth {
			continue
		}

		var edges []Edge
		if dir == TowardDependents {
			edges = g.in[name]
		} else {
			edges = g.out[name]
		}

		for _, e := range edges {
			if allowed != nil {
				if _, ok := allowed[e.Kind.Class()]; !ok {
					continue
				}
			}

			next := e.To
			if dir == TowardDependents {
				next = e.From
			}

			if _, ok := seenEdge[e]; !ok {
				seenEdge[e] = struct{}{}
				out.Edges = append(out.Edges, e)
			}

			if _, seen := visited[next]; seen {
				continue
			}
			if len(visited) >= maxNodes {
				out.Truncated = true
				continue
			}
			visited[next] = depth + 1
			queue = append(queue, next)
		}
	}

	sort.Slice(out.Nodes, func(i, j int) bool {
		if out.Nodes[i].Depth != out.Nodes[j].Depth {
			return out.Nodes[i].Depth < out.Nodes[j].Depth
		}
		return out.Nodes[i].Name < out.Nodes[j].Name
	})
	sortEdges(out.Edges)
	return out
}

func classSet(classes []EdgeClass) map[EdgeClass]struct{} {
	if len(classes) == 0 {
		return nil
	}
	out := make(map[EdgeClass]struct{}, len(classes))
	for _, c := range classes {
		out[c] = struct{}{}
	}
	return out
}

// Impact reports what would be affected if this unit failed.
//
// The transitive closure over *requirement* edges in the dependents
// direction, and ordering edges are excluded deliberately: a unit ordered
// after this one is not affected by it failing, and including them would
// inflate the blast radius to most of the host — every service on a typical
// system is transitively `After=` basic.target.
//
// The result separates hard from soft. A `Requires` dependent will not run
// without this unit; a `Wants` dependent will run degraded. Reporting them as
// one number would overstate the severity.
type Impact struct {
	// Hard are units that cannot run without this one.
	Hard []string `json:"hard"`
	// Soft are units that will still run, without whatever this provides.
	Soft []string `json:"soft"`
	// Truncated reports the walk hit its node cap.
	Truncated bool `json:"truncated"`
}

// Impact computes the blast radius of a unit failing.
func (g *Graph) Impact(name string, maxNodes int) Impact {
	out := Impact{Hard: []string{}, Soft: []string{}}
	if _, ok := g.nodes[name]; !ok {
		return out
	}
	if maxNodes <= 0 {
		maxNodes = len(g.nodes)
	}

	// hard[u] records whether u is reachable through an unbroken chain of hard
	// edges. A soft link anywhere in the chain makes everything beyond it soft:
	// once one unit tolerates the failure, its own dependents are insulated.
	hard := map[string]bool{}
	visited := map[string]struct{}{name: {}}
	type step struct {
		unit string
		hard bool
	}
	queue := []step{{unit: name, hard: true}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		for _, e := range g.in[cur.unit] {
			if e.Kind.Class() != ClassRequirement {
				continue
			}
			dependent := e.From
			isHard := cur.hard && e.Kind.Hard()

			if was, seen := hard[dependent]; seen {
				// Already reached; upgrade to hard if this path is stronger.
				if isHard && !was {
					hard[dependent] = true
					queue = append(queue, step{unit: dependent, hard: true})
				}
				continue
			}
			if len(visited) >= maxNodes {
				out.Truncated = true
				continue
			}
			visited[dependent] = struct{}{}
			hard[dependent] = isHard
			queue = append(queue, step{unit: dependent, hard: isHard})
		}
	}

	for unit, isHard := range hard {
		if isHard {
			out.Hard = append(out.Hard, unit)
		} else {
			out.Soft = append(out.Soft, unit)
		}
	}
	sort.Strings(out.Hard)
	sort.Strings(out.Soft)
	return out
}

// Health is a unit's own state combined with its dependencies'.
type Health string

const (
	// HealthHealthy means the unit is fine and so is everything it needs.
	HealthHealthy Health = "healthy"
	// HealthDegraded means the unit itself is fine but something it depends on
	// is not. Whether it is actually impaired depends on the service, which is
	// why this is not "unhealthy".
	HealthDegraded Health = "degraded"
	// HealthFailed means the unit itself has failed.
	HealthFailed Health = "failed"
	// HealthInactive means the unit is not running and has not failed.
	HealthInactive Health = "inactive"
	// HealthUnknown means Atlas has no state for the unit.
	HealthUnknown Health = "unknown"
)

// Propagation explains a unit's health, including why it is degraded.
type Propagation struct {
	Health Health `json:"health"`
	// FailedDependencies are the failed units this one depends on, direct or
	// transitive, through requirement edges only.
	FailedDependencies []string `json:"failed_dependencies,omitempty"`
}

// Propagate computes a unit's health from its own state and its dependencies.
//
// Only requirement edges are followed. A unit ordered after a failed unit is
// not itself impaired by that failure, and treating ordering as propagation
// would mark most of the host degraded whenever anything failed.
func (g *Graph) Propagate(name string) Propagation {
	node, ok := g.nodes[name]
	if !ok {
		return Propagation{Health: HealthUnknown}
	}

	own := HealthUnknown
	switch node.ActiveState {
	case ActiveStateFailed:
		own = HealthFailed
	case ActiveStateActive, ActiveStateActivating:
		own = HealthHealthy
	case ActiveStateInactive, ActiveStateDeactivating:
		own = HealthInactive
	case ActiveStateUnknown:
		own = HealthUnknown
	}

	if own == HealthFailed {
		return Propagation{Health: HealthFailed}
	}

	reach := g.Traverse(name, TowardDependencies, TraverseOptions{
		Classes: []EdgeClass{ClassRequirement},
	})

	var failed []string
	for _, n := range reach.Nodes {
		if n.Name != name && n.Failed() {
			failed = append(failed, n.Name)
		}
	}
	sort.Strings(failed)

	if len(failed) > 0 && own == HealthHealthy {
		return Propagation{Health: HealthDegraded, FailedDependencies: failed}
	}
	return Propagation{Health: own, FailedDependencies: failed}
}
