import { useMemo } from "react";
import { AlertTriangle, ChevronRight, HardDrive } from "lucide-react";
import { motion } from "framer-motion";
import { useMetricSeries, useMounts, usePrimaryNodeID } from "../api/queries";
import { ApiError, inventoryGapKind } from "../api/client";
import { AgentSubjectGap } from "../components/AgentSubjectGap";
import type { Mount, Series } from "../api/types";
import { emptyArray } from "../api/empty";
import { Card, CardHeader } from "../components/Card";
import { EmptyState } from "../components/EmptyState";
import { PageHeader } from "../components/PageHeader";
import { QueryState } from "../components/QueryState";
import { RadialGauge } from "../components/viz/RadialGauge";
import { TimeSeriesChart, toChartSeries } from "../components/Chart";
import { TABLE, TABLE_WRAP, TD, TD_MUTED, TD_NUM, TH, THEAD_TR, TR } from "../components/table";
import { emptyArt, errorArt } from "../lib/assets";
import { stagger, fadeUp } from "../lib/motion";
import { formatBytes } from "../format";

/**
 * Storage.
 *
 * Built around capacity rather than around a list: the question this page
 * answers is "am I going to run out, and where", which is a shape question
 * before it is a table question. Rings carry the current level, the tree
 * shows how mount points nest, and the chart shows the direction of travel —
 * the one thing a percentage cannot tell you.
 */
export function DisksPage() {
  const nodeID = usePrimaryNodeID();
  const mounts = useMounts(nodeID);
  const history = useMetricSeries(nodeID, ["system.disk.usage"], "24h");

  const all = mounts.data?.mounts ?? emptyArray<Mount>();

  // Distinct filesystems, not distinct mount points. On macOS and on any
  // host using bind mounts, one device is mounted in several places and
  // reporting it repeatedly would triple-count the same disk.
  const pools = useMemo(() => dedupeByPool(all), [all]);
  const tree = useMemo(() => buildTree(all), [all]);

  const fullest = pools[0];
  const atRisk = pools.filter((m) => m.used_percent >= 75);

  if (inventoryGapKind(mounts.error) === "agent") {
    return <AgentSubjectGap subject="mounted filesystems" />;
  }

  if (mounts.error instanceof ApiError && mounts.error.code === "not_implemented") {
    return (
      <>
        <PageHeader title="Disks" subtitle="Mounted filesystems on this host." />
        <Card>
          <EmptyState
            kind="unavailable"
            art={errorArt.forbidden}
            title="Filesystem inventory is not available"
            description="Atlas could not enumerate mounted filesystems on this host."
            hint="Reading the mount table needs access to the host filesystem namespace. A container without it sees only its own layers."
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
            label: "Storage pools",
            value: String(pools.length),
            hint: `${all.length} mount point${all.length === 1 ? "" : "s"}`,
          },
          {
            label: "Fullest",
            value: fullest ? `${fullest.used_percent.toFixed(0)}%` : "—",
            hint: fullest?.mountpoint ?? "",
            tone: fullest && fullest.used_percent >= 90 ? "danger" : fullest && fullest.used_percent >= 75 ? "warning" : "success",
          },
          {
            label: "Free",
            value: fullest ? formatBytes(fullest.free) : "—",
            hint: "on the fullest pool",
          },
          {
            label: "Above 75%",
            value: String(atRisk.length),
            hint: atRisk.length > 0 ? "needs attention" : "all have headroom",
            tone: atRisk.length > 0 ? "warning" : "success",
          },
        ]}
      />

      <QueryState isPending={mounts.isPending} error={mounts.error} />

      {!mounts.isPending && !mounts.error ? (
        <>
          {/* Capacity first: the rings are the page's headline. */}
          <Card className="mb-6">
            <CardHeader
              title="Capacity"
              action={
                atRisk.length > 0 ? (
                  <span className="flex items-center gap-1.5 text-xs font-medium text-warning">
                    <AlertTriangle size={13} />
                    {atRisk.length} above 75%
                  </span>
                ) : (
                  <span className="text-xs text-text-muted">All filesystems have headroom</span>
                )
              }
            />
            {pools.length === 0 ? (
              <p className="py-8 text-center text-sm text-text-muted">No filesystems reported.</p>
            ) : (
              <motion.div
                variants={stagger}
                initial="hidden"
                animate="visible"
                className="flex flex-wrap justify-center gap-8 py-2 sm:justify-start"
              >
                {pools.slice(0, 6).map((m) => (
                  <motion.div key={m.device} variants={fadeUp}>
                    <RadialGauge value={m.used_percent} caption={m.mountpoint} />
                    <p className="mt-1 text-center text-[11px] text-text-muted">
                      {formatBytes(m.free)} free
                    </p>
                  </motion.div>
                ))}
              </motion.div>
            )}
          </Card>

          <div className="mb-6 grid grid-cols-1 gap-4 xl:grid-cols-[3fr_2fr]">
            <Card>
              <CardHeader
                title="Usage over 24 hours"
                action={
                  fullest ? (
                    <span className="text-xs text-text-muted">Fullest: {fullest.mountpoint}</span>
                  ) : null
                }
              />
              <UsageHistory query={history} />
            </Card>

            <Card>
              <CardHeader title="Mount hierarchy" />
              <MountTree nodes={tree} />
            </Card>
          </div>

          <Card>
            <CardHeader title="All mount points" />
            <div className={TABLE_WRAP}>
              <table className={TABLE}>
                <thead>
                  <tr className={THEAD_TR}>
                    <th className={TH}>Mount</th>
                    <th className={TH}>Filesystem</th>
                    <th className={TH}>Used</th>
                    <th className={TH}>Free</th>
                    <th className={TH}>Usage</th>
                    <th className={TH}>Inodes</th>
                  </tr>
                </thead>
                <tbody>
                  {[...all]
                    .sort((a, b) => a.mountpoint.localeCompare(b.mountpoint))
                    .map((m) => (
                      <MountRow key={m.mountpoint} mount={m} />
                    ))}
                </tbody>
              </table>
            </div>
          </Card>
        </>
      ) : null}
    </>
  );
}

