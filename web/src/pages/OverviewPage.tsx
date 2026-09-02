import { useMemo, useState } from "react";
import { Cpu, Gauge, HardDrive, MemoryStick } from "lucide-react";
import {
  useCollectors,
  useContainers,
  useCronJobs,
  useLatestMetrics,
  useMetricSeries,
  useMounts,
  useNodes,
  usePorts,
  usePrimaryNodeID,
  useProcesses,
  useServices,
  useSystemHealth,
  useSystemRuntime,
} from "../api/queries";
import { setSelectedNodeID } from "../lib/selectedNode";
import type { CollectorHealth, LatestValue, MetricUnit, Node, Series } from "../api/types";
import { emptyArray } from "../api/empty";
import { AttentionRequired } from "./overview/AttentionRequired";
import { FleetSummary } from "./overview/FleetSummary";
import { computeFindings, type Finding } from "./overview/findings";
import { TimeSeriesChart, toChartSeries } from "../components/Chart";
import { Card, CardHeader } from "../components/Card";
import { RecentActivity } from "../components/RecentActivity";
import { Environments } from "../components/Environments";
import { StatCard } from "../components/StatCard";
import { Badge, type Tone } from "../components/Badge";
import { PageHeader } from "../components/PageHeader";
import { QueryState } from "../components/QueryState";
import { emptyArt, errorArt } from "../lib/assets";
import { nodePrimaryLabel, nodeSecondaryLabel } from "../lib/nodeIdentity";
import { TABLE, TABLE_WRAP, TD, TD_MUTED, TD_NUM, TH, THEAD_TR, TR } from "../components/table";
import { formatAgo, formatDuration, formatValue } from "../format";

const RANGES = ["15m", "1h", "6h", "24h"] as const;
type Range = (typeof RANGES)[number];

const CHART_METRICS = [
  "system.cpu.usage",
  "system.memory.usage",
  "system.network.rx.bytes",
  "system.network.tx.bytes",
  "system.diskio.read.bytes",
  "system.diskio.write.bytes",
];

