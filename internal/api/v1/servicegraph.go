package v1

import (
	"context"
	"github.com/hexane/atlas/internal/core/pageauthz"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/plugin/service"
)

// The service dependency API.
//
// Shaped for graph rendering rather than for tables: every response that
// describes relationships returns `nodes` and `edges` with stable ids, which
// is what a force-directed or layered layout consumes directly. A tree-shaped
// payload would force the client to flatten it back into a graph, and a
// systemd dependency structure is not a tree in any case — it has shared
// dependencies and genuine cycles.
//
// Relationships are computed here, never on the client. The rules that decide
// what "affected" means — ordering is not requirement, a `Wants` link
// insulates everything behind it — are the substance of this feature, and
// re-deriving them in TypeScript would mean maintaining them twice and having
// them disagree.

// defaultGraphNodes bounds an unrooted graph request.
//
// A host with thousands of units would otherwise ship its entire structure to
// draw one panel. Callers wanting a specific area pass `root` and `depth`.
const defaultGraphNodes = 400

// maxTraversalNodes bounds rooted traversals and impact analysis.
const maxTraversalNodes = 2000

// GraphNodeResponse is one unit as a graph vertex.
type GraphNodeResponse struct {
	// ID is the unit name and is the stable identity edges refer to.
	ID   string `json:"id"`
	Type string `json:"type"`

	Description string `json:"description,omitempty"`
	LoadState   string `json:"load_state,omitempty"`
	ActiveState string `json:"active_state,omitempty"`
	SubState    string `json:"sub_state,omitempty"`

	// Health folds the unit's own state together with its dependencies'.
	Health string `json:"health"`
	// FailedDependencies names the failed units behind a degraded verdict, so
	// the client can explain the health rather than merely display it.
	FailedDependencies []string `json:"failed_dependencies,omitempty"`

	// Known is false for a unit referenced by an edge that the manager does
	// not have — a typo in a unit file, or a package removed without its
	// dependents being updated. A real finding, not something to hide.
	Known bool `json:"known"`

	// Dependencies and Dependents are degree counts, so a layout can size or
	// rank a node without walking the edge list.
	Dependencies int `json:"dependencies"`
	Dependents   int `json:"dependents"`

	// Depth is hops from the traversal root. Absent on unrooted graphs.
	Depth *int `json:"depth,omitempty"`
}

// GraphEdgeResponse is one canonical relationship.
//
// Always dependent → dependency: "A requires B" and "A starts after B" are
// both A→B. systemd reports every relationship from both sides; the collector
// canonicalises them so the client never sees the same relationship twice.
type GraphEdgeResponse struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Kind is the systemd directive: requires, wants, binds_to, part_of,
	// after, conflicts.
	Kind string `json:"kind"`
	// Class is what the kind means for failure analysis: requirement,
	// ordering, or conflict. Sent alongside kind so a client can style or
	// filter without hard-coding the mapping.
	Class string `json:"class"`
}

// ServiceGraphResponse is a renderable graph.
type ServiceGraphResponse struct {
	Nodes []GraphNodeResponse `json:"nodes"`
	Edges []GraphEdgeResponse `json:"edges"`

	TotalNodes int `json:"total_nodes"`
	TotalEdges int `json:"total_edges"`
	// Truncated reports that a cap stopped the result short, so a client can
	// say so rather than presenting a partial graph as complete.
	Truncated bool `json:"truncated"`

	// CollectedAt is when the *structure* was read. State is read per request,
	// so this is not the age of the states shown.
	CollectedAt time.Time `json:"collected_at,omitzero"`
	Root        string    `json:"root,omitempty"`
}

