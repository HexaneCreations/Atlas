import { useMemo } from "react";
import type { GraphEdge, GraphHealth, GraphNode, ServiceGraph } from "../../api/types";

/**
 * The dependency graph, drawn as depth layers.
 *
 * Layered rather than force-directed, and that is a decision the data forced
 * rather than a stylistic one. The full graph on a plain Debian host is 202
 * nodes and 1,165 edges with a single 236-degree hub (`shutdown.target`);
 * simulated as a force layout it is an unreadable hairball, and it jitters on
 * every poll because the simulation restarts. Rooted at one unit and bounded
 * by depth it is 4–35 nodes, which lays out cleanly in columns.
 *
 * The `depth` the API already computes is the layout. Column = hops from the
 * root, vertical position = name order within the column. That makes the
 * drawing deterministic: the same graph renders identically every poll, so
 * nothing moves while somebody is reading it.
 *
 * Edge styling carries meaning rather than decoration. A solid line is a
 * requirement — failure travels along it. A dashed line is ordering only, and
 * nothing propagates. That is the distinction the whole backend was built
 * around, and it has to survive into the picture.
 */

const HEALTH_FILL: Record<GraphHealth, string> = {
  healthy: "var(--success)",
  degraded: "var(--warning)",
  failed: "var(--danger)",
  inactive: "var(--text-subtle)",
  unknown: "var(--text-subtle)",
};

const NODE_W = 168;
const NODE_H = 40;
const COL_GAP = 96;
const ROW_GAP = 12;
const PAD = 16;

interface Placed {
  node: GraphNode;
  x: number;
  y: number;
}

export function DependencyGraph({
  graph,
  root,
  direction,
  selected,
  onSelect,
}: {
  graph: ServiceGraph;
  root: string;
  direction: "dependencies" | "dependents";
  selected: string | null;
  onSelect: (unit: string) => void;
}) {
  const layout = useMemo(() => buildLayout(graph, direction), [graph, direction]);

  if (layout.placed.length === 0) {
    return (
      <p className="py-10 text-center text-sm text-text-muted">
        Nothing to draw — this unit has no relationships in the selected class.
      </p>
    );
  }

  const byID = new Map(layout.placed.map((p) => [p.node.id, p]));

  return (
    // Bounded and scrollable in both axes. A depth-2 view can be 35 nodes
    // tall — `sysinit.target` alone fans out to thirty `Wants` — which
    // unbounded renders a 1,600px SVG that pushes the rest of the page off
    // screen. The graph gets a viewport; it does not get to resize the page.
    <div className="scroll-thin max-h-[30rem] overflow-auto rounded-lg border border-border">
      <svg
        viewBox={`0 0 ${String(layout.width)} ${String(layout.height)}`}
        width={layout.width}
        height={layout.height}
        role="img"
        aria-label={`Dependency graph for ${root}, ${String(graph.nodes.length)} units`}
        className="max-w-none"
      >
        <defs>
          <marker
            id="dep-arrow"
            viewBox="0 0 8 8"
            refX="7"
            refY="4"
            markerWidth="6"
            markerHeight="6"
            orient="auto-start-reverse"
          >
            <path d="M0,0 L8,4 L0,8 z" fill="var(--border)" />
          </marker>
        </defs>

        {graph.edges.map((e) => {
          const a = byID.get(direction === "dependents" ? e.to : e.from);
          const b = byID.get(direction === "dependents" ? e.from : e.to);
          if (!a || !b) return null;
          return (
            <EdgeLine
              key={`${e.from}|${e.to}|${e.kind}`}
              edge={e}
              from={a}
              to={b}
              dimmed={selected !== null && selected !== e.from && selected !== e.to}
            />
          );
        })}

        {layout.placed.map((p) => (
          <NodeBox
            key={p.node.id}
            placed={p}
            isRoot={p.node.id === root}
            isSelected={p.node.id === selected}
            onSelect={() => { onSelect(p.node.id); }}
          />
        ))}
      </svg>
    </div>
  );
}

