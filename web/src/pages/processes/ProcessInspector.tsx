import { useMemo, useState } from "react";
import { Copy, X } from "lucide-react";
import { usePorts } from "../../api/queries";
import type { Port, Process, ProcessState, Series } from "../../api/types";
import { Badge, type Tone } from "../../components/Badge";
import { TimeSeriesChart, toChartSeries } from "../../components/Chart";
import { formatBytes, formatDuration } from "../../format";

const STATE_TONE: Record<ProcessState, Tone> = {
  running: "success",
  sleeping: "neutral",
  stopped: "danger",
  zombie: "danger",
  idle: "neutral",
  other: "neutral",
};

type Tab = "overview" | "resources" | "network" | "command";

/**
 * The right-hand inspector for one process.
 *
 * A docked panel rather than a modal: an operator comparing a process against
 * the list it came from needs both on screen, and a modal hides the table
 * while they do it. The selected row stays highlighted for the same reason.
 *
 * Two tabs the brief asked for are deliberately absent.
 *
 * *Environment* — Atlas does not collect process environment variables, and
 * should not. They routinely carry database passwords, API tokens and signing
 * keys; a monitoring tool that reads them turns every dashboard viewer into a
 * holder of every secret on the host. This is a decision, not a gap.
 *
 * *Open files* — genuinely not collected. It needs a per-process descriptor
 * walk, which is expensive on a busy host and belongs behind an explicit
 * opt-in rather than running on every sweep.
 */
export function ProcessInspector({
  process,
  cpuHistory,
  onClose,
}: {
  process: Process;
  /** Per-name CPU series, already fetched by the page. */
  cpuHistory: Series[];
  onClose: () => void;
}) {
  const [tab, setTab] = useState<Tab>("overview");
  const ports = usePorts();

  // Sockets this PID owns. The ports plugin records the owning process, so
  // this is a real cross-reference rather than an inference from names.
  const sockets = useMemo(
    () => (ports.data?.ports ?? []).filter((p) => p.pid === process.pid),
    [ports.data, process.pid],
  );

  const series = useMemo(
    () => cpuHistory.filter((s) => s.labels?.process === process.name),
    [cpuHistory, process.name],
  );

  return (
    <aside
      aria-label={`Process details: ${process.name}`}
      className="elev-3 sticky top-0 flex max-h-[calc(100vh-7rem)] flex-col overflow-hidden rounded-xl"
    >
      <header className="flex items-start justify-between gap-3 border-b border-border p-4">
        <div className="min-w-0">
          <h2 className="truncate text-base font-semibold text-text" title={process.name}>
            {process.name}
          </h2>
          <p className="mt-0.5 font-mono text-xs text-text-muted">
            pid {process.pid}
            {process.ppid ? ` · parent ${process.ppid}` : ""}
          </p>
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

      <nav className="flex gap-1 border-b border-border px-2 py-1.5" role="tablist">
        {(
          [
            ["overview", "Overview", 0],
            ["resources", "Resources", 0],
            // The socket count rides as a badge rather than a parenthetical:
            // widening the label pushed the four tabs past the panel and wrapped
            // them onto two ragged lines.
            ["network", "Network", sockets.length],
            ["command", "Command", 0],
          ] as const
        ).map(([id, label, count]) => (
          <button
            key={id}
            role="tab"
            aria-selected={tab === id}
            type="button"
            onClick={() => { setTab(id); }}
            className={`flex items-center gap-1 rounded-md px-2.5 py-1.5 text-xs font-medium whitespace-nowrap transition-colors ${
              tab === id ? "bg-primary/15 text-primary" : "text-text-muted hover:bg-surface-hover"
            }`}
          >
            {label}
            {count > 0 ? (
              <span className="rounded-full bg-primary/15 px-1.5 text-[10px] font-semibold tabular-nums text-primary">
                {count}
              </span>
            ) : null}
          </button>
        ))}
      </nav>

      <div className="scroll-thin flex-1 overflow-y-auto p-4">
        {tab === "overview" ? <Overview process={process} /> : null}
        {tab === "resources" ? <Resources process={process} series={series} /> : null}
        {tab === "network" ? <Network sockets={sockets} pending={ports.isPending} /> : null}
        {tab === "command" ? <Command process={process} /> : null}
      </div>
    </aside>
  );
}

function Overview({ process: p }: { process: Process }) {
  return (
    <dl className="flex flex-col">
      <Field label="Status" value={<Badge tone={STATE_TONE[p.state]}>{p.state}</Badge>} />
      <Field label="User" value={p.username ?? "unavailable"} />
      <Field label="CPU" value={`${p.cpu_percent.toFixed(1)}%`} />
      <Field
        label="Memory"
        value={`${formatBytes(p.memory_rss)}${p.memory_percent ? ` · ${p.memory_percent.toFixed(1)}% of host` : ""}`}
      />
      <Field label="Threads" value={p.threads} />
      <Field label="Running for" value={p.running_for_seconds ? formatDuration(p.running_for_seconds) : "unknown"} />
      <Field label="Parent PID" value={p.ppid || "—"} />
    </dl>
  );
}

