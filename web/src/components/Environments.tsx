import { Server } from "lucide-react";
import type { Node } from "../api/types";
import { Card, CardHeader } from "./Card";
import { Badge, type Tone } from "./Badge";

/** Nodes that carry no environment tag are grouped under this label rather
 *  than being hidden or assumed into one — an untagged machine is a real
 *  state, and usually one the operator wants to notice and fix. */
const UNASSIGNED = "unassigned";

interface Group {
  name: string;
  total: number;
  up: number;
  down: number;
  stale: number;
}

/**
 * The fleet broken down by environment.
 *
 * An environment is configuration (`node.environment`), not something Atlas
 * discovers — nothing on a host declares which tier it belongs to. The panel
 * therefore says so plainly when nothing is tagged, rather than rendering an
 * empty shell that looks broken.
 */
export function Environments({ nodes }: { nodes: Node[] }) {
  const groups = groupByEnvironment(nodes);
  const anyTagged = groups.some((g) => g.name !== UNASSIGNED);

  return (
    <Card>
      <CardHeader
        title="Environments"
        action={<span className="text-xs text-text-muted">{groups.length} in use</span>}
      />

      {!anyTagged ? (
        <p className="py-8 text-center text-sm text-text-muted">
          No nodes are tagged yet. Set <code className="font-mono text-xs">node.environment</code> on
          each agent to group the fleet here.
        </p>
      ) : (
        // Two columns at most. This panel now sits in the narrower half of the
        // Platform row, and a third column squeezed the name and its status
        // badge into each other.
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          {groups.map((g) => (
            <EnvironmentCard key={g.name} group={g} />
          ))}
        </div>
      )}
    </Card>
  );
}

function EnvironmentCard({ group: g }: { group: Group }) {
  const tone: Tone = g.down > 0 ? "danger" : g.stale > 0 ? "warning" : "success";
  const label = g.down > 0 ? `${g.down} down` : g.stale > 0 ? `${g.stale} stale` : "healthy";

  return (
    <div className="rounded-lg border border-border bg-bg p-4">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-x-2 gap-y-1.5">
        <span className="flex min-w-0 items-center gap-2 text-sm font-medium text-text capitalize">
          <Server size={14} className="shrink-0 text-text-muted" />
          <span className="truncate" title={g.name}>{g.name}</span>
        </span>
        <Badge tone={tone} pulse={g.down > 0}>
          {label}
        </Badge>
      </div>
      <div className="flex items-baseline gap-1.5">
        <span className="text-2xl font-semibold tracking-tight text-text">{g.total}</span>
        <span className="text-xs text-text-muted">node{g.total === 1 ? "" : "s"}</span>
        <span className="ml-auto text-xs text-text-muted">{g.up} up</span>
      </div>
    </div>
  );
}

function groupByEnvironment(nodes: Node[]): Group[] {
  const byName = new Map<string, Group>();

  for (const n of nodes) {
    const name = n.environment ?? UNASSIGNED;
    const g = byName.get(name) ?? { name, total: 0, up: 0, down: 0, stale: 0 };
    g.total++;
    if (n.status === "up") g.up++;
    else if (n.status === "down") g.down++;
    else g.stale++;
    byName.set(name, g);
  }

  // Tagged environments first, alphabetically; untagged last, because it is a
  // gap to close rather than a tier to monitor.
  return [...byName.values()].sort((a, b) => {
    if (a.name === UNASSIGNED) return 1;
    if (b.name === UNASSIGNED) return -1;
    return a.name.localeCompare(b.name);
  });
}