function EdgeLine({
  edge,
  from,
  to,
  dimmed,
}: {
  edge: GraphEdge;
  from: Placed;
  to: Placed;
  dimmed: boolean;
}) {
  const x1 = from.x + NODE_W;
  const y1 = from.y + NODE_H / 2;
  const x2 = to.x;
  const y2 = to.y + NODE_H / 2;
  const mid = (x1 + x2) / 2;

  const requirement = edge.class === "requirement";

  return (
    <path
      d={`M${String(x1)},${String(y1)} C${String(mid)},${String(y1)} ${String(mid)},${String(y2)} ${String(x2)},${String(y2)}`}
      fill="none"
      stroke={requirement ? "var(--text-subtle)" : "var(--border)"}
      strokeWidth={requirement ? 1.5 : 1}
      // Dashed means ordering: nothing propagates along it. Solid means
      // requirement: failure does.
      strokeDasharray={requirement ? undefined : "3 3"}
      opacity={dimmed ? 0.25 : 1}
      markerEnd="url(#dep-arrow)"
    >
      <title>
        {edge.from} {edge.kind} {edge.to}
        {requirement ? "" : " (ordering only — failure does not propagate)"}
      </title>
    </path>
  );
}

function NodeBox({
  placed,
  isRoot,
  isSelected,
  onSelect,
}: {
  placed: Placed;
  isRoot: boolean;
  isSelected: boolean;
  onSelect: () => void;
}) {
  const { node, x, y } = placed;
  const fill = HEALTH_FILL[node.health];
  const short = node.id.replace(/\.(service|target|socket|mount|timer|slice|device|path|automount|scope)$/, "");

  return (
    <g
      onClick={onSelect}
      className="cursor-pointer"
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect();
        }
      }}
      aria-label={`${node.id}, ${node.health}`}
    >
      <rect
        x={x}
        y={y}
        width={NODE_W}
        height={NODE_H}
        rx={8}
        fill="var(--surface)"
        stroke={isSelected || isRoot ? "var(--primary)" : "var(--border)"}
        strokeWidth={isSelected || isRoot ? 2 : 1}
      />
      {/* A health rail rather than a filled box: the label has to stay
          readable, and a coloured edge reads at a glance without fighting it. */}
      <rect x={x} y={y} width={3.5} height={NODE_H} rx={2} fill={fill} />

      <text
        x={x + 12}
        y={y + 17}
        className="fill-text"
        style={{ fontSize: 12, fontWeight: isRoot ? 600 : 500 }}
      >
        {truncate(short, 20)}
      </text>
      <text x={x + 12} y={y + 31} className="fill-text-subtle" style={{ fontSize: 10 }}>
        {node.type}
        {node.known ? "" : " · not on host"}
      </text>

      <title>
        {node.id}
        {"\n"}health: {node.health}
        {node.active_state ? `\nstate: ${node.active_state}` : ""}
        {node.description ? `\n${node.description}` : ""}
        {node.known ? "" : "\nreferenced by a unit file but not present on this host"}
      </title>
    </g>
  );
}

function truncate(s: string, n: number): string {
  return s.length <= n ? s : `${s.slice(0, n - 1)}…`;
}

/**
 * Places nodes in columns by depth.
 *
 * Dependents are drawn right-to-left so the arrow still reads "depends on":
 * in that direction the further-out units are the ones that need the root, and
 * putting them on the left keeps every arrow pointing at what it requires.
 */
function buildLayout(graph: ServiceGraph, direction: "dependencies" | "dependents") {
  const byDepth = new Map<number, GraphNode[]>();
  for (const n of graph.nodes) {
    const d = n.depth ?? 0;
    byDepth.set(d, [...(byDepth.get(d) ?? []), n]);
  }

  const depths = [...byDepth.keys()].sort((a, b) => a - b);
  const maxDepth = depths.length > 0 ? Math.max(...depths) : 0;

  const placed: Placed[] = [];
  let tallest = 0;

  for (const depth of depths) {
    const column = (byDepth.get(depth) ?? []).slice().sort((a, b) => a.id.localeCompare(b.id));
    // Column index flips for dependents so arrows keep pointing at the
    // dependency rather than away from it.
    const col = direction === "dependents" ? maxDepth - depth : depth;

    column.forEach((node, i) => {
      placed.push({
        node,
        x: PAD + col * (NODE_W + COL_GAP),
        y: PAD + i * (NODE_H + ROW_GAP),
      });
    });
    tallest = Math.max(tallest, column.length);
  }

  return {
    placed,
    tallest,
    width: PAD * 2 + (maxDepth + 1) * NODE_W + maxDepth * COL_GAP,
    height: PAD * 2 + tallest * NODE_H + Math.max(tallest - 1, 0) * ROW_GAP,
  };
}
