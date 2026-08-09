import { useMemo, useState } from "react";
import { ArrowDown, ArrowUp, ChevronsUpDown } from "lucide-react";
import { useCollectors, useFleetLatestMetrics, useNodes } from "../api/queries";
import type { Node, NodeStatus } from "../api/types";
import { emptyArray } from "../api/empty";
import { Card } from "../components/Card";
import { Badge, type Tone } from "../components/Badge";
import { PageHeader } from "../components/PageHeader";
import { QueryState } from "../components/QueryState";
import { EmptyAction } from "../components/EmptyState";
import { emptyArt } from "../lib/assets";
import { SearchInput, FilterSelect, Toolbar } from "../components/Toolbar";
import { formatAgo, formatDuration, formatValue } from "../format";
import { FleetDistribution } from "./nodes/FleetDistribution";
import { EnvironmentComparison } from "./nodes/EnvironmentComparison";
import { NodeComparison } from "./nodes/NodeComparison";
import { NodeInspector } from "./nodes/NodeInspector";
import { readVitals } from "./nodes/nodeMetrics";
import { DEFAULT_FILTERS, UNTAGGED, countByStatus, useNodeTable, type SortKey } from "./nodes/useNodeTable";

const STATUS_TONE: Record<NodeStatus, Tone> = { up: "success", stale: "warning", down: "danger" };

/** The hero's four stat labels, shared so the unresolved header keeps the same
 *  shape as the loaded one and the layout does not jump when data lands. */
const FLEET_STAT_LABELS = ["Fleet", "Reporting", "Not reporting", "Environments"] as const;

const STATUS_OPTIONS: { value: "all" | NodeStatus; label: string }[] = [
  { value: "all", label: "All status" },
  { value: "up", label: "Reporting" },
  { value: "stale", label: "Stale" },
  { value: "down", label: "Not reporting" },
];

/**
 * The fleet.
 *
 * Structured as summary → comparison → explorer → inspector, which is the
 * order the questions actually arrive in: what is the estate made of, how do
 * its machines compare, which one do I want, and then everything about that
 * one.
 *
 * The page runs on two data sources with different availability, and keeping
 * them distinct is the main correctness concern here. The *node record* is
 * inventory — cores, kernel, architecture, last contact — which survives a
 * node going away and is shown for every node. *Metrics* are live vitals,
 * resolved over a five-minute window, which a node that stopped reporting
 * simply does not have. Anything derived from metrics is either omitted or
 * labelled "not reporting"; none of it is ever rendered as zero.
 */
