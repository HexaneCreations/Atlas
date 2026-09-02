import { useMemo, useState } from "react";
import { ArrowDown, ArrowUp, ChevronsUpDown, FileText } from "lucide-react";
import { Link } from "react-router";
import { useContainers, useLatestMetrics, usePrimaryNodeID } from "../api/queries";
import { ApiError, inventoryGapKind } from "../api/client";
import { AgentSubjectGap } from "../components/AgentSubjectGap";
import type { Container, ContainerState } from "../api/types";
import { emptyArray } from "../api/empty";
import { Card } from "../components/Card";
import { EmptyState, EmptyAction } from "../components/EmptyState";
import { Badge, type Tone } from "../components/Badge";
import { PageHeader } from "../components/PageHeader";
import { QueryState } from "../components/QueryState";
import { SearchInput, FilterSelect, Toolbar } from "../components/Toolbar";
import { emptyArt } from "../lib/assets";
import { formatBytes, formatDuration, formatValue } from "../format";
import {
  STANDALONE, countByState, effectiveHealth, readUsage, readExit,
  summariseImages, summariseProjects, type EffectiveHealth,
} from "./containers/containerModel";
import { ContainerInspector } from "./containers/ContainerInspector";
import { ImageDistribution } from "./containers/ImageDistribution";
import { LifecycleAnalytics } from "./containers/LifecycleAnalytics";
import { ProjectExplorer } from "./containers/ProjectExplorer";
import { ResourceAllocation } from "./containers/ResourceAllocation";
import { containerLogsPath } from "./containers/logViewerModel";
import { DEFAULT_FILTERS, useContainerTable, type SortKey } from "./containers/useContainerTable";

const STATE_TONE: Record<ContainerState, Tone> = {
  running: "success",
  restarting: "warning",
  paused: "warning",
  created: "info",
  exited: "neutral",
  dead: "danger",
  removing: "neutral",
};

const HEALTH_TONE: Record<EffectiveHealth, Tone> = {
  healthy: "success",
  unhealthy: "danger",
  starting: "warning",
  none: "neutral",
  unknown: "neutral",
};

const STATE_OPTIONS: { value: "all" | ContainerState; label: string }[] = [
  { value: "all", label: "All states" },
  { value: "running", label: "Running" },
  { value: "restarting", label: "Restarting" },
  { value: "paused", label: "Paused" },
  { value: "created", label: "Created" },
  { value: "exited", label: "Exited" },
  { value: "dead", label: "Dead" },
];

const HEALTH_OPTIONS: { value: "all" | EffectiveHealth; label: string }[] = [
  { value: "all", label: "All health" },
  { value: "healthy", label: "Healthy" },
  { value: "unhealthy", label: "Unhealthy" },
  { value: "starting", label: "Starting" },
  { value: "none", label: "No healthcheck" },
  { value: "unknown", label: "Not running" },
];

/**
 * The container operations centre.
 *
 * Ordered summary → projects → images → resources → lifecycle → explorer,
 * which is how the questions arrive: what is the shape of the estate, how are
 * its deployments doing, what is it built from, where is the load, what has
 * been failing, and finally the one container being hunted.
 *
 * Two rules run through the whole page and both exist because Docker's own
 * model invites getting them wrong.
 *
 * *Runtime state and health are separate axes.* A container can be running and
 * unhealthy. They are shown as separate columns and separate filters, never
 * collapsed into one "status".
 *
 * *Health belongs to running containers only.* Docker freezes the last
 * healthcheck result when a container stops, so an exited container reports
 * `unhealthy` forever. This page reports that as "not running" rather than as
 * a current failure — the same bug already produced two bogus criticals on the
 * Overview before it was caught.
 *
 * Describes the selected node — the same one the node switcher and every
 * other inventory page reads via usePrimaryNodeID.
 */
