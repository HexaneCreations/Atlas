import { useMemo } from "react";
import type { LatestValue, Node } from "../../api/types";
import { Card, CardHeader } from "../../components/Card";
import { formatValue } from "../../format";
import { readVitals } from "./nodeMetrics";

/**
 * Every reporting node's live figures, side by side.
 *
 * The point of this panel is relative reading: one machine at 90% memory is
 * only alarming once you can see the other three sitting at 40%. Bars are
 * therefore scaled to a fixed 0–100 for percentages — scaling to the leader,
 * as the ranked lists elsewhere do, would make the busiest node look pinned
 * whether it was at 12% or 99%.
 *
 * Nodes that are not reporting are excluded and counted underneath rather than
 * rendered as a row of zeroes, which would read as an idle machine.
 */

interface Row {
  node: Node;
  cpu?: number | undefined;
  memory?: number | undefined;
  disk?: number | undefined;
  diskMount?: string | undefined;
  load?: number | undefined;
  loadPerCore?: number | undefined;
  net: number;
  containers?: number | undefined;
}

export function NodeComparison({
  nodes,
  metricsByNode,
}: {
  nodes: Node[];
  metricsByNode: Map<string, LatestValue[]>;
}) {
  const { rows, silent } = useMemo(() => {
    const rows: Row[] = [];
    let silent = 0;

    for (const node of nodes) {
      const v = readVitals(metricsByNode.get(node.node_id) ?? []);
      if (!v.reporting) {
        silent++;
        continue;
      }
      rows.push({
        node,
        cpu: v.cpu,
        memory: v.memoryPercent,
        disk: v.disk?.usedPercent,
        diskMount: v.disk?.mountpoint,
        load: v.load1,
        loadPerCore: v.loadPerCore1,
        net: (v.netRx ?? 0) + (v.netTx ?? 0),
        containers: v.containersRunning,
      });
    }

    // Busiest first by CPU: the comparison exists to find the outlier.
    rows.sort((a, b) => (b.cpu ?? 0) - (a.cpu ?? 0));
    return { rows, silent };
  }, [nodes, metricsByNode]);

  if (rows.length === 0) {
    return (
      <Card level="flat" className="mb-6">
        <CardHeader title="Node comparison" />
        <p className="py-8 text-center text-sm text-text-muted">
          No node in this selection is reporting, so there are no live figures to compare.
        </p>
      </Card>
    );
  }

  return (
    <Card level="flat" className="mb-6">
      <CardHeader
        title="Node comparison"
        action={
          <span className="text-xs text-text-muted">
            {rows.length} reporting
            {silent > 0 ? ` · ${String(silent)} not reporting, excluded` : ""}
          </span>
        }
      />

      <div className="scroll-thin overflow-x-auto">
        <table aria-label="Node comparison" className="w-full border-collapse text-sm">
          <thead>
            <tr className="border-b border-border">
              <Th align="left">Node</Th>
              <Th>CPU</Th>
              <Th>Memory</Th>
              <Th>Disk</Th>
              <Th align="right">Load 1m</Th>
              <Th align="right">Network</Th>
              <Th align="right">Containers</Th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.node.node_id} className="border-b border-border/60 last:border-0">
                <td className="max-w-[14rem] py-2.5 pr-3">
                  <span className="block truncate font-medium text-text" title={r.node.hostname}>
                    {r.node.hostname}
                  </span>
                  <span className="block truncate text-[11px] text-text-subtle">
                    {r.node.environment ?? "untagged"}
                    {r.node.cpu_cores ? ` · ${String(r.node.cpu_cores)} cores` : ""}
                  </span>
                </td>
                <BarCell value={r.cpu} />
                <BarCell value={r.memory} />
                <BarCell value={r.disk} title={r.diskMount} />
                <td className="px-2 py-2.5 text-right">
                  <span className="block text-sm tabular-nums text-text">
                    {r.load !== undefined ? r.load.toFixed(2) : "—"}
                  </span>
                  {r.loadPerCore !== undefined ? (
                    <span
                      className={`block text-[11px] tabular-nums ${r.loadPerCore >= 1 ? "text-warning" : "text-text-subtle"}`}
                    >
                      {r.loadPerCore.toFixed(2)}/core
                    </span>
                  ) : null}
                </td>
                <td className="px-2 py-2.5 text-right text-xs tabular-nums text-text-muted">
                  {formatValue(r.net, "bytes_per_second")}
                </td>
                <td className="px-2 py-2.5 text-right text-xs tabular-nums text-text-muted">
                  {r.containers ?? "—"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <p className="mt-3 text-[11px] leading-relaxed text-text-subtle">
        Disk shows the fullest filesystem on each node, not an average across mounts — a node is
        constrained by its worst one. Network is the sum across every interface.
      </p>
    </Card>
  );
}

function Th({ children, align = "center" }: { children: React.ReactNode; align?: "left" | "center" | "right" }) {
  return (
    <th
      className={`px-2 py-2 text-[11px] font-semibold tracking-wider text-text-muted uppercase ${
        align === "left" ? "text-left" : align === "right" ? "text-right" : "text-left"
      }`}
    >
      {children}
    </th>
  );
}

/** A percentage on a fixed 0–100 scale, so columns are comparable by eye. */
function BarCell({ value, title }: { value: number | undefined; title?: string | undefined }) {
  if (value === undefined) {
    return <td className="px-2 py-2.5 text-xs text-text-subtle">—</td>;
  }

  const bar = value >= 90 ? "bg-danger" : value >= 75 ? "bg-warning" : "bg-primary";
  const text = value >= 90 ? "text-danger" : value >= 75 ? "text-warning" : "text-text";

  return (
    <td className="px-2 py-2.5" title={title}>
      <span className={`mb-1 block text-xs font-medium tabular-nums ${text}`}>
        {formatValue(value, "percent")}
      </span>
      <span className="block h-1 w-full max-w-24 overflow-hidden rounded-full bg-surface-hover">
        <span
          className={`block h-full rounded-full ${bar}`}
          style={{ width: `${String(Math.min(Math.max(value, 1), 100))}%` }}
        />
      </span>
    </td>
  );
}