/** The stored per-mountpoint capacity series, drawn as lines. */
function UsageHistory({ query }: { query: ReturnType<typeof useMetricSeries> }) {
  const series = useMemo(() => {
    const all = query.data?.series ?? [];
    // The two busiest filesystems. Nine overlapping lines at identical
    // values — which is what a macOS volume group produces — is noise.
    return [...all]
      .sort((a, b) => lastValue(b) - lastValue(a))
      .slice(0, 2)
      .map((s, i) =>
        toChartSeries(s, s.labels?.mountpoint ?? s.metric, i === 0 ? 1 : 2),
      );
  }, [query.data]);

  return (
    <>
      <QueryState
        isPending={query.isPending}
        error={query.error}
        isEmpty={series.length === 0}
        onRetry={() => void query.refetch()}
        rows={3}
        empty={{
          art: emptyArt.reports,
          title: "No stored history yet",
          description:
            "Capacity is sampled once a minute and written to storage. A chart appears as soon as there are two points to draw a line between.",
          hint: "Current usage above is read live and does not depend on stored history.",
        }}
      />
      {!query.isPending && !query.error && series.length > 0 ? (
        <>
          <div className="mb-2 flex flex-wrap gap-4 text-xs">
            {series.map((s) => (
              <span key={s.slot} className="flex items-center gap-1.5 text-text-muted">
                <span
                  aria-hidden="true"
                  className="h-0.5 w-3 rounded-full"
                  style={{ background: `var(--series-${s.slot})` }}
                />
                {s.label}
              </span>
            ))}
          </div>
          <TimeSeriesChart series={series} unit="percent" percentScale area height={190} />
        </>
      ) : null}
    </>
  );
}

interface TreeNode {
  path: string;
  label: string;
  depth: number;
  mount: Mount;
}

/**
 * The mount points as a hierarchy.
 *
 * Mount paths already describe a tree; showing them as a flat alphabetical
 * list throws that away. Indentation makes it obvious at a glance that
 * /System/Volumes/Data sits under /System/Volumes, and that a nested mount
 * shares — or does not share — its parent's device.
 */