export function ContainersPage() {
  const nodeID = usePrimaryNodeID();
  const containers = useContainers(nodeID);
  const latest = useLatestMetrics(nodeID);

  const [selectedID, setSelectedID] = useState<string | null>(null);

  const all = containers.data?.containers ?? emptyArray<Container>();

  // Live usage, keyed by container name — the label the stats collector emits.
  // Only running containers appear, which is what keeps stopped ones out of
  // every resource figure on the page.
  const usage = useMemo(() => readUsage(latest.data?.values ?? []), [latest.data]);

  const { filters, setFilters, projects: projectNames, images: imageNames,
    sorted, sortKey, direction, toggleSort } = useContainerTable(all, usage);

  const projects = useMemo(() => summariseProjects(all, usage), [all, usage]);
  const images = useMemo(() => summariseImages(all), [all]);
  const states = useMemo(() => countByState(all), [all]);

  // Failing health is counted over running containers only. See effectiveHealth.
  const unhealthy = useMemo(() => all.filter((c) => effectiveHealth(c) === "unhealthy").length, [all]);
  const abnormalExits = useMemo(() => all.filter((c) => readExit(c).abnormal).length, [all]);

  const selected = useMemo(() => all.find((c) => c.id === selectedID) ?? null, [all, selectedID]);

  if (inventoryGapKind(containers.error) === "agent") {
    return <AgentSubjectGap subject="containers" />;
  }

  if (containers.error instanceof ApiError && containers.error.code === "not_implemented") {
    return (
      <>
        {/* No stats here: with no Docker every figure would be zero, and a row
            of zeroes reads as "nothing is running" rather than as "Atlas cannot
            see anything to run". */}
        <PageHeader subtitle="Docker is not available on this host." />
        <Card>
          <EmptyState
            kind="unavailable"
            art={emptyArt.containers}
            title="Docker is not available on this host"
            description="Atlas found no Docker daemon here. This is a fact about the host, not a failure — the panel appears automatically wherever Docker is running."
            hint="Detection needs read access to the Docker socket. Where the daemon is running but unreadable, this page reports the permission error instead of an absence."
          />
        </Card>
      </>
    );
  }

  if (containers.isPending || containers.error || all.length === 0) {
    return (
      <>
        <PageHeader
          stats={["Containers", "Running", "Projects", "Images"].map((label) => ({
            label,
            value: "—",
            hint: containers.error ? "unavailable" : containers.isPending ? "loading" : "none",
          }))}
        />
        <Card level="floating" className="p-0">
          <QueryState
            isPending={containers.isPending}
            error={containers.error}
            isEmpty={all.length === 0}
            onRetry={() => void containers.refetch()}
            rows={5}
            empty={{
              art: emptyArt.containers,
              title: "No containers on this host",
              description:
                "The Docker daemon is reachable and reports nothing running or stopped. That is a healthy answer on a host that simply has no workloads yet.",
              hint: "Stopped containers are included. An empty list means the daemon has neither.",
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
            label: "Containers",
            value: String(all.length),
            hint: `${states.exited + states.dead} stopped · ${states.created} created`,
          },
          {
            label: "Running",
            value: `${states.running}/${all.length}`,
            hint: states.restarting > 0 ? `${states.restarting} restarting` : `${states.paused} paused`,
            tone: states.restarting > 0 ? "warning" : "success",
          },
          {
            label: "Unhealthy",
            value: String(unhealthy),
            hint: unhealthy > 0 ? "failing healthchecks now" : "none failing",
            tone: unhealthy > 0 ? "danger" : "success",
          },
          {
            label: "Projects",
            value: String(projects.filter((p) => !p.standalone).length),
            hint: `${images.length} images · ${String(projects.find((p) => p.standalone)?.total ?? 0)} standalone`,
          },
        ]}
      />

      {abnormalExits > 0 || unhealthy > 0 ? (
        <p className="mb-4 text-sm text-text-muted">
          {unhealthy > 0 ? (
            <strong className="text-danger">
              {unhealthy} running container{unhealthy === 1 ? "" : "s"} failing a healthcheck
            </strong>
          ) : null}
          {unhealthy > 0 && abnormalExits > 0 ? " · " : ""}
          {abnormalExits > 0 ? (
            <strong className="text-warning">
              {abnormalExits} ended abnormally
            </strong>
          ) : null}
        </p>
      ) : null}

      <h2 className="eyebrow mb-3">Projects</h2>
      <ProjectExplorer
        projects={projects}
        selected={filters.project === "all" ? null : filters.project}
        onSelect={(name) => { setFilters((f) => ({ ...f, project: name ?? "all" })); }}
      />

      <h2 className="eyebrow mb-3">Images</h2>
      <ImageDistribution
        images={images}
        selected={filters.image === "all" ? null : filters.image}
        onSelect={(ref) => { setFilters((f) => ({ ...f, image: ref ?? "all" })); }}
      />

      <h2 className="eyebrow mb-3">Resources</h2>
      <ResourceAllocation containers={all} usage={usage} projects={projects} />

      <h2 className="eyebrow mb-3">Lifecycle</h2>
      <LifecycleAnalytics containers={all} />

      <h2 className="eyebrow mb-3">Container explorer</h2>

      <div className={`grid grid-cols-1 gap-4 ${selected ? "xl:grid-cols-[minmax(0,1fr)_22rem]" : ""}`}>
        <Card level="floating" className="min-w-0 p-0">
          <div className="border-b border-border p-4">
            <Toolbar>
              <SearchInput
                value={filters.search}
                onChange={(v) => { setFilters((f) => ({ ...f, search: v })); }}
                placeholder="Search name, image, id, project, or service…"
              />
              <FilterSelect
                value={filters.state}
                onChange={(v) => { setFilters((f) => ({ ...f, state: v })); }}
                options={STATE_OPTIONS}
              />
              <FilterSelect
                value={filters.health}
                onChange={(v) => { setFilters((f) => ({ ...f, health: v })); }}
                options={HEALTH_OPTIONS}
              />
              <FilterSelect
                value={filters.project}
                onChange={(v) => { setFilters((f) => ({ ...f, project: v })); }}
                options={[
                  { value: "all", label: "All projects" },
                  ...projectNames.map((p) => ({ value: p, label: p })),
                ]}
              />
              <FilterSelect
                value={filters.image}
                onChange={(v) => { setFilters((f) => ({ ...f, image: v })); }}
                options={[
                  { value: "all", label: "All images" },
                  ...imageNames.map((i) => ({ value: i, label: i })),
                ]}
              />
            </Toolbar>
            <p className="mt-2 text-xs text-text-muted">
              {sorted.length} of {all.length} container{all.length === 1 ? "" : "s"} · click a row to
              inspect
            </p>
          </div>

          <QueryState
            isPending={false}
            isEmpty={sorted.length === 0}
            rows={4}
            empty={{
              kind: "filtered",
              art: emptyArt.search,
              title: "No containers match these filters",
              description: `${all.length.toLocaleString()} containers are present; the current combination of search, state, health, project and image excludes every one.`,
              action: (
                <EmptyAction onClick={() => { setFilters(DEFAULT_FILTERS); }}>
                  Clear filters
                </EmptyAction>
              ),
            }}
          />

          {sorted.length > 0 ? (
            <div className="scroll-thin max-h-[34rem] overflow-auto">
              <table aria-label="Container explorer" className="w-full border-collapse text-sm">
                <thead className="sticky top-0 z-10 bg-surface-hover">
                  <tr>
                    <SortableTh label="Container" col="name" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="State" col="state" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Health" col="health" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Project" col="project" {...{ sortKey, direction, toggleSort }} />
                    {selected ? null : (
                      <SortableTh label="Image" col="image" {...{ sortKey, direction, toggleSort }} />
                    )}
                    <SortableTh label="CPU" col="cpu" align="right" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Memory" col="memory" align="right" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Restarts" col="restarts" align="right" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Uptime" col="uptime" align="right" {...{ sortKey, direction, toggleSort }} />
                    <th className="px-3 py-2.5" />
                  </tr>
                </thead>
                <tbody>
                  {sorted.map((c) => (
                    <ContainerRow
                      key={c.id}
                      container={c}
                      usage={usage.get(c.name)}
                      selected={c.id === selectedID}
                      showImage={!selected}
                      nodeID={nodeID}
                      onSelect={() => { setSelectedID(c.id === selectedID ? null : c.id); }}
                    />
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </Card>

        {selected ? (
          <ContainerInspector
            container={selected}
            usage={usage.get(selected.name)}
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

function ContainerRow({
  container: c,
  usage,
  selected,
  showImage,
  nodeID,
  onSelect,
}: {
  container: Container;
  usage: ReturnType<typeof readUsage> extends Map<string, infer U> ? U | undefined : never;
  selected: boolean;
  showImage: boolean;
  nodeID: string | undefined;
  onSelect: () => void;
}) {
  const health = effectiveHealth(c);
  const exit = readExit(c);

  return (
    <tr
      onClick={onSelect}
      className={`cursor-pointer border-b border-border/60 transition-colors last:border-0 ${
        selected ? "bg-primary/10" : "hover:bg-surface-hover"
      }`}
    >
      <td className="max-w-[15rem] px-3 py-2">
        <span className="block truncate font-medium text-text" title={c.name}>
          {c.name}
        </span>
        <span className="block truncate font-mono text-[11px] text-text-subtle">{c.short_id}</span>
      </td>

      <td className="px-3 py-2">
        <Badge tone={STATE_TONE[c.state]} pulse={c.state === "restarting"}>
          {c.state}
        </Badge>
        {/* An abnormal exit is the reason a stopped container matters, so it
            rides with the state rather than hiding in the inspector. */}
        {exit.abnormal ? (
          <span className="mt-0.5 block font-mono text-[11px] text-danger">exit {exit.code}</span>
        ) : null}
      </td>

      <td className="px-3 py-2">
        {health === "unknown" ? (
          // Not the container's last recorded check: it is not running, so it
          // has no current health.
          <span className="text-xs text-text-subtle">—</span>
        ) : health === "none" ? (
          <span className="text-xs text-text-subtle">no check</span>
        ) : (
          <Badge tone={HEALTH_TONE[health]} pulse={health === "unhealthy"}>
            {health}
          </Badge>
        )}
      </td>

      <td className="px-3 py-2 text-xs">
        {c.compose_project ? (
          <>
            <span className="block truncate text-text">{c.compose_project}</span>
            {c.compose_service ? (
              <span className="block truncate text-[11px] text-text-subtle">{c.compose_service}</span>
            ) : null}
          </>
        ) : (
          <span className="text-text-subtle">{STANDALONE}</span>
        )}
      </td>

      {showImage ? (
        <td className="max-w-[14rem] px-3 py-2">
          <span className="block truncate text-xs text-text-muted" title={c.image}>
            {c.image}
          </span>
        </td>
      ) : null}

      <UsageCell value={usage?.cpu} format={(v) => formatValue(v, "percent")} />
      <UsageCell value={usage?.memory} format={formatBytes} />

      <td className="px-3 py-2 text-right text-xs tabular-nums">
        <span className={c.restart_count > 0 ? "text-warning" : "text-text-muted"}>
          {c.restart_count}
        </span>
      </td>

      <td className="px-3 py-2 text-right text-xs tabular-nums text-text-muted">
        {c.state === "running" && c.uptime_seconds ? formatDuration(c.uptime_seconds) : "—"}
      </td>

      <td className="px-3 py-2" onClick={(e) => { e.stopPropagation(); }}>
        <Link
          to={containerLogsPath(c.id, nodeID)}
          title="View logs"
          aria-label={`View logs for ${c.name}`}
          className="flex items-center gap-1 rounded px-1.5 py-0.5 text-[11px] text-text-muted hover:bg-surface-hover hover:text-text"
        >
          <FileText size={11} aria-hidden="true" />
          logs
        </Link>
      </td>
    </tr>
  );
}

/** A live figure, or an explicit dash when the container is not running.
 *  Never a zero — a zeroed row reads as an idle container rather than a
 *  stopped one. */
function UsageCell({
  value,
  format,
}: {
  value: number | undefined;
  format: (v: number) => string;
}) {
  return (
    <td className="px-3 py-2 text-right text-xs tabular-nums">
      {value === undefined ? (
        <span className="text-text-subtle">—</span>
      ) : (
        <span className="text-text">{format(value)}</span>
      )}
    </td>
  );
}