/** The landing page: this node's vitals, right now and over time. */
export function OverviewPage() {
  const nodes = useNodes();
  const [range, setRange] = useState<Range>("1h");

  // The node every panel on this page reflects: the same one the shell node
  // switcher and every other inventory page read. Overview used to keep its
  // own independent selection that defaulted to `nodeList[0]`, which churns
  // as nodes reorder by last_seen_at and left this page showing a different
  // host from the rest of the app.
  const nodeID = usePrimaryNodeID();

  // Stable identity when empty: a fresh [] each render would re-run the
  // findings memo on every poll.
  const nodeList = nodes.data?.nodes ?? emptyArray<Node>();
  const node = nodeList.find((n) => n.node_id === nodeID);

  const latest = useLatestMetrics(nodeID);
  const series = useMetricSeries(nodeID, CHART_METRICS, range);
  const collectors = useCollectors();
  const health = useSystemHealth();
  const runtime = useSystemRuntime();

  // The operations centre reads every source at once. These are the same
  // queries the individual pages use, so they are served from one shared
  // cache rather than fetched twice.
  const services = useServices(nodeID);
  const containers = useContainers(nodeID);
  const mounts = useMounts(nodeID);
  const ports = usePorts(nodeID);
  const processes = useProcesses(nodeID);
  const cron = useCronJobs(nodeID);

  const values = useMemo(() => indexLatest(latest.data?.values ?? []), [latest.data]);

  const { findings, coverage } = useMemo(
    () =>
      computeFindings({
        nodes: nodeList,
        services: { data: services.data?.services ?? [], error: services.error },
        containers: { data: containers.data?.containers ?? [], error: containers.error },
        mounts: { data: mounts.data?.mounts ?? [], error: mounts.error },
        ports: { data: ports.data?.ports ?? [], error: ports.error },
        processes: { data: processes.data?.processes ?? [], error: processes.error },
        cron: { data: cron.data, error: cron.error },
        collectors: collectors.data,
        health: health.data,
        runtime: runtime.data,
      }),
    [nodeList, services.data, services.error, containers.data, containers.error, mounts.data,
     mounts.error, ports.data, ports.error, processes.data, processes.error, cron.data,
     cron.error, collectors.data, health.data, runtime.data],
  );

  const critical = findings.filter((f) => f.severity === "critical").length;

  if (nodes.isPending || nodes.error || !node) {
    return (
      <>
        <PageHeader title="Overview" subtitle="This node's vitals, right now and over time." />
        <QueryState
          isPending={nodes.isPending}
          error={nodes.error}
          isEmpty={!node}
          onRetry={() => void nodes.refetch()}
          rows={4}
          empty={{
            art: emptyArt.servers,
            title: "No servers are reporting yet",
            description:
              "Atlas registers a node the first time it completes a sweep. Until one does, there are no vitals to show and nothing is wrong.",
            hint: "Collection begins within one interval of startup.",
          }}
        />
      </>
    );
  }

  const cpu = values.get("system.cpu.usage");
  const memory = values.get("system.memory.usage");
  const load = values.get("system.load.average|window=1m");
  const disk = fullestDisk(latest.data?.values ?? []);

  return (
    <>
      <PageHeader
        subtitle={verdictLine(findings, nodeList)}
        action={<RangePicker range={range} onChange={setRange} />}
        stats={[
          {
            label: "Fleet",
            value: `${nodeList.filter((n) => n.status === "up").length}/${nodeList.length}`,
            hint: "nodes reporting",
            tone: nodeList.some((n) => n.status === "down") ? "danger" : "success",
          },
          {
            label: "CPU",
            value: cpu ? formatValue(cpu.value, "percent") : "—",
            hint: nodePrimaryLabel(node),
            tone: cpu && cpu.value >= 90 ? "danger" : cpu && cpu.value >= 75 ? "warning" : "default",
          },
          {
            label: "Memory",
            value: memory ? formatValue(memory.value, "percent") : "—",
            hint: node.cpu_cores ? `${node.cpu_cores} cores` : "",
            tone: memory && memory.value >= 90 ? "danger" : memory && memory.value >= 75 ? "warning" : "default",
          },
          {
            label: "Attention",
            value: findings.length === 0 ? "clear" : String(findings.length),
            hint:
              findings.length === 0
                ? `${coverage.filter((c) => c.ok).length} sources checked`
                : critical > 0
                  ? `${critical} critical`
                  : "needs review",
            tone: critical > 0 ? "danger" : findings.length > 0 ? "warning" : "success",
          },
        ]}
      />

      {/* The operations centre leads with what is wrong. Everything below
          describes the machine; this says whether anyone needs to act. */}
      <AttentionRequired findings={findings} coverage={coverage} />

      {/* The fleet roll-up. Its chips are also the selector for the vitals
          below, so "how is the fleet" and "which host am I reading" are one
          control instead of two rows listing the same machines. */}
      <FleetSummary nodes={nodeList} selectedID={node.node_id} onSelect={setSelectedNodeID} />

      <h2 className="eyebrow mb-3">
        This node · {nodePrimaryLabel(node)}
        {nodeSecondaryLabel(node) ? (
          <span className="ml-2 font-normal normal-case text-text-muted">
            {nodeSecondaryLabel(node)}
          </span>
        ) : null}
      </h2>

      <div className="mb-4 flex flex-wrap items-center gap-4 text-sm text-text-muted">
        <NodeStatusBadge status={node.status} />
        {node.platform ? <span>{node.platform}</span> : null}
        {node.kernel ? <span>{node.kernel}</span> : null}
        {node.cpu_cores ? <span>{node.cpu_cores} cores</span> : null}
        {node.uptime_seconds ? <span>up {formatDuration(node.uptime_seconds)}</span> : null}
        <span>last seen {formatAgo(node.seconds_since_seen)}</span>
      </div>

      <div className="mb-6 grid grid-cols-2 gap-4 lg:grid-cols-4">
        <StatCard
          label="CPU"
          icon={Cpu}
          value={cpu ? formatValue(cpu.value, "percent") : "—"}
          tone={toneForPercent(cpu?.value)}
          iconTone={iconToneForPercent(cpu?.value, "info")}
        />
        <StatCard
          label="Memory"
          icon={MemoryStick}
          value={memory ? formatValue(memory.value, "percent") : "—"}
          tone={toneForPercent(memory?.value)}
          iconTone={iconToneForPercent(memory?.value, "primary")}
        />
        <StatCard
          label="Load (1m)"
          icon={Gauge}
          value={load ? formatValue(load.value, "ratio") : "—"}
          iconTone="info"
        />
        <StatCard
          label={disk ? `Disk · ${disk.labels?.mountpoint ?? ""}` : "Disk"}
          icon={HardDrive}
          value={disk ? formatValue(disk.value, "percent") : "—"}
          tone={toneForPercent(disk?.value)}
          iconTone={iconToneForPercent(disk?.value, "primary")}
        />
      </div>

      <div className="mb-6 grid grid-cols-1 gap-4 xl:grid-cols-[2fr_1fr]">
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          <ChartCard title="CPU utilisation" query={series} metrics={["system.cpu.usage"]} labels={["CPU"]} unit="percent" percentScale area />
        <ChartCard title="Memory usage" query={series} metrics={["system.memory.usage"]} labels={["Memory"]} unit="percent" percentScale area />
        <ChartCard title="Network throughput" query={series} metrics={["system.network.rx.bytes", "system.network.tx.bytes"]} labels={["Received", "Sent"]} unit="bytes_per_second" />
          <ChartCard title="Disk throughput" query={series} metrics={["system.diskio.read.bytes", "system.diskio.write.bytes"]} labels={["Read", "Written"]} unit="bytes_per_second" />
        </div>
        <RecentActivity />
      </div>

      <h2 className="eyebrow mt-8 mb-3">Platform</h2>

      {/* `items-start` matters here: the collector table runs to a dozen rows
          and the environments panel to two, and a stretched grid row turns
          that difference into a tall empty card. Narrower column for the
          shorter panel, and neither stretches. */}
      <div className="mb-6 grid grid-cols-1 items-start gap-4 xl:grid-cols-[minmax(0,1fr)_minmax(0,1.6fr)]">
        <Environments nodes={nodeList} />
        <CollectorTable query={collectors} />
      </div>
    </>
  );
}

