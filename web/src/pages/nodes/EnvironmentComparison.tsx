import { useMemo } from "react";
import type { LatestValue, Node } from "../../api/types";
import { Card, CardHeader } from "../../components/Card";
import { Badge, type Tone } from "../../components/Badge";
import { formatValue } from "../../format";
import { readVitals } from "./nodeMetrics";
import { UNTAGGED, untaggedLast } from "./useNodeTable";

/**
 * Environments side by side.
 *
 * An environment is configuration — `node.environment`, set on the agent —
 * not something Atlas discovers, because nothing on a host declares which tier
 * it belongs to. The panel therefore never invents a tier, and shows an
 * untagged bucket plainly so the gap is visible rather than hidden.
 *
 * Averages here are across *reporting* nodes only, and the count of nodes
 * contributing is stated next to each figure. An environment whose average CPU
 * is drawn from one of four machines is not describing that environment, and
 * the panel has to say so rather than print a confident number.
 */

interface EnvRow {
  name: string;
  total: number;
  up: number;
  stale: number;
  down: number;
  /** Nodes that returned metrics — the denominator for every average. */
  reporting: number;
  cpu?: number | undefined;
  memory?: number | undefined;
  disk?: number | undefined;
  cores: number;
}

export function EnvironmentComparison({
  nodes,
  metricsByNode,
}: {
  nodes: Node[];
  metricsByNode: Map<string, LatestValue[]>;
}) {
  const rows = useMemo(() => buildRows(nodes, metricsByNode), [nodes, metricsByNode]);

  return (
    <Card level="flat" className="mb-6">
      <CardHeader
        title="Environment comparison"
        action={
          <span className="text-xs text-text-muted">
            averages across reporting nodes
          </span>
        }
      />

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
        {rows.map((r) => (
          <EnvCard key={r.name} row={r} />
        ))}
      </div>
    </Card>
  );
}

function EnvCard({ row: r }: { row: EnvRow }) {
  const tone: Tone = r.down > 0 ? "danger" : r.stale > 0 ? "warning" : "success";
  const label = r.down > 0 ? `${r.down} down` : r.stale > 0 ? `${r.stale} stale` : "healthy";
  const untagged = r.name === UNTAGGED;

  return (
    <div className={`rounded-lg border p-4 ${untagged ? "border-dashed border-border" : "border-border"}`}>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-x-2 gap-y-1.5">
        <span
          className={`truncate text-sm font-medium capitalize ${untagged ? "text-text-subtle" : "text-text"}`}
          title={untagged ? "Nodes with no environment tag set on the agent." : r.name}
        >
          {r.name}
        </span>
        <Badge tone={tone} pulse={r.down > 0}>
          {label}
        </Badge>
      </div>

      <div className="mb-3 flex items-baseline gap-1.5">
        <span className="text-2xl font-semibold tracking-tight text-text tabular-nums">{r.total}</span>
        <span className="text-xs text-text-muted">node{r.total === 1 ? "" : "s"}</span>
        <span className="ml-auto text-xs tabular-nums text-text-muted">{r.cores || "—"} cores</span>
      </div>

      {r.reporting === 0 ? (
        <p className="text-xs leading-relaxed text-text-subtle">
          No node in this environment is reporting, so there are no live figures to average.
        </p>
      ) : (
        <dl className="flex flex-col gap-1.5">
          <Metric label="CPU" value={r.cpu} />
          <Metric label="Memory" value={r.memory} />
          <Metric label="Disk" value={r.disk} />
          {r.reporting < r.total ? (
            <p className="mt-1 text-[11px] text-text-subtle">
              Averaged over {r.reporting} of {r.total} nodes — the rest are not reporting.
            </p>
          ) : null}
        </dl>
      )}
    </div>
  );
}

function Metric({ label, value }: { label: string; value: number | undefined }) {
  const tone =
    value === undefined ? "text-text-muted"
      : value >= 90 ? "text-danger"
      : value >= 75 ? "text-warning"
      : "text-text";

  return (
    <div className="flex items-center justify-between gap-2 text-xs">
      <dt className="text-text-muted">{label}</dt>
      <dd className={`font-semibold tabular-nums ${tone}`}>
        {value === undefined ? "—" : formatValue(value, "percent")}
      </dd>
    </div>
  );
}

function buildRows(nodes: Node[], metricsByNode: Map<string, LatestValue[]>): EnvRow[] {
  const byEnv = new Map<string, Node[]>();
  for (const n of nodes) {
    const key = n.environment ?? UNTAGGED;
    byEnv.set(key, [...(byEnv.get(key) ?? []), n]);
  }

  const rows: EnvRow[] = [];

  for (const [name, group] of byEnv) {
    const cpu: number[] = [];
    const memory: number[] = [];
    const disk: number[] = [];
    let reporting = 0;

    for (const n of group) {
      const v = readVitals(metricsByNode.get(n.node_id) ?? []);
      if (!v.reporting) continue;
      reporting++;
      if (v.cpu !== undefined) cpu.push(v.cpu);
      if (v.memoryPercent !== undefined) memory.push(v.memoryPercent);
      // The fullest filesystem, not an average across mounts: an environment
      // is at risk when any of its disks is, and averaging a full root with
      // nine empty synthetic volumes hides exactly that.
      if (v.disk) disk.push(v.disk.usedPercent);
    }

    rows.push({
      name,
      total: group.length,
      up: group.filter((n) => n.status === "up").length,
      stale: group.filter((n) => n.status === "stale").length,
      down: group.filter((n) => n.status === "down").length,
      reporting,
      cpu: mean(cpu),
      memory: mean(memory),
      disk: mean(disk),
      cores: group.reduce((s, n) => s + (n.cpu_cores ?? 0), 0),
    });
  }

  return rows.sort((a, b) => untaggedLast(a.name, b.name));
}

function mean(xs: number[]): number | undefined {
  if (xs.length === 0) return undefined;
  return xs.reduce((s, x) => s + x, 0) / xs.length;
}
