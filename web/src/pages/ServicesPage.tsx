import { useMemo, useState } from "react";
import { ArrowDown, ArrowUp, ChevronsUpDown } from "lucide-react";
import { usePrimaryNodeID, useServiceGraph, useServices } from "../api/queries";
import { ApiError, inventoryGapKind } from "../api/client";
import { AgentSubjectGap } from "../components/AgentSubjectGap";
import type { GraphHealth, Service } from "../api/types";
import { emptyArray } from "../api/empty";
import { Card, CardHeader } from "../components/Card";
import { EmptyState, EmptyAction } from "../components/EmptyState";
import { Badge } from "../components/Badge";
import { PageHeader } from "../components/PageHeader";
import { QueryState } from "../components/QueryState";
import { SearchInput, FilterSelect, Toolbar } from "../components/Toolbar";
import { emptyArt } from "../lib/assets";
import { formatBytes, formatDuration } from "../format";
import { DependencyGraph } from "./services/DependencyGraph";
import { ServiceInspector } from "./services/ServiceInspector";
import { HEALTH_EXPLANATION, HEALTH_LABEL, HEALTH_TONE } from "./services/serviceLabels";
import {
  DEFAULT_FILTERS, joinServices, useServiceTable, type ServiceRow, type SortKey,
} from "./services/useServiceTable";

const HEALTH_OPTIONS: { value: "all" | GraphHealth; label: string }[] = [
  { value: "all", label: "All health" },
  { value: "failed", label: "Failed" },
  { value: "degraded", label: "Degraded" },
  { value: "healthy", label: "Healthy" },
  { value: "inactive", label: "Inactive" },
  { value: "unknown", label: "Unknown" },
];

const ENABLED_OPTIONS = [
  { value: "all", label: "Boot: any" },
  { value: "enabled", label: "Enabled at boot" },
  { value: "disabled", label: "Not enabled at boot" },
] as const;

const DEPTH_OPTIONS = [
  { value: "1", label: "Depth 1" },
  { value: "2", label: "Depth 2" },
  { value: "3", label: "Depth 3" },
] as const;

const CLASS_OPTIONS = [
  { value: "requirement", label: "Requirements only" },
  { value: "all", label: "All relationships" },
] as const;

const DIRECTION_OPTIONS = [
  { value: "dependencies", label: "Depends on" },
  { value: "dependents", label: "Needed by" },
] as const;

/**
 * Service health and dependencies.
 *
 * The page is built around one fact the rest of Atlas cannot show: a unit's
 * health is not only its own state. A service can be active while the database
 * it requires has failed, and no per-unit view reveals that. The backend
 * computes the propagation; this page leads with it.
 *
 * The dependency graph is rooted and depth-limited rather than showing
 * everything. That is a decision the data forced: the full graph on a plain
 * Debian host is 202 nodes and 1,165 edges with a single 236-degree hub, and
 * drawn whole it is an unreadable hairball. Rooted at one unit, bounded to two
 * hops and filtered to requirement edges it is 4–35 nodes, which is legible
 * and answers a real question.
 *
 * Requirement edges are the default because 726 of those 1,165 edges are
 * ordering. Showing them by default would drown the 294 that mean something.
 */