function RangePicker({ range, onChange }: { range: Range; onChange: (r: Range) => void }) {
  return (
    <div role="group" aria-label="Time range" className="flex rounded-lg border border-border p-1">
      {RANGES.map((r) => (
        <button
          key={r}
          type="button"
          aria-pressed={r === range}
          onClick={() => { onChange(r); }}
          className={`rounded-md px-3 py-1 text-xs font-medium transition-colors ${
            r === range ? "bg-primary text-white" : "text-text-muted hover:bg-surface-hover"
          }`}
        >
          {r}
        </button>
      ))}
    </div>
  );
}

/**
 * The hero's one-line verdict.
 *
 * Derived from the findings rather than from its own copy of the rules. When
 * this had independent logic the hero could read "all systems operational"
 * while the panel below it listed a failed unit — the two disagreeing about
 * the same host is worse than either being wrong alone.
 */
function verdictLine(findings: Finding[], nodes: Node[]): string {
  const worst = findings[0];
  if (!worst) {
    return nodes.length === 1
      ? "Nothing needs attention on this host."
      : `Nothing needs attention across ${String(nodes.length)} nodes.`;
  }
  if (findings.length === 1) return `${worst.title}. ${worst.detail}`;
  const critical = findings.filter((f) => f.severity === "critical").length;
  return critical > 0
    ? `${String(findings.length)} findings need review, ${String(critical)} of them critical.`
    : `${String(findings.length)} findings need review.`;
}

function toneForPercent(value: number | undefined): "neutral" | "warning" | "danger" {
  if (value === undefined) return "neutral";
  if (value >= 90) return "danger";
  if (value >= 75) return "warning";
  return "neutral";
}

/** The icon badge tracks the same thresholds as the tile's text tone, but
 *  falls back to a tile-specific base colour rather than grey when nothing
 *  is wrong — the same variety the reference design uses to keep four tiles
 *  from reading as one repeated shape. */
function iconToneForPercent(
  value: number | undefined,
  base: "primary" | "info",
): "primary" | "info" | "warning" | "danger" {
  if (value === undefined) return base;
  if (value >= 90) return "danger";
  if (value >= 75) return "warning";
  return base;
}

const NODE_STATUS_TONE: Record<Node["status"], Tone> = {
  up: "success",
  stale: "warning",
  down: "danger",
};

function NodeStatusBadge({ status }: { status: Node["status"] }) {
  return (
    <Badge tone={NODE_STATUS_TONE[status]} pulse={status === "down"}>
      {status}
    </Badge>
  );
}

interface ChartCardProps {
  title: string;
  query: ReturnType<typeof useMetricSeries>;
  metrics: string[];
  labels: string[];
  unit: MetricUnit;
  percentScale?: boolean;
  area?: boolean;
}

function ChartCard({ title, query, metrics, labels, unit, percentScale, area }: ChartCardProps) {
  const matched = useMemo(() => {
    const all = query.data?.series ?? [];
    return metrics
      .map((m, i) => {
        const parts = all.filter((s) => s.metric === m);
        if (parts.length === 0) return null;
        return toChartSeries(sumSeries(parts), labels[i] ?? m, i === 0 ? 1 : 2);
      })
      .filter((s): s is NonNullable<typeof s> => s !== null);
  }, [query.data, metrics, labels]);

  const current = matched[0]?.points.at(-1)?.value;

  return (
    <Card>
      <div className="mb-1 flex items-center justify-between">
        <h3 className="text-card-title font-semibold text-text">{title}</h3>
        {current !== undefined ? (
          <span className="text-sm font-semibold tabular-nums text-text">{formatValue(current, unit)}</span>
        ) : null}
      </div>

      {matched.length > 1 ? (
        <div className="mb-2 flex gap-4 text-xs">
          {matched.map((s) => (
            <span key={s.slot} className="flex items-center gap-1.5 text-text-muted">
              <span
                aria-hidden="true"
                className="h-0.5 w-2.5 rounded-full"
                style={{ background: `var(--series-${s.slot})` }}
              />
              {s.label}
            </span>
          ))}
        </div>
      ) : null}

      <QueryState
        isPending={query.isPending}
        error={query.error}
        isEmpty={matched.length === 0}
        onRetry={() => void query.refetch()}
        rows={2}
        empty={{
          art: emptyArt.reports,
          title: "No data in this range",
          description:
            "Nothing was recorded for this metric over the selected window. A shorter range, or a host that started recently, will often look like this.",
          hint: "Series are written per sweep; a range shorter than one interval can legitimately contain no points.",
        }}
      />
      {!query.isPending && !query.error && matched.length > 0 ? (
        <TimeSeriesChart series={matched} unit={unit} percentScale={percentScale ?? false} area={area ?? false} />
      ) : null}
    </Card>
  );
}

