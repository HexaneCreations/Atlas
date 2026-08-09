import { useMemo, useState } from "react";
import { ArrowDown, ArrowUp, ChevronsUpDown, Cpu, MemoryStick } from "lucide-react";
import { useLatestMetrics, useMetricSeries, useProcesses, usePrimaryNodeID } from "../api/queries";
import { ApiError } from "../api/client";
import type { Process, ProcessState, Series } from "../api/types";
import { emptyArray } from "../api/empty";
import { Card, CardHeader } from "../components/Card";
import { EmptyState, EmptyAction } from "../components/EmptyState";
import { Badge, type Tone } from "../components/Badge";
import { PageHeader } from "../components/PageHeader";
import { FreshnessBadge } from "../components/FreshnessBadge";
import { QueryState } from "../components/QueryState";
import { SearchInput, FilterSelect, Toolbar } from "../components/Toolbar";
import { Heatmap, type HeatmapRow } from "../components/viz/Heatmap";
import { StackedArea } from "../components/viz/StackedArea";
import { TopList } from "../components/TopList";
import { ProcessInspector, CopyButton } from "./processes/ProcessInspector";
import { useProcessTable, DEFAULT_FILTERS, type SortKey } from "./processes/useProcessTable";
import { emptyArt, errorArt } from "../lib/assets";
import { formatBytes, formatDuration, formatValue } from "../format";

const STATE_TONE: Record<ProcessState, Tone> = {
  running: "success",
  sleeping: "neutral",
  stopped: "danger",
  zombie: "danger",
  idle: "neutral",
  other: "neutral",
};

/**
 * Band colours for the state chart.
 *
 * These deliberately diverge from the table's badge tones, where stopped and
 * zombie are both `danger`. Two bands the same red are two bands nobody can
 * tell apart, so the chart splits them by severity: a stopped process is
 * suspended and recoverable (amber), a zombie is a reaped-child leak that only
 * grows (red). `other` cannot use the border token it once did — a hairline
 * grey is invisible as a filled area.
 */
const STATE_COLOR: Record<ProcessState, string> = {
  running: "var(--success)",
  sleeping: "var(--border)",
  stopped: "var(--warning)",
  zombie: "var(--danger)",
  idle: "var(--info)",
  other: "var(--text-muted)",
};

const STATE_OPTIONS: { value: "all" | ProcessState; label: string }[] = [
  { value: "all", label: "All states" },
  { value: "running", label: "Running" },
  { value: "sleeping", label: "Sleeping" },
  { value: "stopped", label: "Stopped" },
  { value: "zombie", label: "Zombie" },
  { value: "idle", label: "Idle" },
];

const CPU_OPTIONS = [
  { value: "0", label: "Any CPU" },
  { value: "1", label: "CPU > 1%" },
  { value: "5", label: "CPU > 5%" },
  { value: "25", label: "CPU > 25%" },
];

const MEM_OPTIONS = [
  { value: "0", label: "Any memory" },
  { value: "64", label: "Memory > 64 MiB" },
  { value: "256", label: "Memory > 256 MiB" },
  { value: "1024", label: "Memory > 1 GiB" },
];

/**
 * The process explorer.
 *
 * The table is the page. An operator opens this to find a specific process
 * and understand what it is doing, so the list gets the space and the
 * analytics sit beneath it as support — the reverse of a metrics dashboard,
 * which answers questions nobody arrived with.
 *
 * "Actions" here are read-only, by necessity and by design. Atlas observes and
 * never modifies, so there is no kill and no renice; the useful action is
 * getting a PID or a command line into a terminal, where the operator acts
 * with their own authority and their own audit trail. That is what the copy
 * controls do.
 */
