import { useState } from "react";
import { X } from "lucide-react";
import type { CollectorsResponse, LatestValue, Node } from "../../api/types";
import { Badge, type Tone } from "../../components/Badge";
import { EmptyState } from "../../components/EmptyState";
import { CopyButton } from "../processes/ProcessInspector";
import { emptyArt } from "../../lib/assets";
import { nodePrimaryLabel, nodeSecondaryLabel } from "../../lib/nodeIdentity";
import { formatAgo, formatBytes, formatDuration, formatValue } from "../../format";
import { readVitals, type Filesystem, type Interface, type Vitals } from "./nodeMetrics";
import { UNTAGGED } from "./useNodeTable";

const STATUS_TONE: Record<Node["status"], Tone> = {
  up: "success",
  stale: "warning",
  down: "danger",
};

type Tab = "hardware" | "resources" | "network" | "workload" | "metadata";

/**
 * The right-hand inspector for one node.
 *
 * Docked rather than modal, for the same reason as the process explorer's: an
 * operator comparing a node against the fleet it came from needs both on
 * screen at once.
 *
 * The panel is built on the split that governs this whole page. *Inventory* —
 * cores, kernel, architecture, agent version — comes from the node record and
 * is shown for every node, including ones that are down, because last-known
 * hardware is still true. *Vitals* come from per-node metrics and simply do
 * not exist for a node that stopped reporting, so those tabs say so instead of
 * rendering zeroes.
 *
 * Container and port detail is read from this node's own metrics rather than
 * from `/containers` and `/ports`. Those endpoints describe the host Atlas
 * itself runs on and are not node-scoped, so using them here would attribute
 * the local machine's workload to whichever node happened to be selected.
 */