function CollectorTable({ query }: { query: ReturnType<typeof useCollectors> }) {
  const collectors = query.data?.collectors ?? [];
  const scheduler = query.data?.scheduler;

  return (
    <Card>
      <CardHeader
        title="Collectors"
        action={
          scheduler ? (
            <span className="text-xs text-text-muted">
              {scheduler.runs.toLocaleString()} runs · {scheduler.samples.toLocaleString()} samples
              {scheduler.failures > 0 ? ` · ${scheduler.failures} failed` : ""}
            </span>
          ) : null
        }
      />

      <QueryState
        isPending={query.isPending}
        error={query.error}
        isEmpty={collectors.length === 0}
        onRetry={() => void query.refetch()}
        rows={3}
        empty={{
          kind: "unavailable",
          art: errorArt.offline,
          title: "No collectors are registered",
          description:
            "The scheduler is running with nothing to run. Every metric on this page comes from a collector, so an empty registry means the rest of the dashboard cannot fill in.",
          hint: "Collectors register during plugin detection at startup. An empty list points at detection, not at the host.",
        }}
      />

      {!query.isPending && !query.error && collectors.length > 0 ? (
        <div className={TABLE_WRAP}>
          <table className={TABLE}>
            <thead>
              <tr className={THEAD_TR}>
                <th className={TH}>Collector</th>
                <th className={TH}>Interval</th>
                <th className={TH}>Runs</th>
                <th className={TH}>Series</th>
                <th className={TH}>Status</th>
              </tr>
            </thead>
            <tbody>
              {collectors.map((c) => (
                <CollectorRow key={c.collector_id} collector={c} />
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </Card>
  );
}

function CollectorRow({ collector: c }: { collector: CollectorHealth }) {
  const nearLimit = c.series_limit > 0 && c.series > c.series_limit * 0.8;

  return (
    <tr className={TR}>
      <td className={TD}>
        <span className="block font-medium">{c.name}</span>
        <span className="block text-xs text-text-muted">{c.collector_id}</span>
      </td>
      <td className={TD_MUTED}>{c.interval}</td>
      <td className={TD_NUM}>
        {c.total_runs.toLocaleString()}
        {c.total_failures > 0 ? <span className="ml-1 text-danger">· {c.total_failures} failed</span> : null}
      </td>
      <td className={nearLimit ? "px-4 py-3 text-right tabular-nums text-warning" : TD_NUM}>
        {c.series}
        {c.refused_series > 0 ? <span className="ml-1 text-danger">· {c.refused_series} refused</span> : null}
      </td>
      <td className={TD}>
        {c.healthy ? (
          <Badge tone="success">healthy</Badge>
        ) : (
          <Badge tone="danger">{c.last_error ? "failing" : "degraded"}</Badge>
        )}
      </td>
    </tr>
  );
}

function indexLatest(values: LatestValue[]): Map<string, LatestValue> {
  const out = new Map<string, LatestValue>();
  for (const v of values) {
    if (!out.has(v.metric)) out.set(v.metric, v);
    for (const [k, label] of Object.entries(v.labels ?? {})) {
      out.set(`${v.metric}|${k}=${label}`, v);
    }
  }
  return out;
}

function fullestDisk(values: LatestValue[]): LatestValue | undefined {
  return values.filter((v) => v.metric === "system.disk.usage").sort((a, b) => b.value - a.value)[0];
}

function sumSeries(parts: Series[]): Series {
  const first = parts.at(0);
  if (!first) throw new Error("sumSeries requires at least one series");
  if (parts.length === 1) return first;

  const totals = new Map<string, number>();
  for (const part of parts) {
    for (const p of part.points) {
      totals.set(p.time, (totals.get(p.time) ?? 0) + p.value);
    }
  }

  const points = [...totals.entries()]
    .map(([time, value]) => ({ time, value }))
    .sort((a, b) => a.time.localeCompare(b.time));

  return {
    node_id: first.node_id,
    collector_id: first.collector_id,
    metric: first.metric,
    unit: first.unit,
    kind: first.kind,
    points,
  };
}