function Resources({ process: p, series }: { process: Process; series: Series[] }) {
  const chart = useMemo(
    () => series.slice(0, 1).map((s) => toChartSeries(s, p.name, 1)),
    [series, p.name],
  );

  return (
    <div className="flex flex-col gap-5">
      <div>
        <div className="mb-2 flex items-baseline justify-between">
          <h3 className="eyebrow">CPU, last hour</h3>
          <span className="text-sm font-semibold tabular-nums text-text">
            {p.cpu_percent.toFixed(1)}%
          </span>
        </div>
        {chart.length > 0 ? (
          <TimeSeriesChart series={chart} unit="percent" area height={130} />
        ) : (
          <p className="py-6 text-center text-xs text-text-muted">
            No stored series for this process name yet.
          </p>
        )}
        {/* The stored series are aggregated by name, and saying so matters:
            with forty workers sharing a name the line is their sum, not this
            PID's own usage. */}
        {chart.length > 0 ? (
          <p className="mt-2 text-[11px] text-text-muted">
            Aggregated across every process named “{p.name}”, not this PID alone.
          </p>
        ) : null}
      </div>

      <div>
        <h3 className="eyebrow mb-2">Memory</h3>
        <Meter
          label={formatBytes(p.memory_rss)}
          percent={p.memory_percent}
          caption={`${p.memory_percent.toFixed(1)}% of host memory`}
        />
      </div>

      <div>
        <h3 className="eyebrow mb-2">Threads</h3>
        <p className="text-2xl font-semibold tabular-nums text-text">{p.threads}</p>
      </div>
    </div>
  );
}

function Network({ sockets, pending }: { sockets: Port[]; pending: boolean }) {
  if (pending) {
    return <p className="py-6 text-center text-xs text-text-muted">Loading sockets…</p>;
  }
  if (sockets.length === 0) {
    return (
      <p className="py-6 text-center text-xs text-text-muted">
        This process is not listening on any port. Outbound connections are not tracked — Atlas reads
        listening sockets only.
      </p>
    );
  }

  return (
    <ul className="flex flex-col gap-2">
      {sockets.map((s) => (
        <li
          key={`${s.protocol}:${s.address}:${s.port}`}
          className="elev-1 flex items-center gap-3 rounded-lg p-2.5"
        >
          <span className="font-mono text-sm font-semibold text-text">{s.port}</span>
          <span className="eyebrow">{s.protocol}</span>
          <span className="ml-auto truncate font-mono text-[11px] text-text-muted">{s.address}</span>
          {s.tls ? (
            <Badge tone={s.tls.expired ? "danger" : "success"}>
              {s.tls.expired ? "expired" : "TLS"}
            </Badge>
          ) : null}
        </li>
      ))}
    </ul>
  );
}

function Command({ process: p }: { process: Process }) {
  return (
    <div className="flex flex-col gap-4">
      <Block label="Executable" value={p.executable} />
      <Block label="Command line" value={p.cmdline} />
      {!p.executable && !p.cmdline ? (
        <p className="text-[11px] text-text-muted">
          Reading another user's command line requires privilege Atlas does not have here. Running as
          root would reveal it — this is a permission boundary, not a failure.
        </p>
      ) : null}
    </div>
  );
}

function Block({ label, value }: { label: string; value?: string | undefined }) {
  if (!value) return null;
  return (
    <div>
      <div className="mb-1.5 flex items-center justify-between">
        <h3 className="eyebrow">{label}</h3>
        <CopyButton value={value} />
      </div>
      <code className="elev-1 block rounded-lg p-2.5 font-mono text-[11px] leading-relaxed break-all text-text">
        {value}
      </code>
    </div>
  );
}

/**
 * Copies a value to the clipboard.
 *
 * This is what "actions" means in Atlas. The platform observes and never
 * modifies, so there is no kill, no renice, no restart — the useful action on
 * a process is getting its identifiers into a terminal where an operator can
 * act with their own authority and their own audit trail.
 */
export function CopyButton({ value, label = "Copy" }: { value: string; label?: string }) {
  const [copied, setCopied] = useState(false);

  return (
    <button
      type="button"
      onClick={() => {
        void navigator.clipboard.writeText(value).then(() => {
          setCopied(true);
          window.setTimeout(() => { setCopied(false); }, 1500);
        });
      }}
      className="flex items-center gap-1 rounded px-1.5 py-0.5 text-[11px] text-text-muted hover:bg-surface-hover hover:text-text"
      title={`${label} to clipboard`}
    >
      <Copy size={11} />
      {copied ? "Copied" : label}
    </button>
  );
}

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3 border-b border-border py-2.5 text-sm last:border-0">
      <dt className="text-text-muted">{label}</dt>
      <dd className="text-right text-text">{value}</dd>
    </div>
  );
}

function Meter({ label, percent, caption }: { label: string; percent: number; caption: string }) {
  return (
    <>
      <div className="mb-1.5 flex items-baseline justify-between">
        <span className="text-lg font-semibold tabular-nums text-text">{label}</span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-surface-hover">
        <div
          className="h-full rounded-full bg-primary"
          style={{ width: `${Math.min(Math.max(percent, 0), 100)}%` }}
        />
      </div>
      <p className="mt-1 text-[11px] text-text-muted">{caption}</p>
    </>
  );
}
