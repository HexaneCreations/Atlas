import { useMemo } from "react";
import type { Node } from "../../api/types";
import { Card, CardHeader } from "../../components/Card";
import { TopList } from "../../components/TopList";
import { formatDuration } from "../../format";
import { UNTAGGED, untaggedLast } from "./useNodeTable";

/**
 * How the fleet is composed: environment, operating system, architecture and
 * uptime.
 *
 * Distinct from the Overview's fleet strip, which is a roll-up and a handoff.
 * This is the detail that roll-up points at, and it exists to answer questions
 * about the shape of the estate rather than its health — "what are we running,
 * on what, and how long has it been up".
 *
 * Every breakdown is drawn from the node record rather than from metrics, so
 * it stays complete when a node stops reporting. A fleet inventory that
 * silently loses the machines that went down is the opposite of useful.
 */

export function FleetDistribution({ nodes }: { nodes: Node[] }) {
  const environments = useMemo(() => countBy(nodes, (n) => n.environment ?? UNTAGGED, untaggedLast), [nodes]);
  const systems = useMemo(() => countBy(nodes, (n) => n.platform ?? n.os ?? "unknown"), [nodes]);
  const architectures = useMemo(() => countBy(nodes, (n) => n.architecture ?? "unknown"), [nodes]);

  return (
    // The four cards stretch to a common height rather than sitting ragged.
    // The tallest is only ~100px taller than the shortest, so the extra space
    // lands inside a bordered card and reads as breathing room; left ragged it
    // read as a hole in the page.
    <div className="mb-6 grid grid-cols-1 gap-4 lg:grid-cols-2 xl:grid-cols-4">
      <Card level="flat">
        <CardHeader title="Environments" action={<Count n={environments.length} noun="tier" />} />
        <TopList items={toItems(environments, nodes.length, "env")} />
      </Card>

      <Card level="flat">
        <CardHeader title="Operating systems" action={<Count n={systems.length} noun="build" />} />
        <TopList items={toItems(systems, nodes.length, "os")} />
      </Card>

      <Card level="flat">
        <CardHeader title="Architectures" action={<Count n={architectures.length} noun="arch" />} />
        <TopList items={toItems(architectures, nodes.length, "arch")} />
      </Card>

      <Card level="flat">
        <CardHeader title="Uptime" action={<span className="text-xs text-text-muted">at last contact</span>} />
        <UptimeOverview nodes={nodes} />
      </Card>
    </div>
  );
}

function Count({ n, noun }: { n: number; noun: string }) {
  return (
    <span className="text-xs text-text-muted">
      {n} {noun}
      {n === 1 ? "" : "s"}
    </span>
  );
}

/**
 * Uptime, as the longest- and shortest-running nodes plus a median.
 *
 * A mean would be misleading across a fleet where one host has been up for a
 * year and another was rebooted an hour ago. The extremes are what an operator
 * is actually looking for: the machine that has never been patched, and the
 * one that restarted when nobody expected it to.
 */
function UptimeOverview({ nodes }: { nodes: Node[] }) {
  // Hostname breaks ties. Without it, nodes booted together — which is the
  // normal case for a container fleet — have identical uptimes, and the names
  // shown for "median" and "shortest" swapped on every poll as the API's
  // ordering varied.
  const known = nodes
    .filter((n): n is Node & { uptime_seconds: number } => typeof n.uptime_seconds === "number")
    .sort((a, b) => b.uptime_seconds - a.uptime_seconds || a.hostname.localeCompare(b.hostname));

  if (known.length === 0) {
    return (
      <p className="py-6 text-center text-sm text-text-muted">
        No node has reported an uptime yet.
      </p>
    );
  }

  const longest = known[0];
  const shortest = known[known.length - 1];
  const median = known[Math.floor(known.length / 2)];
  // Reported at last contact, so for a node that is down this is how long it
  // had been up when it stopped — not how long it has been up now.
  const stale = nodes.some((n) => n.status !== "up");

  return (
    <div className="flex flex-col gap-3">
      <UptimeRow label="Longest" node={longest} />
      {known.length > 2 ? <UptimeRow label="Median" node={median} /> : null}
      {known.length > 1 ? <UptimeRow label="Shortest" node={shortest} /> : null}
      {stale ? (
        <p className="border-t border-border pt-2.5 text-[11px] leading-relaxed text-text-subtle">
          For a node that is not reporting, this is how long it had been up when it went quiet.
        </p>
      ) : null}
    </div>
  );
}

function UptimeRow({ label, node }: { label: string; node: Node | undefined }) {
  if (!node) return null;
  return (
    <div className="flex items-baseline gap-2">
      <span className="eyebrow shrink-0">{label}</span>
      <span className="min-w-0 flex-1 truncate text-xs text-text-muted" title={node.hostname}>
        {node.hostname}
      </span>
      <span className="shrink-0 text-sm font-semibold tabular-nums text-text">
        {node.uptime_seconds ? formatDuration(node.uptime_seconds) : "—"}
      </span>
    </div>
  );
}

function countBy(
  nodes: Node[],
  key: (n: Node) => string,
  sort?: (a: string, b: string) => number,
): [string, number][] {
  const out = new Map<string, number>();
  for (const n of nodes) out.set(key(n), (out.get(key(n)) ?? 0) + 1);
  // Largest group first, so the fleet's dominant configuration leads. A
  // caller-supplied collation wins where one is given, which is how the
  // untagged bucket is kept at the bottom.
  return [...out.entries()].sort((a, b) => (sort ? sort(a[0], b[0]) : b[1] - a[1]));
}

function toItems(entries: [string, number][], total: number, prefix: string) {
  return entries.map(([label, n]) => ({
    key: `${prefix}-${label}`,
    label,
    value: String(n),
    fraction: total > 0 ? n / total : 0,
  }));
}