function MountTree({ nodes }: { nodes: TreeNode[] }) {
  if (nodes.length === 0) {
    return <p className="py-8 text-center text-sm text-text-muted">Nothing mounted.</p>;
  }

  return (
    <ul className="flex flex-col gap-0.5 font-mono text-xs">
      {nodes.map((n) => {
        const tone =
          n.mount.used_percent >= 90
            ? "text-danger"
            : n.mount.used_percent >= 75
              ? "text-warning"
              : "text-text-muted";
        return (
          <li
            key={n.path}
            className="flex items-center gap-2 rounded px-2 py-1.5 hover:bg-surface-hover"
            style={{ paddingLeft: `${0.5 + n.depth * 0.9}rem` }}
          >
            {n.depth > 0 ? (
              <ChevronRight size={11} className="shrink-0 text-text-muted/50" aria-hidden="true" />
            ) : (
              <HardDrive size={11} className="shrink-0 text-text-muted" aria-hidden="true" />
            )}
            <span className="min-w-0 flex-1 truncate text-text" title={n.mount.mountpoint}>
              {n.label}
            </span>
            {/* A miniature capacity bar, so the tree carries level as well as
                shape. */}
            <span className="h-1 w-14 shrink-0 overflow-hidden rounded-full bg-surface-hover">
              <span
                className="block h-full rounded-full"
                style={{
                  width: `${Math.min(n.mount.used_percent, 100)}%`,
                  background:
                    n.mount.used_percent >= 90
                      ? "var(--danger)"
                      : n.mount.used_percent >= 75
                        ? "var(--warning)"
                        : "var(--primary)",
                }}
              />
            </span>
            <span className={`w-11 shrink-0 text-right tabular-nums ${tone}`}>
              {n.mount.used_percent.toFixed(0)}%
            </span>
          </li>
        );
      })}
    </ul>
  );
}

function MountRow({ mount: m }: { mount: Mount }) {
  return (
    <tr className={TR}>
      <td className={TD}>
        <span className="block font-medium">{m.mountpoint}</span>
        <span className="block text-xs text-text-muted">{m.device}</span>
      </td>
      <td className={TD_MUTED}>{m.fstype}</td>
      <td className={TD_NUM}>{formatBytes(m.used)}</td>
      <td className={TD_NUM}>{formatBytes(m.free)}</td>
      <td className={m.used_percent >= 90 ? "px-4 py-3 text-right tabular-nums text-danger" : TD_NUM}>
        {m.used_percent.toFixed(1)}%
      </td>
      <td className={(m.inodes_used_percent ?? 0) >= 90 ? "px-4 py-3 text-right tabular-nums text-danger" : TD_NUM}>
        {m.inodes_total ? `${(m.inodes_used_percent ?? 0).toFixed(1)}%` : "—"}
      </td>
    </tr>
  );
}

/**
 * One entry per distinct pool of storage, fullest first.
 *
 * Grouping by device name is not enough. An APFS container presents each
 * volume as its own device — disk3s1s1, disk3s5, disk3s2 — while all of them
 * draw from one shared free-space pool; Linux bind mounts and btrfs subvolumes
 * behave the same way. Rendering a ring per device would show six separate
 * disks at 94% when there is one disk at 94%, which turns a single capacity
 * risk into an apparent fleet of them.
 *
 * Identical total *and* identical free is the signal: two filesystems that
 * genuinely happen to match on both, to the byte, are the same storage.
 */
function dedupeByPool(mounts: Mount[]): Mount[] {
  const seen = new Map<string, Mount>();
  for (const m of mounts) {
    const pool = `${m.total}:${m.free}`;
    const existing = seen.get(pool);
    // Keep the shallowest mount point: it is the one an operator recognises,
    // and the nested ones describe the same bytes.
    if (!existing || m.mountpoint.length < existing.mountpoint.length) {
      seen.set(pool, m);
    }
  }
  return [...seen.values()].sort((a, b) => b.used_percent - a.used_percent);
}

/** Orders mount points into a tree by path nesting. */
function buildTree(mounts: Mount[]): TreeNode[] {
  const sorted = [...mounts].sort((a, b) => a.mountpoint.localeCompare(b.mountpoint));

  return sorted.map((m) => {
    // Depth is how many other mount points this path sits beneath, which is
    // the real nesting rather than a count of slashes: "/" is a parent of
    // everything but has no slashes to count.
    const parents = sorted.filter(
      (other) => other.mountpoint !== m.mountpoint && m.mountpoint.startsWith(rootedPrefix(other.mountpoint)),
    );
    const deepestParent = parents.reduce<string>(
      (longest, p) => (p.mountpoint.length > longest.length ? p.mountpoint : longest),
      "",
    );
    const label =
      deepestParent && m.mountpoint.startsWith(rootedPrefix(deepestParent))
        ? m.mountpoint.slice(rootedPrefix(deepestParent).length)
        : m.mountpoint;

    return { path: m.mountpoint, label: label || m.mountpoint, depth: parents.length, mount: m };
  });
}

/** "/" stays "/", anything else gains a trailing slash so that "/var" does
 *  not match "/variable". */
function rootedPrefix(path: string): string {
  return path.endsWith("/") ? path : `${path}/`;
}

function lastValue(s: Series): number {
  return s.points[s.points.length - 1]?.value ?? 0;
}