export function NodesPage() {
  const nodes = useNodes();
  const collectors = useCollectors();
  const [selectedID, setSelectedID] = useState<string | null>(null);

  const all = nodes.data?.nodes ?? emptyArray<Node>();
  const { filters, setFilters, environments, platforms, sorted, sortKey, direction, toggleSort } =
    useNodeTable(all);

  // One request per node. See useFleetLatestMetrics for why there is no bulk
  // fetch and what the scaling limit is.
  const nodeIDs = useMemo(() => all.map((n) => n.node_id), [all]);
  const fleet = useFleetLatestMetrics(nodeIDs);

  const counts = useMemo(() => countByStatus(all), [all]);
  const selected = useMemo(
    () => all.find((n) => n.node_id === selectedID) ?? null,
    [all, selectedID],
  );

  const reporting = useMemo(
    () => all.filter((n) => readVitals(fleet.byNode.get(n.node_id) ?? []).reporting).length,
    [all, fleet.byNode],
  );

  const untagged = all.filter((n) => !n.environment).length;
  // The first node in the list is the host Atlas runs on; only its collector
  // health is Atlas's to report.
  const primaryID = all[0]?.node_id;

  // Nothing below this point may render before the node list resolves.
  //
  // When it was allowed to, a failed query rendered the whole page against an
  // empty fleet: "0 tiers", "0 builds", "Nothing to show", a comparison
  // reporting that no node was reporting, and a hero showing a green "none"
  // under Not reporting and "all tagged" under Environments. A broken query
  // read as a healthy, empty estate — the one failure this product exists to
  // prevent. Every summary panel is a derived view of `all`, so without `all`
  // the only honest thing to render is why it is missing.
  if (nodes.isPending || nodes.error || all.length === 0) {
    return (
      <>
        <PageHeader
          stats={FLEET_STAT_LABELS.map((label) => ({
            label,
            value: "\u2014",
            hint: nodes.error ? "unavailable" : nodes.isPending ? "loading" : "no nodes",
          }))}
        />
        <Card level="floating" className="p-0">
          <QueryState
            isPending={nodes.isPending}
            error={nodes.error}
            isEmpty={all.length === 0}
            onRetry={() => void nodes.refetch()}
            rows={5}
            empty={{
              art: emptyArt.servers,
              title: "No servers are reporting yet",
              description:
                "A node registers itself on its first successful sweep. Nothing has completed one, so there is no fleet to show yet.",
              hint: "Collection begins within one interval of agent startup — allow a minute before treating this as a fault.",
            }}
          />
        </Card>
      </>
    );
  }

  return (
    <>
      <PageHeader
        stats={[
          {
            label: "Fleet",
            value: String(all.length),
            hint: `${reporting} of ${all.length} sending metrics`,
          },
          {
            label: "Reporting",
            value: `${counts.up}/${all.length}`,
            hint: counts.stale > 0 ? `${counts.stale} stale` : "seen recently",
            tone: counts.up === all.length ? "success" : "warning",
          },
          {
            label: "Not reporting",
            value: String(counts.down),
            hint: counts.down > 0 ? "silent past the threshold" : "none",
            tone: counts.down > 0 ? "danger" : "success",
          },
          {
            label: "Environments",
            value: String(environments.length),
            hint: untagged > 0 ? `${untagged} untagged` : "all tagged",
            tone: untagged > 0 ? "warning" : "success",
          },
        ]}
      />

      <h2 className="eyebrow mb-3">Fleet composition</h2>
      <FleetDistribution nodes={all} />

      <EnvironmentComparison nodes={all} metricsByNode={fleet.byNode} />

      <h2 className="eyebrow mb-3">Live comparison</h2>
      <NodeComparison nodes={all} metricsByNode={fleet.byNode} />

      <h2 className="eyebrow mb-3">Node explorer</h2>

      {/* Table left, inspector docked right. Collapses to one column below xl,
          where a docked panel would leave the table too narrow to read. */}
      <div className={`grid grid-cols-1 gap-4 ${selected ? "xl:grid-cols-[minmax(0,1fr)_22rem]" : ""}`}>
        <Card level="floating" className="min-w-0 p-0">
          <div className="border-b border-border p-4">
            <Toolbar>
              <SearchInput
                value={filters.search}
                onChange={(v) => { setFilters((f) => ({ ...f, search: v })); }}
                placeholder="Search hostname, node ID, platform, or kernel…"
              />
              <FilterSelect
                value={filters.status}
                onChange={(v) => { setFilters((f) => ({ ...f, status: v })); }}
                options={STATUS_OPTIONS}
              />
              <FilterSelect
                value={filters.environment}
                onChange={(v) => { setFilters((f) => ({ ...f, environment: v })); }}
                options={[
                  { value: "all", label: "All environments" },
                  ...environments.map((e) => ({ value: e, label: e })),
                ]}
              />
              <FilterSelect
                value={filters.platform}
                onChange={(v) => { setFilters((f) => ({ ...f, platform: v })); }}
                options={[
                  { value: "all", label: "All platforms" },
                  ...platforms.map((p) => ({ value: p, label: p })),
                ]}
              />
            </Toolbar>
            <p className="mt-2 text-xs text-text-muted">
              {sorted.length} of {all.length} node{all.length === 1 ? "" : "s"} · click a row to
              inspect
            </p>
          </div>

          <QueryState
            isPending={false}
            isEmpty={sorted.length === 0}
            rows={4}
            // Only the filtered case can reach here: an unresolved or empty
            // fleet returns above, before any of this renders.
            empty={{
              kind: "filtered",
              art: emptyArt.search,
              title: "No nodes match these filters",
              description: `${all.length.toLocaleString()} node${all.length === 1 ? " is" : "s are"} registered; the current combination of search, status, environment and platform excludes every one.`,
              action: (
                <EmptyAction onClick={() => { setFilters(DEFAULT_FILTERS); }}>
                  Clear filters
                </EmptyAction>
              ),
            }}
          />

          {sorted.length > 0 ? (
            <div className="scroll-thin max-h-[34rem] overflow-auto">
              {/* Named: this page renders two tables, and an unlabelled one is
                  announced only as "table" by a screen reader. */}
              <table aria-label="Node explorer" className="w-full border-collapse text-sm">
                <thead className="sticky top-0 z-10 bg-surface-hover">
                  <tr>
                    <SortableTh label="Node" col="hostname" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Status" col="status" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Environment" col="environment" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Platform" col="platform" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Cores" col="cores" align="right" {...{ sortKey, direction, toggleSort }} />
                    {selected ? null : (
                      <th className="px-3 py-2.5 text-left text-[11px] font-semibold tracking-wider text-text-muted uppercase">
                        CPU
                      </th>
                    )}
                    {selected ? null : (
                      <th className="px-3 py-2.5 text-left text-[11px] font-semibold tracking-wider text-text-muted uppercase">
                        Memory
                      </th>
                    )}
                    <SortableTh label="Uptime" col="uptime" align="right" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Last seen" col="lastSeen" align="right" {...{ sortKey, direction, toggleSort }} />
                  </tr>
                </thead>
                <tbody>
                  {sorted.map((n) => (
                    <NodeRow
                      key={n.node_id}
                      node={n}
                      values={fleet.byNode.get(n.node_id) ?? emptyArray()}
                      selected={n.node_id === selectedID}
                      showVitals={!selected}
                      onSelect={() => { setSelectedID(n.node_id === selectedID ? null : n.node_id); }}
                    />
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </Card>

        {selected ? (
          <NodeInspector
            node={selected}
            values={fleet.byNode.get(selected.node_id) ?? emptyArray()}
            collectors={collectors.data}
            isPrimary={selected.node_id === primaryID}
            onClose={() => { setSelectedID(null); }}
          />
        ) : null}
      </div>
    </>
  );
}

function SortableTh({
  label,
  col,
  align = "left",
  sortKey,
  direction,
  toggleSort,
}: {
  label: string;
  col: SortKey;
  align?: "left" | "right";
  sortKey: SortKey;
  direction: "asc" | "desc";
  toggleSort: (k: SortKey) => void;
}) {
  const active = sortKey === col;
  const Icon = !active ? ChevronsUpDown : direction === "asc" ? ArrowUp : ArrowDown;

  return (
    <th
      aria-sort={active ? (direction === "asc" ? "ascending" : "descending") : "none"}
      className="px-3 py-2.5 text-[11px] font-semibold tracking-wider text-text-muted uppercase"
    >
      <button
        type="button"
        onClick={() => { toggleSort(col); }}
        className={`flex w-full items-center gap-1 hover:text-text ${align === "right" ? "justify-end" : ""}`}
      >
        {align === "right" ? null : <span>{label}</span>}
        <Icon size={11} className={active ? "text-primary" : "opacity-50"} />
        {align === "right" ? <span>{label}</span> : null}
      </button>
    </th>
  );
}

function NodeRow({
  node: n,
  values,
  selected,
  showVitals,
  onSelect,
}: {
  node: Node;
  values: Parameters<typeof readVitals>[0];
  selected: boolean;
  showVitals: boolean;
  onSelect: () => void;
}) {
  const v = readVitals(values);

  return (
    <tr
      onClick={onSelect}
      className={`cursor-pointer border-b border-border/60 transition-colors last:border-0 ${
        selected ? "bg-primary/10" : "hover:bg-surface-hover"
      }`}
    >
      <td className="max-w-[14rem] px-3 py-2">
        <span className="block truncate font-medium text-text" title={n.hostname}>
          {n.hostname}
        </span>
        <span className="block truncate font-mono text-[11px] text-text-subtle" title={n.node_id}>
          {n.node_id.slice(0, 12)}
        </span>
      </td>
      <td className="px-3 py-2">
        <Badge tone={STATUS_TONE[n.status]} pulse={n.status === "down"}>
          {n.status === "up" ? "reporting" : n.status === "down" ? "not reporting" : "stale"}
        </Badge>
      </td>
      <td className="px-3 py-2 text-xs">
        {n.environment ? (
          <span className="rounded bg-primary/10 px-1.5 py-0.5 text-primary">{n.environment}</span>
        ) : (
          <span className="text-text-subtle">{UNTAGGED}</span>
        )}
      </td>
      <td className="px-3 py-2 text-xs text-text-muted">{n.platform ?? "—"}</td>
      <td className="px-3 py-2 text-right text-xs tabular-nums text-text-muted">
        {n.cpu_cores ?? "—"}
      </td>
      {showVitals ? <VitalCell value={v.cpu} /> : null}
      {showVitals ? <VitalCell value={v.memoryPercent} /> : null}
      <td className="px-3 py-2 text-right text-xs tabular-nums text-text-muted">
        {n.uptime_seconds ? formatDuration(n.uptime_seconds) : "—"}
      </td>
      <td className="px-3 py-2 text-right text-xs tabular-nums text-text-muted">
        {formatAgo(n.seconds_since_seen)}
      </td>
    </tr>
  );
}

/** A live percentage, or an explicit dash when the node is not reporting.
 *  Never a zero — a zeroed bar reads as an idle machine rather than an
 *  unreachable one. */
function VitalCell({ value }: { value: number | undefined }) {
  if (value === undefined) {
    return <td className="px-3 py-2 text-xs text-text-subtle">—</td>;
  }

  const bar = value >= 90 ? "bg-danger" : value >= 75 ? "bg-warning" : "bg-primary";
  const text = value >= 90 ? "text-danger" : value >= 75 ? "text-warning" : "text-text";

  return (
    <td className="px-3 py-2">
      <span className={`block text-xs font-medium tabular-nums ${text}`}>
        {formatValue(value, "percent")}
      </span>
      <span className="mt-0.5 block h-0.5 w-full max-w-16 overflow-hidden rounded-full bg-surface-hover">
        <span
          className={`block h-full rounded-full ${bar}`}
          style={{ width: `${String(Math.min(Math.max(value, 1), 100))}%` }}
        />
      </span>
    </td>
  );
}