export function ServicesPage() {
  const nodeID = usePrimaryNodeID();
  const services = useServices(nodeID);
  const [selected, setSelected] = useState<string | null>(null);
  const [depth, setDepth] = useState<"1" | "2" | "3">("1");
  const [edgeClass, setEdgeClass] = useState<"requirement" | "all">("requirement");
  const [direction, setDirection] = useState<"dependencies" | "dependents">("dependencies");

  // The unrooted graph supplies propagated health for the whole list. Its node
  // cap is generous because it is data, not a drawing — nothing renders 202
  // nodes at once.
  const fleetGraph = useServiceGraph({ node: nodeID, limit: 1000 });
  // The rooted graph is what gets drawn.
  const rootedGraph = useServiceGraph({
    node: nodeID,
    root: selected ?? undefined,
    depth: Number(depth),
    direction,
    edgeClass,
    limit: 200,
    enabled: Boolean(selected),
  });

  const units = services.data?.services ?? emptyArray<Service>();
  const rows = useMemo(
    () => joinServices(units, fleetGraph.data?.nodes),
    [units, fleetGraph.data],
  );

  const { filters, setFilters, states, sorted, sortKey, direction: sortDir, toggleSort } =
    useServiceTable(rows);

  const counts = useMemo(() => {
    const out: Record<GraphHealth, number> = {
      healthy: 0, degraded: 0, failed: 0, inactive: 0, unknown: 0,
    };
    for (const r of rows) out[r.health]++;
    return out;
  }, [rows]);

  const selectedRow = useMemo(
    () => rows.find((r) => r.unit.name === selected) ?? null,
    [rows, selected],
  );

  const notEnabled = rows.filter((r) => r.unit.active_state === "active" && !r.unit.enabled).length;

  if (inventoryGapKind(services.error) === "agent") {
    return <AgentSubjectGap subject="services" />;
  }

  if (services.error instanceof ApiError && services.error.code === "not_implemented") {
    return (
      <>
        <PageHeader subtitle="No service manager on this host." />
        <Card>
          <EmptyState
            kind="unavailable"
            art={emptyArt.services}
            title="No service manager on this host"
            description="systemd was not found. Atlas reports the absence rather than an empty list, because “no services” and “cannot see services” are different answers."
            hint="This page populates on hosts running systemd. macOS (launchd) and minimal containers have no unit table to read."
          />
        </Card>
      </>
    );
  }

  if (services.isPending || services.error || units.length === 0) {
    return (
      <>
        <PageHeader
          stats={["Units", "Failed", "Degraded", "Active, not enabled"].map((label) => ({
            label,
            value: "—",
            hint: services.error ? "unavailable" : services.isPending ? "loading" : "none",
          }))}
        />
        <Card level="floating" className="p-0">
          <QueryState
            isPending={services.isPending}
            error={services.error}
            isEmpty={units.length === 0}
            onRetry={() => void services.refetch()}
            rows={5}
            empty={{
              art: emptyArt.services,
              title: "No units reported",
              description:
                "systemd answered, but listed no units. That is unusual on a booted system and worth checking before reading it as good news.",
              hint: "Atlas reads unit state over systemctl and never starts, stops or restarts anything.",
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
            label: "Units",
            value: String(rows.length),
            hint: `${counts.healthy} healthy · ${counts.inactive} inactive`,
          },
          {
            label: "Failed",
            value: String(counts.failed),
            hint: counts.failed > 0 ? "systemd has given up" : "none failing",
            tone: counts.failed > 0 ? "danger" : "success",
          },
          {
            label: "Degraded",
            value: String(counts.degraded),
            hint:
              counts.degraded > 0
                ? "running, but a requirement has failed"
                : "no failed requirements",
            tone: counts.degraded > 0 ? "warning" : "success",
          },
          {
            // Deliberately narrower than the explorer's "not enabled" filter,
            // which counts every disabled unit including the 48 that are not
            // running. The number worth a hero slot is the one that is a
            // latent outage: running now, gone after the next reboot.
            label: "Active, not enabled",
            value: String(notEnabled),
            hint: notEnabled > 0 ? "running now, gone after reboot" : "all active units enabled",
            tone: notEnabled > 0 ? "warning" : "success",
          },
        ]}
      />

      {/* The graph is data as well as a drawing: without it, health is only
          each unit's own state and no degraded verdict is possible. Saying so
          matters more than hiding the failure.
          
          The condition is `isFetched && !data`, and both halves are load-bearing.
          `error` alone never settles: a query with a refetch interval starts a
          new retry round before the last one exhausts, so the page would
          silently report every unit as healthy-or-inactive with no sign that
          propagation was missing. `isPending` alone flickers for the same
          reason — measured, the banner blinked on a ten-second cycle, which is
          worse than not warning at all. `isFetched` latches once the first
          fetch completes and never goes back. */}
      {fleetGraph.isFetched && !fleetGraph.data ? (
        <div className="surface-warn mb-4 rounded-lg p-3 text-xs leading-relaxed">
          The dependency graph could not be read, so health below is each unit's own state only —
          no unit can be reported as degraded without knowing what it requires.
        </div>
      ) : null}

      <h2 className="eyebrow mb-3">Health</h2>
      <HealthSummary counts={counts} total={rows.length} />

      <h2 className="eyebrow mb-3">Dependency graph</h2>
      <Card level="flat" className="mb-6">
        <CardHeader
          title={selected ? `${direction === "dependents" ? "What needs" : "What"} ${selected} ${direction === "dependents" ? "" : "depends on"}`.trim() : "Dependency graph"}
          action={
            selected ? (
              <div className="flex flex-wrap gap-2">
                <FilterSelect
                  label="Graph direction"
                  value={direction}
                  onChange={setDirection}
                  options={[...DIRECTION_OPTIONS]}
                />
                <FilterSelect
                  label="Graph depth"
                  value={depth}
                  onChange={setDepth}
                  options={[...DEPTH_OPTIONS]}
                />
                <FilterSelect
                  label="Relationship class"
                  value={edgeClass}
                  onChange={setEdgeClass}
                  options={[...CLASS_OPTIONS]}
                />
              </div>
            ) : null
          }
        />

        {!selected ? (
          <EmptyState
            art={emptyArt.data}
            title="Select a unit to see its dependencies"
            description="The graph is rooted on one unit rather than showing everything at once. The full structure on this host runs to hundreds of units with a single node connected to over two hundred others, which draws as an unreadable tangle."
            hint="Pick a row in the explorer below. Requirement edges are shown by default; ordering edges outnumber them roughly three to one and carry no failure."
            compact
          />
        ) : rootedGraph.isPending || rootedGraph.error ? (
          <QueryState
            isPending={rootedGraph.isPending}
            error={rootedGraph.error}
            onRetry={() => void rootedGraph.refetch()}
            rows={4}
          />
        ) : (
          // Narrowed by the branches above: a settled, error-free query has
          // data.
          <>
            <DependencyGraph
              graph={rootedGraph.data}
              root={selected}
              direction={direction}
              selected={selected}
              onSelect={setSelected}
            />
            <GraphLegend
              nodes={rootedGraph.data.nodes.length}
              edges={rootedGraph.data.edges.length}
              truncated={rootedGraph.data.truncated}
              edgeClass={edgeClass}
            />
          </>
        )}
      </Card>

      <h2 className="eyebrow mb-3">Service explorer</h2>

      <div className={`grid grid-cols-1 gap-4 ${selectedRow ? "xl:grid-cols-[minmax(0,1fr)_22rem]" : ""}`}>
        <Card level="floating" className="min-w-0 p-0">
          <div className="border-b border-border p-4">
            <Toolbar>
              <SearchInput
                value={filters.search}
                onChange={(v) => { setFilters((f) => ({ ...f, search: v })); }}
                placeholder="Search unit, description, or failed dependency…"
              />
              <FilterSelect
                label="Filter by health"
                value={filters.health}
                onChange={(v) => { setFilters((f) => ({ ...f, health: v })); }}
                options={HEALTH_OPTIONS}
              />
              <FilterSelect
                label="Filter by state"
                value={filters.state}
                onChange={(v) => { setFilters((f) => ({ ...f, state: v })); }}
                options={[
                  { value: "all", label: "All states" },
                  ...states.map((s) => ({ value: s, label: s })),
                ]}
              />
              <FilterSelect
                label="Filter by boot enablement"
                value={filters.enabled}
                onChange={(v) => { setFilters((f) => ({ ...f, enabled: v })); }}
                options={[...ENABLED_OPTIONS]}
              />
            </Toolbar>
            <p className="mt-2 text-xs text-text-muted">
              {sorted.length} of {rows.length} unit{rows.length === 1 ? "" : "s"} · click a row to
              inspect and root the graph
            </p>
          </div>

          <QueryState
            isPending={false}
            isEmpty={sorted.length === 0}
            rows={4}
            empty={{
              kind: "filtered",
              art: emptyArt.search,
              title: "No units match these filters",
              description: `${rows.length.toLocaleString()} units are loaded; the current combination of search, health, state and boot filters excludes every one.`,
              action: (
                <EmptyAction onClick={() => { setFilters(DEFAULT_FILTERS); }}>
                  Clear filters
                </EmptyAction>
              ),
            }}
          />

          {sorted.length > 0 ? (
            <div className="scroll-thin max-h-[34rem] overflow-auto">
              <table aria-label="Service explorer" className="w-full border-collapse text-sm">
                <thead className="sticky top-0 z-10 bg-surface-hover">
                  <tr>
                    <SortableTh label="Unit" col="name" {...{ sortKey, direction: sortDir, toggleSort }} />
                    <SortableTh label="Health" col="health" {...{ sortKey, direction: sortDir, toggleSort }} />
                    <SortableTh label="State" col="state" {...{ sortKey, direction: sortDir, toggleSort }} />
                    <SortableTh label="Boot" col="enabled" {...{ sortKey, direction: sortDir, toggleSort }} />
                    <SortableTh label="Restarts" col="restarts" align="right" {...{ sortKey, direction: sortDir, toggleSort }} />
                    <SortableTh label="Uptime" col="uptime" align="right" {...{ sortKey, direction: sortDir, toggleSort }} />
                    {selectedRow ? null : (
                      <SortableTh label="Memory" col="memory" align="right" {...{ sortKey, direction: sortDir, toggleSort }} />
                    )}
                  </tr>
                </thead>
                <tbody>
                  {sorted.map((r) => (
                    <ServiceTableRow
                      key={r.unit.name}
                      row={r}
                      selected={r.unit.name === selected}
                      showMemory={!selectedRow}
                      onSelect={() => { setSelected(r.unit.name === selected ? null : r.unit.name); }}
                    />
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </Card>

        {selectedRow ? (
          <ServiceInspector
            row={selectedRow}
            nodeID={nodeID}
            onClose={() => { setSelected(null); }}
            onSelectUnit={setSelected}
          />
        ) : null}
      </div>
    </>
  );
}

function HealthSummary({
  counts,
  total,
}: {
  counts: Record<GraphHealth, number>;
  total: number;
}) {
  const order: GraphHealth[] = ["failed", "degraded", "healthy", "inactive", "unknown"];
  const present = order.filter((h) => counts[h] > 0);

  const BAR: Record<GraphHealth, string> = {
    failed: "bg-danger",
    degraded: "bg-warning",
    healthy: "bg-success",
    inactive: "bg-text-subtle",
    unknown: "bg-border",
  };

  return (
    <Card level="flat" className="mb-6">
      <div className="mb-3 flex h-2 overflow-hidden rounded-full bg-surface-hover">
        {present.map((h) => (
          <span
            key={h}
            className={BAR[h]}
            style={{ width: `${String((counts[h] / total) * 100)}%` }}
            title={`${String(counts[h])} ${h}`}
          />
        ))}
      </div>

      <ul className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-5">
        {order.map((h) => (
          <li key={h} className="flex flex-col gap-0.5" title={HEALTH_EXPLANATION[h]}>
            <span className="flex items-center gap-1.5">
              <span aria-hidden="true" className={`h-2 w-2 shrink-0 rounded-sm ${BAR[h]}`} />
              <span className="text-lg font-semibold tabular-nums text-text">{counts[h]}</span>
            </span>
            <span className="text-xs capitalize text-text-muted">{HEALTH_LABEL[h]}</span>
          </li>
        ))}
      </ul>

      <p className="mt-3 border-t border-border pt-2.5 text-[11px] leading-relaxed text-text-subtle">
        Degraded means the unit is running but something it <em>requires</em> has failed. Units
        merely ordered after a failure are not degraded — ordering carries no requirement.
      </p>
    </Card>
  );
}

function GraphLegend({
  nodes,
  edges,
  truncated,
  edgeClass,
}: {
  nodes: number;
  edges: number;
  truncated: boolean;
  edgeClass: "requirement" | "all";
}) {
  return (
    <div className="mt-3 flex flex-wrap items-center gap-x-5 gap-y-2 border-t border-border pt-3 text-[11px] text-text-subtle">
      <span className="tabular-nums">
        {nodes} unit{nodes === 1 ? "" : "s"} · {edges} edge{edges === 1 ? "" : "s"}
      </span>

      <span className="flex items-center gap-1.5">
        <svg width="26" height="8" aria-hidden="true">
          <line x1="0" y1="4" x2="26" y2="4" stroke="var(--text-subtle)" strokeWidth="1.5" />
        </svg>
        requirement — failure travels along it
      </span>

      {edgeClass === "all" ? (
        <span className="flex items-center gap-1.5">
          <svg width="26" height="8" aria-hidden="true">
            <line
              x1="0"
              y1="4"
              x2="26"
              y2="4"
              stroke="var(--border)"
              strokeWidth="1"
              strokeDasharray="3 3"
            />
          </svg>
          ordering — nothing propagates
        </span>
      ) : null}

      {truncated ? (
        <span className="text-warning">
          Node cap reached — this view is partial rather than complete.
        </span>
      ) : null}
    </div>
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

function ServiceTableRow({
  row,
  selected,
  showMemory,
  onSelect,
}: {
  row: ServiceRow;
  selected: boolean;
  showMemory: boolean;
  onSelect: () => void;
}) {
  const u = row.unit;

  return (
    <tr
      onClick={onSelect}
      className={`cursor-pointer border-b border-border/60 transition-colors last:border-0 ${
        selected ? "bg-primary/10" : "hover:bg-surface-hover"
      }`}
    >
      <td className="max-w-[18rem] px-3 py-2">
        <span className="block truncate font-medium text-text" title={u.name}>
          {u.name}
        </span>
        {u.description ? (
          <span className="block truncate text-[11px] text-text-subtle" title={u.description}>
            {u.description}
          </span>
        ) : null}
      </td>

      <td className="px-3 py-2">
        <Badge tone={HEALTH_TONE[row.health]} pulse={row.health === "failed"}>
          {HEALTH_LABEL[row.health]}
        </Badge>
        {/* A degraded verdict is unhelpful without its cause. */}
        {row.failedDependencies.length > 0 && row.health === "degraded" ? (
          <span
            className="mt-0.5 block truncate text-[11px] text-warning"
            title={row.failedDependencies.join(", ")}
          >
            needs {row.failedDependencies[0]}
            {row.failedDependencies.length > 1 ? ` +${String(row.failedDependencies.length - 1)}` : ""}
          </span>
        ) : null}
      </td>

      <td className="px-3 py-2 text-xs text-text-muted">
        {u.active_state}
        <span className="block text-[11px] text-text-subtle">{u.sub_state}</span>
      </td>

      <td className="px-3 py-2 text-xs">
        {u.enabled ? (
          <span className="text-text-muted">enabled</span>
        ) : (
          <span className="text-warning" title="Active now, but will not start on the next boot">
            no
          </span>
        )}
      </td>

      <td className="px-3 py-2 text-right text-xs tabular-nums">
        <span className={u.restart_count > 0 ? "text-warning" : "text-text-muted"}>
          {u.restart_count}
        </span>
      </td>

      <td className="px-3 py-2 text-right text-xs tabular-nums text-text-muted">
        {u.uptime_seconds ? formatDuration(u.uptime_seconds) : "—"}
      </td>

      {showMemory ? (
        <td className="px-3 py-2 text-right text-xs tabular-nums text-text-muted">
          {u.memory_bytes ? formatBytes(u.memory_bytes) : "—"}
        </td>
      ) : null}
    </tr>
  );
}