// ServiceGraph returns the dependency graph, whole or rooted at one unit.
//
//	GET /services/graph
//	GET /services/graph?root=nginx.service&depth=2&direction=dependents&class=requirement
func (h *Handler) ServiceGraph(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ServiceGraph"

	scope, err := h.requireScope(r, user.PermissionNodeRead, pageauthz.PageServices)
	if err != nil {
		return err
	}
	graph, err := h.serviceGraph(r.Context(), scope)
	if err != nil {
		return err
	}

	q := r.URL.Query()
	root := strings.TrimSpace(q.Get("root"))

	depth, err := intParam(q.Get("depth"), 0, op)
	if err != nil {
		return err
	}
	limit, err := intParam(q.Get("limit"), defaultGraphNodes, op)
	if err != nil {
		return err
	}
	classes, err := classParam(q.Get("class"), op)
	if err != nil {
		return err
	}
	direction, err := directionParam(q.Get("direction"), op)
	if err != nil {
		return err
	}

	totalNodes, totalEdges := graph.Len()
	resp := ServiceGraphResponse{
		TotalNodes:  totalNodes,
		TotalEdges:  totalEdges,
		CollectedAt: graph.CollectedAt(),
		Root:        root,
	}

	if root != "" {
		if _, ok := graph.Node(root); !ok {
			return errs.New(errs.CodeNotFound, "no such unit").
				WithOp(op).WithDetail("unit", root)
		}
		reach := graph.Traverse(root, direction, service.TraverseOptions{
			Depth:    depth,
			Classes:  classes,
			MaxNodes: min(limit, maxTraversalNodes),
		})
		resp.Truncated = reach.Truncated
		for _, n := range reach.Nodes {
			d := n.Depth
			node := presentGraphNode(graph, n.Node)
			node.Depth = &d
			resp.Nodes = append(resp.Nodes, node)
		}
		resp.Edges = presentEdges(reach.Edges)
		httpx.JSON(w, r, http.StatusOK, resp)
		return nil
	}

	// Unrooted: the whole structure, capped. Nodes come back name-ordered, so
	// a truncated result is at least stable between polls rather than an
	// arbitrary subset that changes shape each time.
	nodes := graph.Nodes()
	if len(nodes) > limit {
		nodes = nodes[:limit]
		resp.Truncated = true
	}

	included := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		included[n.Name] = struct{}{}
		resp.Nodes = append(resp.Nodes, presentGraphNode(graph, n))
	}
	// Only edges whose endpoints are both present, so a truncated graph never
	// carries an edge to a node the client does not have.
	for _, e := range graph.Edges() {
		if _, ok := included[e.From]; !ok {
			continue
		}
		if _, ok := included[e.To]; !ok {
			continue
		}
		resp.Edges = append(resp.Edges, presentEdge(e))
	}

	httpx.JSON(w, r, http.StatusOK, resp)
	return nil
}

// ServiceDetailResponse is one unit with its immediate relationships.
type ServiceDetailResponse struct {
	Node GraphNodeResponse `json:"node"`
	// Dependencies and Dependents are the direct edges, one hop only.
	Dependencies []GraphEdgeResponse `json:"dependencies"`
	Dependents   []GraphEdgeResponse `json:"dependents"`
	// Impact is the blast radius if this unit fails.
	Impact ServiceImpactResponse `json:"impact"`
}

// ServiceImpactResponse is the set of units affected by a failure.
//
// Hard and soft are separated deliberately. A `Requires` dependent cannot run
// without this unit; a `Wants` dependent runs without whatever it provides.
// Reporting one number would overstate every outage.
type ServiceImpactResponse struct {
	Hard      []string `json:"hard"`
	Soft      []string `json:"soft"`
	Truncated bool     `json:"truncated"`
}

// ServiceDetail returns one unit, its direct relationships, and its impact.
//
//	GET /services/{unit}
func (h *Handler) ServiceDetail(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ServiceDetail"

	scope, err := h.requireScope(r, user.PermissionNodeRead, pageauthz.PageServices)
	if err != nil {
		return err
	}
	graph, err := h.serviceGraph(r.Context(), scope)
	if err != nil {
		return err
	}

	name := r.PathValue("unit")
	if name == "" {
		return errs.New(errs.CodeInvalidArgument, "a unit name is required").
			WithOp(op).WithDetail("field", "unit")
	}

	node, ok := graph.Node(name)
	if !ok {
		return errs.New(errs.CodeNotFound, "no such unit").
			WithOp(op).WithDetail("unit", name)
	}

	impact := graph.Impact(name, maxTraversalNodes)

	httpx.JSON(w, r, http.StatusOK, ServiceDetailResponse{
		Node:         presentGraphNode(graph, node),
		Dependencies: presentEdges(graph.Dependencies(name)),
		Dependents:   presentEdges(graph.Dependents(name)),
		Impact: ServiceImpactResponse{
			Hard: impact.Hard, Soft: impact.Soft, Truncated: impact.Truncated,
		},
	})
	return nil
}