export function NodeInspector({
  node,
  values,
  collectors,
  isPrimary,
  onClose,
}: {
  node: Node;
  values: LatestValue[];
  collectors: CollectorsResponse | undefined;
  /** True when this node is the host Atlas runs on, which is the only node
   *  whose collector health Atlas can actually report. */
  isPrimary: boolean;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<Tab>("hardware");
  const v = readVitals(values);

  return (
    <aside
      aria-label={`Node details: ${nodePrimaryLabel(node)}`}
      className="elev-3 sticky top-0 flex max-h-[calc(100vh-7rem)] flex-col overflow-hidden rounded-xl"
    >
      <header className="flex items-start justify-between gap-3 border-b border-border p-4">
        <div className="min-w-0">
          <h2 className="truncate text-base font-semibold text-text" title={node.node_id}>
            {nodePrimaryLabel(node)}
          </h2>
          {nodeSecondaryLabel(node) ? (
            <p className="truncate text-xs text-text-muted" title={nodeSecondaryLabel(node)}>
              {nodeSecondaryLabel(node)}
            </p>
          ) : null}
          <div className="mt-1 flex flex-wrap items-center gap-2">
            <Badge tone={STATUS_TONE[node.status]} pulse={node.status === "down"}>
              {node.status}
            </Badge>
            <span className="text-xs text-text-muted">
              {node.environment ?? UNTAGGED}
            </span>
          </div>
        </div>
        <button
          type="button"
          onClick={onClose}
          aria-label="Close inspector"
          className="shrink-0 rounded p-1 text-text-muted hover:bg-surface-hover hover:text-text"
        >
          <X size={16} />
        </button>
      </header>

      <nav className="flex flex-wrap gap-1 border-b border-border px-2 py-1.5" role="tablist">
        {(
          [
            ["hardware", "Hardware"],
            ["resources", "Resources"],
            ["network", "Network"],
            ["workload", "Workload"],
            ["metadata", "Metadata"],
          ] as const
        ).map(([id, label]) => (
          <button
            key={id}
            role="tab"
            aria-selected={tab === id}
            type="button"
            onClick={() => { setTab(id); }}
            className={`rounded-md px-2.5 py-1.5 text-xs font-medium whitespace-nowrap transition-colors ${
              tab === id ? "bg-primary/15 text-primary" : "text-text-muted hover:bg-surface-hover"
            }`}
          >
            {label}
          </button>
        ))}
      </nav>

      <div className="scroll-thin flex-1 overflow-y-auto p-4">
        {tab === "hardware" ? <Hardware node={node} vitals={v} /> : null}
        {tab === "resources" ? <Resources vitals={v} node={node} /> : null}
        {tab === "network" ? <Network vitals={v} node={node} /> : null}
        {tab === "workload" ? (
          <Workload vitals={v} node={node} collectors={collectors} isPrimary={isPrimary} />
        ) : null}
        {tab === "metadata" ? <Metadata node={node} /> : null}
      </div>
    </aside>
  );
}

/**
 * Shown on every vitals tab for a node that is not reporting.
 *
 * Rendering a zeroed CPU gauge for a machine Atlas cannot reach is the exact
 * failure this product exists to avoid: it looks like a quiet, healthy host.
 */
function NotReporting({ node }: { node: Node }) {
  return (
    <EmptyState
      kind="unavailable"
      art={emptyArt.servers}
      title="This node is not reporting"
      description={`No metrics have arrived within the resolution window. Its last contact was ${formatAgo(node.seconds_since_seen)}.`}
      hint="Hardware and metadata below are the last known values and remain accurate. Live figures are omitted rather than shown as zero."
      compact
    />
  );
}

function Hardware({ node, vitals: v }: { node: Node; vitals: Vitals }) {
  return (
    <dl className="flex flex-col">
      <Field label="CPU cores" value={node.cpu_cores ?? v.cores ?? "—"} />
      <Field label="Architecture" value={node.architecture ?? "—"} />
      <Field label="Operating system" value={node.os ?? "—"} />
      <Field label="Platform" value={node.platform ?? "—"} />
      <Field label="Kernel" value={node.kernel ?? "—"} />
      <Field
        label="Memory"
        value={v.memoryTotal ? formatBytes(v.memoryTotal) : "—"}
      />
      <Field
        label="Storage"
        value={
          v.filesystems.length > 0
            ? `${formatBytes(v.filesystems.reduce((s, f) => s + (f.total ?? 0), 0))} · ${String(v.filesystems.length)} filesystem${v.filesystems.length === 1 ? "" : "s"}`
            : "—"
        }
      />
      <Field
        label="Uptime"
        value={node.uptime_seconds ? formatDuration(node.uptime_seconds) : "—"}
      />
      <Field label="Booted" value={node.boot_time ? new Date(node.boot_time).toLocaleString() : "—"} />

      {!v.reporting ? (
        <p className="mt-4 border-t border-border pt-3 text-[11px] leading-relaxed text-text-subtle">
          Memory and storage totals come from metrics and are unavailable while this node is not
          reporting. Everything above them is inventory recorded at last contact.
        </p>
      ) : null}
    </dl>
  );
}

function Resources({ vitals: v, node }: { vitals: Vitals; node: Node }) {
  if (!v.reporting) return <NotReporting node={node} />;

  return (
    <div className="flex flex-col gap-5">
      <section>
        <div className="mb-2 flex items-baseline justify-between">
          <h3 className="eyebrow">CPU</h3>
          <span className="text-sm font-semibold tabular-nums text-text">
            {v.cpu !== undefined ? formatValue(v.cpu, "percent") : "—"}
          </span>
        </div>
        {v.coreUsage.length > 0 ? (
          <>
            {/* Per-core rather than one average: a saturated single core on an
                otherwise idle box is a pinned thread, and the aggregate hides
                it completely. */}
            <div className="flex flex-wrap gap-1">
              {v.coreUsage.map((c) => (
                <span
                  key={c.core}
                  title={`core ${c.core} · ${c.value.toFixed(1)}%`}
                  className="h-8 w-3 overflow-hidden rounded-sm bg-surface-hover"
                >
                  <span
                    className={`block w-full ${c.value >= 90 ? "bg-danger" : c.value >= 75 ? "bg-warning" : "bg-primary"}`}
                    style={{ height: `${String(Math.min(Math.max(c.value, 2), 100))}%`, marginTop: `${String(100 - Math.min(Math.max(c.value, 2), 100))}%` }}
                  />
                </span>
              ))}
            </div>
            <p className="mt-1.5 text-[11px] text-text-subtle">
              {v.coreUsage.length} cores, each shown separately
            </p>
          </>
        ) : null}
      </section>

      <section>
        <h3 className="eyebrow mb-2">Memory</h3>
        <Meter
          percent={v.memoryPercent}
          label={
            v.memoryUsed && v.memoryTotal
              ? `${formatBytes(v.memoryUsed)} of ${formatBytes(v.memoryTotal)}`
              : v.memoryPercent !== undefined
                ? formatValue(v.memoryPercent, "percent")
                : "—"
          }
        />
        {v.swapTotal ? (
          <div className="mt-3">
            <p className="mb-1.5 text-[11px] text-text-muted">
              Swap · {formatBytes(v.swapTotal)}
            </p>
            <Meter percent={v.swapPercent} label={v.swapPercent !== undefined ? formatValue(v.swapPercent, "percent") : "—"} compact />
          </div>
        ) : null}
      </section>

      <section>
        <h3 className="eyebrow mb-2">Load average</h3>
        <div className="grid grid-cols-3 gap-2">
          <LoadCell label="1m" value={v.load1} />
          <LoadCell label="5m" value={v.load5} />
          <LoadCell label="15m" value={v.load15} />
        </div>
        {v.loadPerCore1 !== undefined ? (
          // Load only means anything relative to core count, and this is the
          // number that actually says whether the machine is saturated.
          <p className="mt-2 text-[11px] text-text-subtle">
            {v.loadPerCore1.toFixed(2)} per core over 1m
            {v.loadPerCore1 >= 1 ? " — at or above saturation" : ""}
          </p>
        ) : null}
      </section>

      <section>
        <h3 className="eyebrow mb-2">Storage</h3>
        {v.filesystems.length === 0 ? (
          <p className="text-xs text-text-muted">No filesystems reported.</p>
        ) : (
          <ul className="flex flex-col gap-2.5">
            {v.filesystems.map((f) => (
              <FilesystemRow key={f.mountpoint} fs={f} />
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}

function FilesystemRow({ fs }: { fs: Filesystem }) {
  return (
    <li>
      <div className="mb-1 flex items-baseline justify-between gap-2">
        <span className="truncate font-mono text-[11px] text-text" title={fs.mountpoint}>
          {fs.mountpoint}
        </span>
        <span className="shrink-0 text-xs tabular-nums text-text-muted">
          {fs.total ? formatBytes(fs.total) : ""}
        </span>
      </div>
      <Meter percent={fs.usedPercent} label={formatValue(fs.usedPercent, "percent")} compact />
      {fs.inodesPercent !== undefined && fs.inodesPercent >= 85 ? (
        <p className="mt-1 text-[11px] text-danger">
          {fs.inodesPercent.toFixed(0)}% of inodes used — this fills independently of capacity
        </p>
      ) : null}
    </li>
  );
}

function Network({ vitals: v, node }: { vitals: Vitals; node: Node }) {
  if (!v.reporting) return <NotReporting node={node} />;

  if (v.interfaces.length === 0) {
    return <p className="py-6 text-center text-xs text-text-muted">No interfaces reported.</p>;
  }

  return (
    <ul className="flex flex-col gap-2">
      {v.interfaces.map((i) => (
        <InterfaceRow key={i.name} iface={i} />
      ))}
    </ul>
  );
}

function InterfaceRow({ iface: i }: { iface: Interface }) {
  const errors = i.rxErrors + i.txErrors;
  const dropped = i.rxDropped + i.txDropped;

  return (
    <li className="elev-1 rounded-lg p-2.5">
      <div className="mb-1.5 flex items-baseline justify-between gap-2">
        <span className="truncate font-mono text-xs font-semibold text-text">{i.name}</span>
        {errors > 0 || dropped > 0 ? (
          <Badge tone={errors > 0 ? "danger" : "warning"}>
            {errors > 0 ? `${errors.toFixed(0)} err/s` : `${dropped.toFixed(0)} drop/s`}
          </Badge>
        ) : null}
      </div>
      <div className="flex gap-4 text-[11px] text-text-muted">
        <span>↓ {formatValue(i.rx, "bytes_per_second")}</span>
        <span>↑ {formatValue(i.tx, "bytes_per_second")}</span>
      </div>
    </li>
  );
}

/**
 * What the node is running: containers, listening sockets, processes, and the
 * collectors producing all of it.
 *
 * The counts come from this node's own metrics. Collector health, by contrast,
 * is only meaningful for the host Atlas runs on — the scheduler reports on
 * itself — so it is stated as such rather than implied to describe a remote
 * machine.
 */
function Workload({
  vitals: v,
  node,
  collectors,
  isPrimary,
}: {
  vitals: Vitals;
  node: Node;
  collectors: CollectorsResponse | undefined;
  isPrimary: boolean;
}) {
  if (!v.reporting) return <NotReporting node={node} />;

  const list = collectors?.collectors ?? [];
  const failing = list.filter((c) => !c.healthy);

  return (
    <div className="flex flex-col gap-5">
      <section>
        <h3 className="eyebrow mb-2">Running</h3>
        <dl className="flex flex-col">
          <Field
            label="Containers"
            value={
              v.containersTotal === undefined
                ? "no Docker on this node"
                : `${String(v.containersRunning ?? 0)} running of ${String(v.containersTotal)}`
            }
          />
          <Field label="Listening ports" value={v.portsListening ?? "—"} />
          <Field label="Processes" value={v.processesTotal?.toLocaleString() ?? "—"} />
          <Field
            label="Zombies"
            value={
              v.zombies === undefined ? "—" : v.zombies > 0 ? `${String(v.zombies)} — not being reaped` : "0"
            }
          />
        </dl>
      </section>

      <section>
        <div className="mb-2 flex items-baseline justify-between">
          <h3 className="eyebrow">Collectors</h3>
          {isPrimary && list.length > 0 ? (
            <span className="text-xs tabular-nums text-text-muted">
              {list.length - failing.length}/{list.length} healthy
            </span>
          ) : null}
        </div>

        {!isPrimary ? (
          // Collector health is reported by the scheduler about itself. Showing
          // this host's collectors under a remote node would be a plain
          // misattribution, so it is withheld with the reason given.
          <p className="text-[11px] leading-relaxed text-text-subtle">
            Collector health is reported by the Atlas instance about its own scheduler. It describes
            the host Atlas runs on, not this node, so it is not shown here.
          </p>
        ) : list.length === 0 ? (
          <p className="text-xs text-text-muted">No collectors are registered.</p>
        ) : (
          <ul className="flex flex-col gap-1.5">
            {list.map((c) => (
              <li key={c.collector_id} className="flex items-center gap-2 text-xs">
                <span
                  aria-hidden="true"
                  className={`h-1.5 w-1.5 shrink-0 rounded-full ${c.healthy ? "bg-success" : "bg-danger"}`}
                />
                <span className="truncate text-text">{c.name}</span>
                <span className="ml-auto shrink-0 tabular-nums text-text-subtle">{c.interval}</span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}

function Metadata({ node }: { node: Node }) {
  return (
    <div className="flex flex-col gap-4">
      <div>
        <div className="mb-1.5 flex items-center justify-between">
          <h3 className="eyebrow">Node ID</h3>
          <CopyButton value={node.node_id} label="Copy" />
        </div>
        <code className="elev-1 block rounded-lg p-2.5 font-mono text-[11px] break-all text-text">
          {node.node_id}
        </code>
      </div>

      <dl className="flex flex-col">
        <Field label="Hostname" value={node.hostname} />
        <Field label="Environment" value={node.environment ?? "not tagged"} />
        <Field label="Agent version" value={node.agent_version ?? "—"} />
        <Field label="First seen" value={new Date(node.first_seen_at).toLocaleString()} />
        <Field label="Last seen" value={`${new Date(node.last_seen_at).toLocaleString()} · ${formatAgo(node.seconds_since_seen)}`} />
      </dl>

      {!node.environment ? (
        <p className="text-[11px] leading-relaxed text-text-subtle">
          No environment tag. Atlas never guesses which tier a host belongs to — set{" "}
          <code className="font-mono">node.environment</code> on the agent to group it.
        </p>
      ) : null}
    </div>
  );
}

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3 border-b border-border py-2.5 text-sm last:border-0">
      <dt className="shrink-0 text-text-muted">{label}</dt>
      <dd className="truncate text-right text-text">{value}</dd>
    </div>
  );
}

function LoadCell({ label, value }: { label: string; value: number | undefined }) {
  return (
    <div className="elev-1 rounded-lg p-2 text-center">
      <span className="block text-sm font-semibold tabular-nums text-text">
        {value !== undefined ? value.toFixed(2) : "—"}
      </span>
      <span className="eyebrow">{label}</span>
    </div>
  );
}

function Meter({
  percent,
  label,
  compact = false,
}: {
  percent: number | undefined;
  label: string;
  compact?: boolean;
}) {
  const pct = Math.min(Math.max(percent ?? 0, 0), 100);
  const bar = pct >= 90 ? "bg-danger" : pct >= 75 ? "bg-warning" : "bg-primary";

  return (
    <>
      {!compact ? (
        <p className="mb-1.5 text-lg font-semibold tabular-nums text-text">{label}</p>
      ) : null}
      <div className="h-1.5 overflow-hidden rounded-full bg-surface-hover">
        <div className={`h-full rounded-full ${bar}`} style={{ width: `${String(pct)}%` }} />
      </div>
      {compact ? <p className="mt-1 text-[11px] tabular-nums text-text-muted">{label}</p> : null}
    </>
  );
}