export function ProcessesPage() {
  const processes = useProcesses();
  const nodeID = usePrimaryNodeID();
  const cpuHistory = useMetricSeries(nodeID, ["process.top.cpu"], "1h");
  const stateHistory = useMetricSeries(nodeID, ["process.count"], "1h");
  const latest = useLatestMetrics(nodeID);

  const [selectedPid, setSelectedPid] = useState<number | null>(null);

  const all = processes.data?.processes ?? emptyArray<Process>();
  const { filters, setFilters, users, sorted, sortKey, direction, toggleSort } = useProcessTable(all);

  const byState = useMemo(() => {
    const out: Record<ProcessState, number> = {
      running: 0, sleeping: 0, stopped: 0, zombie: 0, idle: 0, other: 0,
    };
    for (const p of all) out[p.state]++;
    return out;
  }, [all]);

  const totals = useMemo(
    () => ({
      cpu: all.reduce((s, p) => s + p.cpu_percent, 0),
      memory: all.reduce((s, p) => s + p.memory_rss, 0),
      threads: all.reduce((s, p) => s + p.threads, 0),
    }),
    [all],
  );

  const selected = useMemo(() => all.find((p) => p.pid === selectedPid) ?? null, [all, selectedPid]);

  const hostCpu = latest.data?.values.find((v) => v.metric === "system.cpu.usage");
  const hostMem = latest.data?.values.find((v) => v.metric === "system.memory.usage");
  const load = latest.data?.values.find(
    (v) => v.metric === "system.load.average" && v.labels?.window === "1m",
  );

  const heatmap = useMemo(() => toHeatmap(cpuHistory.data?.series ?? []), [cpuHistory.data]);
  const stateStack = useMemo(
    () => toStack(stateHistory.data?.series ?? [], "state", ["sleeping"]),
    [stateHistory.data],
  );

  const topCPU = useMemo(
    () =>
      [...all]
        .sort((a, b) => b.cpu_percent - a.cpu_percent)
        .slice(0, 5)
        .map((p) => ({
          key: `cpu-${p.pid}`,
          label: p.name,
          value: `${p.cpu_percent.toFixed(1)}%`,
          fraction: p.cpu_percent / 100,
        })),
    [all],
  );

  const topMemory = useMemo(() => {
    const s = [...all].sort((a, b) => b.memory_rss - a.memory_rss);
    const leader = s[0];
    const max = leader && leader.memory_rss > 0 ? leader.memory_rss : 1;
    return s.slice(0, 5).map((p) => ({
      key: `mem-${p.pid}`,
      label: p.name,
      value: formatBytes(p.memory_rss),
      fraction: p.memory_rss / max,
    }));
  }, [all]);

  if (processes.error instanceof ApiError && processes.error.code === "not_implemented") {
    return (
      <>
        <PageHeader subtitle="Process enumeration is not available on this host." />
        <Card>
          <EmptyState
            kind="unavailable"
            art={errorArt.forbidden}
            title="Process enumeration is not available"
            description="A hardened container or restricted sandbox is refusing the process list. Atlas reports that rather than showing zero processes, which would be untrue."
            hint="Reading the process table needs visibility of the host PID namespace. A container started without it can see only itself."
          />
        </Card>
      </>
    );
  }

  const total = processes.data?.total ?? 0;

  return (
    <>
      <PageHeader
        action={<FreshnessBadge live={processes.data?.live} observedAt={processes.data?.observed_at} />}
        stats={[
          // While the sweep is in flight every total is legitimately zero, and
          // a hero reading "0 processes · 0 threads" states that the machine is
          // idle. It is not; it is unmeasured. Em-dashes until the data lands.
          processes.isPending
            ? { label: "Processes", value: "—", hint: "collecting" }
            : { label: "Processes", value: total.toLocaleString(), hint: `${byState.running} running` },
          {
            label: "Host CPU",
            value: hostCpu ? formatValue(hostCpu.value, "percent") : "—",
            hint: processes.isPending ? "collecting" : `${totals.cpu.toFixed(0)}% summed across processes`,
            tone: hostCpu && hostCpu.value >= 90 ? "danger" : hostCpu && hostCpu.value >= 75 ? "warning" : "default",
          },
          {
            label: "Host memory",
            value: hostMem ? formatValue(hostMem.value, "percent") : "—",
            hint: processes.isPending ? "collecting" : `${formatBytes(totals.memory)} resident`,
            tone: hostMem && hostMem.value >= 90 ? "danger" : hostMem && hostMem.value >= 75 ? "warning" : "default",
          },
          {
            label: "Load (1m)",
            value: load ? load.value.toFixed(2) : "—",
            hint: processes.isPending ? "collecting" : `${totals.threads.toLocaleString()} threads`,
          },
        ]}
      />

      {/* The explorer: table on the left, inspector docked right. The grid
          collapses to one column below xl, where a docked panel would leave
          the table too narrow to read. */}
      <div className={`grid grid-cols-1 gap-4 ${selected ? "xl:grid-cols-[minmax(0,1fr)_22rem]" : ""}`}>
        <Card level="floating" className="min-w-0 p-0">
          <div className="border-b border-border p-4">
            <Toolbar>
              <SearchInput
                value={filters.search}
                onChange={(v) => { setFilters((f) => ({ ...f, search: v })); }}
                placeholder="Search name, PID, user, or command…"
              />
              <FilterSelect
                value={filters.state}
                onChange={(v) => { setFilters((f) => ({ ...f, state: v })); }}
                options={STATE_OPTIONS}
              />
              <FilterSelect
                value={filters.user}
                onChange={(v) => { setFilters((f) => ({ ...f, user: v })); }}
                options={[
                  { value: "all", label: "All users" },
                  ...users.map((u) => ({ value: u, label: u })),
                ]}
              />
              <FilterSelect
                value={String(filters.minCpu)}
                onChange={(v) => { setFilters((f) => ({ ...f, minCpu: Number(v) })); }}
                options={CPU_OPTIONS}
              />
              <FilterSelect
                value={String(filters.minMemoryMiB)}
                onChange={(v) => { setFilters((f) => ({ ...f, minMemoryMiB: Number(v) })); }}
                options={MEM_OPTIONS}
              />
            </Toolbar>
            <p className="mt-2 text-xs text-text-muted">
              {sorted.length.toLocaleString()} of {all.length.toLocaleString()} processes · click a
              row to inspect
            </p>
          </div>

          <QueryState
            isPending={processes.isPending}
            error={processes.error}
            isEmpty={sorted.length === 0}
            onRetry={() => void processes.refetch()}
            rows={6}
            empty={
              all.length === 0
                ? {
                    art: emptyArt.data,
                    title: "No processes reported",
                    description:
                      "The host returned an empty process list. On a running machine this is close to impossible, so treat it as a collection problem rather than an idle host.",
                    hint: "Processes are enumerated on every sweep, not sampled from storage.",
                  }
                : {
                    kind: "filtered",
                    art: emptyArt.search,
                    title: "No processes match these filters",
                    description: `All ${all.length.toLocaleString()} processes are still being collected — the current combination of search, state, user and resource thresholds excludes every one of them.`,
                    action: (
                      <EmptyAction onClick={() => { setFilters(DEFAULT_FILTERS); }}>
                        Clear filters
                      </EmptyAction>
                    ),
                  }
            }
          />

          {!processes.isPending && !processes.error && sorted.length > 0 ? (
            <div className="scroll-thin max-h-[38rem] overflow-auto">
              <table className="w-full border-collapse text-sm">
                {/* Sticky header: the list runs to hundreds of rows, and
                    scrolling the column names away makes a dense table
                    unreadable. */}
                <thead className="sticky top-0 z-10 bg-surface-hover">
                  <tr>
                    <SortableTh label="PID" col="pid" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Process" col="name" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="User" col="user" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="CPU" col="cpu" align="right" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Memory" col="memory" align="right" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Threads" col="threads" align="right" {...{ sortKey, direction, toggleSort }} />
                    <th className="px-3 py-2.5 text-left text-[11px] font-semibold tracking-wider text-text-muted uppercase">
                      Status
                    </th>
                    <SortableTh label="Runtime" col="runtime" align="right" {...{ sortKey, direction, toggleSort }} />
                    {/* The command line is the widest column and the first to
                        be clipped when the inspector takes its 22rem. It is
                        dropped rather than truncated then: the inspector's
                        Command tab shows it in full, so a half-visible copy
                        beside it is noise. */}
                    {selected ? null : (
                      <th className="px-3 py-2.5 text-left text-[11px] font-semibold tracking-wider text-text-muted uppercase">
                        Command
                      </th>
                    )}
                    <th className="px-3 py-2.5" />
                  </tr>
                </thead>
                <tbody>
                  {sorted.map((p) => (
                    <ProcessRow
                      key={p.pid}
                      process={p}
                      selected={p.pid === selectedPid}
                      showCommand={!selected}
                      onSelect={() => { setSelectedPid(p.pid === selectedPid ? null : p.pid); }}
                    />
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </Card>

        {selected ? (
          <ProcessInspector
            process={selected}
            cpuHistory={cpuHistory.data?.series ?? []}
            onClose={() => { setSelectedPid(null); }}
          />
        ) : null}
      </div>

      {/* Analytics support the explorer, so they sit beneath it. */}
      <h2 className="eyebrow mt-8 mb-3">Supporting analytics</h2>

      <div className="mb-4 grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Card level="flat">
          <CardHeader title="Top CPU consumers" action={<Cpu size={14} className="text-text-muted" />} />
          <TopList items={topCPU} />
        </Card>
        <Card level="flat">
          <CardHeader
            title="Top memory consumers"
            action={<MemoryStick size={14} className="text-text-muted" />}
          />
          <TopList items={topMemory} />
        </Card>
      </div>

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-[3fr_2fr]">
        <Card level="flat">
          <CardHeader
            title="CPU by process, last hour"
            action={<span className="text-xs text-text-muted">by name · darker is busier</span>}
          />
          <QueryState
            isPending={cpuHistory.isPending}
            error={cpuHistory.error}
            isEmpty={heatmap.length === 0}
            onRetry={() => void cpuHistory.refetch()}
            rows={4}
            empty={{
              art: emptyArt.reports,
              title: "No stored history yet",
              description: "Per-process CPU is written to storage on each sweep. The first hour of a fresh install has nothing to draw.",
              hint: "Only the top processes by CPU are stored by name — recording a series per PID would blow the cardinality budget within a day.",
            }}
          />
          {!cpuHistory.isPending && !cpuHistory.error && heatmap.length > 0 ? (
            <Heatmap rows={heatmap.slice(0, 8)} format={(v) => `${v.toFixed(1)}% CPU`} />
          ) : null}
        </Card>

        <Card level="flat">
          <CardHeader
            title="Active states"
            action={
              <span className="text-xs text-text-muted">
                {byState.sleeping.toLocaleString()} sleeping, excluded
              </span>
            }
          />
          <QueryState isPending={stateHistory.isPending} error={stateHistory.error} />
          {!stateHistory.isPending && !stateHistory.error ? (
            <StackedArea
              data={stateStack.rows}
              series={stateStack.keys.map((k) => ({
                key: k,
                label: k,
                color: STATE_COLOR[k as ProcessState],
              }))}
              height={170}
              format={(v) => v.toFixed(0)}
            />
          ) : null}
        </Card>
      </div>
    </>
  );
}

function SortableTh({
  label,
  col,
  sortKey,
  direction,
  toggleSort,
  align = "left",
}: {
  label: string;
  col: SortKey;
  sortKey: SortKey;
  direction: "asc" | "desc";
  toggleSort: (k: SortKey) => void;
  align?: "left" | "right";
}) {
  const active = sortKey === col;
  const Icon = !active ? ChevronsUpDown : direction === "asc" ? ArrowUp : ArrowDown;

  return (
    <th className={`px-3 py-2.5 ${align === "right" ? "text-right" : "text-left"}`}>
      <button
        type="button"
        onClick={() => { toggleSort(col); }}
        className={`inline-flex items-center gap-1 text-[11px] font-semibold tracking-wider uppercase transition-colors ${
          active ? "text-text" : "text-text-muted hover:text-text"
        } ${align === "right" ? "flex-row-reverse" : ""}`}
      >
        {label}
        <Icon size={11} className={active ? "text-primary" : "opacity-50"} />
      </button>
    </th>
  );
}

function ProcessRow({
  process: p,
  selected,
  showCommand,
  onSelect,
}: {
  process: Process;
  selected: boolean;
  showCommand: boolean;
  onSelect: () => void;
}) {
  return (
    <tr
      onClick={onSelect}
      className={`cursor-pointer border-b border-border/60 transition-colors last:border-0 ${
        selected ? "bg-primary/10" : "hover:bg-surface-hover"
      }`}
    >
      <td className="px-3 py-2 font-mono text-xs tabular-nums text-text-muted">{p.pid}</td>
      <td className="max-w-[15rem] px-3 py-2">
        <span className="block truncate font-medium text-text" title={p.name}>
          {p.name}
        </span>
      </td>
      <td className="px-3 py-2 text-xs text-text-muted">{p.username ?? "—"}</td>
      <td className="px-3 py-2">
        <UsageCell percent={p.cpu_percent} label={`${p.cpu_percent.toFixed(1)}%`} />
      </td>
      <td className="px-3 py-2">
        <UsageCell percent={p.memory_percent} label={formatBytes(p.memory_rss)} />
      </td>
      <td className="px-3 py-2 text-right text-xs tabular-nums text-text-muted">{p.threads}</td>
      <td className="px-3 py-2">
        <Badge tone={STATE_TONE[p.state]}>{p.state}</Badge>
      </td>
      <td className="px-3 py-2 text-right text-xs tabular-nums text-text-muted">
        {p.running_for_seconds ? formatDuration(p.running_for_seconds) : "—"}
      </td>
      {showCommand ? (
        <td className="max-w-[18rem] px-3 py-2">
          <code className="block truncate font-mono text-[11px] text-text-muted" title={p.cmdline}>
            {p.cmdline ?? "—"}
          </code>
        </td>
      ) : null}
      <td className="px-3 py-2" onClick={(e) => { e.stopPropagation(); }}>
        <CopyButton value={String(p.pid)} label="PID" />
      </td>
    </tr>
  );
}

/** A number with its share drawn underneath. The bar makes a column of
 *  percentages scannable without reading any of them. */
function UsageCell({ percent, label }: { percent: number; label: string }) {
  const tone = percent >= 50 ? "bg-danger" : percent >= 20 ? "bg-warning" : "bg-primary";
  return (
    <div className="flex flex-col items-end gap-1">
      <span className="text-xs tabular-nums text-text">{label}</span>
      <span className="h-1 w-14 overflow-hidden rounded-full bg-surface-hover">
        <span
          className={`block h-full rounded-full ${tone}`}
          style={{ width: `${Math.min(Math.max(percent, 0), 100)}%` }}
        />
      </span>
    </div>
  );
}

function toHeatmap(series: Series[]): HeatmapRow[] {
  return [...series]
    .map((s) => ({
      key: s.labels?.process ?? s.metric,
      label: s.labels?.process ?? s.metric,
      values: s.points.map((p) => p.value),
      peak: s.points.reduce((m, p) => Math.max(m, p.value), 0),
    }))
    .sort((a, b) => b.peak - a.peak)
    .map(({ key, label, values }) => ({ key, label, values }));
}

function toStack(
  series: Series[],
  labelKey: string,
  exclude: string[] = [],
): { rows: Record<string, string | number>[]; keys: string[] } {
  const byTime = new Map<string, Record<string, string | number>>();
  const keys: string[] = [];

  for (const s of series) {
    const key = s.labels?.[labelKey] ?? s.metric;
    if (exclude.includes(key)) continue;
    if (!keys.includes(key)) keys.push(key);
    for (const p of s.points) {
      const row = byTime.get(p.time) ?? { time: p.time };
      row[key] = p.value;
      byTime.set(p.time, row);
    }
  }

  const rows = [...byTime.values()].sort((a, b) => String(a.time).localeCompare(String(b.time)));
  for (const row of rows) {
    for (const k of keys) row[k] ??= 0;
  }

  const totalFor = (k: string) => rows.reduce((sum, r) => sum + Number(r[k] ?? 0), 0);
  keys.sort((a, b) => totalFor(b) - totalFor(a));
  return { rows, keys };
}