// ServiceImpact returns only the blast radius, for callers that do not need
// the surrounding detail.
//
//	GET /services/{unit}/impact
func (h *Handler) ServiceImpact(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ServiceImpact"

	scope, err := h.requireScope(r, user.PermissionNodeRead, pageauthz.PageServices)
	if err != nil {
		return err
	}
	graph, err := h.serviceGraph(r.Context(), scope)
	if err != nil {
		return err
	}

	name := r.PathValue("unit")
	if _, ok := graph.Node(name); !ok {
		return errs.New(errs.CodeNotFound, "no such unit").
			WithOp(op).WithDetail("unit", name)
	}

	impact := graph.Impact(name, maxTraversalNodes)
	httpx.JSON(w, r, http.StatusOK, ServiceImpactResponse{
		Hard: impact.Hard, Soft: impact.Soft, Truncated: impact.Truncated,
	})
	return nil
}

// serviceGraph resolves the graph, distinguishing "no service manager on this
// host" from "the manager failed", the same way every other inventory does.
//
// Unlike every other subject in inventory.go, this does not read a stored
// snapshot for a remote scope — it still refuses one outright via
// [inventory.ErrRemoteUnavailable]. [service.Graph] is a traversal structure
// with methods (Traverse, Impact, Propagate), not a plain slice a JSON
// snapshot can round-trip into; making it work remotely means either shipping
// the whole graph's structure over the wire and reconstructing it faithfully,
// or having the agent compute traversals itself, and neither is a small
// addition. This is deferred, not silently dropped — see the implementation
// report for this milestone.
func (h *Handler) serviceGraph(ctx context.Context, scope inventory.Scope) (*service.Graph, error) {
	const op = "v1.Handler.serviceGraph"

	if err := h.requirePlugin("service"); err != nil {
		return nil, err
	}

	graph, err := h.deps.Collection.ServiceGraph(ctx, scope)
	if err != nil {
		return nil, err
	}
	if graph == nil {
		return nil, errs.New(errs.CodeNotImplemented,
			"the service integration is not available on this host").WithOp(op)
	}
	return graph, nil
}

func presentGraphNode(g *service.Graph, n service.Node) GraphNodeResponse {
	prop := g.Propagate(n.Name)
	return GraphNodeResponse{
		ID:                 n.Name,
		Type:               n.Type,
		Description:        n.Description,
		LoadState:          n.LoadState,
		ActiveState:        string(n.ActiveState),
		SubState:           string(n.SubState),
		Health:             string(prop.Health),
		FailedDependencies: prop.FailedDependencies,
		Known:              n.Known,
		Dependencies:       len(g.Dependencies(n.Name)),
		Dependents:         len(g.Dependents(n.Name)),
	}
}

func presentEdge(e service.Edge) GraphEdgeResponse {
	return GraphEdgeResponse{
		From:  e.From,
		To:    e.To,
		Kind:  string(e.Kind),
		Class: string(e.Kind.Class()),
	}
}

func presentEdges(es []service.Edge) []GraphEdgeResponse {
	out := make([]GraphEdgeResponse, 0, len(es))
	for _, e := range es {
		out = append(out, presentEdge(e))
	}
	return out
}

func intParam(raw string, fallback int, op string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, errs.New(errs.CodeInvalidArgument,
			"must be a non-negative integer").WithOp(op).WithDetail("value", raw)
	}
	return n, nil
}

func classParam(raw string, op string) ([]service.EdgeClass, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "all" {
		return nil, nil
	}
	var out []service.EdgeClass
	for _, part := range strings.Split(raw, ",") {
		switch service.EdgeClass(strings.TrimSpace(part)) {
		case service.ClassRequirement:
			out = append(out, service.ClassRequirement)
		case service.ClassOrdering:
			out = append(out, service.ClassOrdering)
		case service.ClassConflict:
			out = append(out, service.ClassConflict)
		default:
			return nil, errs.New(errs.CodeInvalidArgument,
				"class must be requirement, ordering, conflict, or all").
				WithOp(op).WithDetail("value", part)
		}
	}
	return out, nil
}

func directionParam(raw string, op string) (service.Direction, error) {
	switch strings.TrimSpace(raw) {
	case "", string(service.TowardDependencies):
		return service.TowardDependencies, nil
	case string(service.TowardDependents):
		return service.TowardDependents, nil
	default:
		return "", errs.New(errs.CodeInvalidArgument,
			"direction must be dependencies or dependents").
			WithOp(op).WithDetail("value", raw)
	}
}
