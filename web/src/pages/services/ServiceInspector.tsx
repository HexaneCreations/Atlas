import { useState } from "react";
import { AlertTriangle, X } from "lucide-react";
import { useServiceDetail } from "../../api/queries";
import type { GraphEdge, ServiceImpact } from "../../api/types";
import { Badge, type Tone } from "../../components/Badge";
import { QueryState } from "../../components/QueryState";
import { CopyButton } from "../processes/ProcessInspector";
import { formatBytes, formatDuration } from "../../format";
import { HEALTH_LABEL, HEALTH_TONE, KIND_EXPLANATION } from "./serviceLabels";
import type { ServiceRow } from "./useServiceTable";

type Tab = "overview" | "dependencies" | "dependents" | "impact";

/**
 * The right-hand inspector for one systemd unit.
 *
 * Relationship data comes from `GET /services/{unit}`, which returns direct
 * edges and the computed blast radius. Nothing here re-derives relationships:
 * whether a dependent is hard or soft, and whether a failure propagates, are
 * server-side answers, and computing them again in the browser would mean two
 * implementations that eventually disagree.
 */
export function ServiceInspector({
  row,
  onClose,
  onSelectUnit,
}: {
  row: ServiceRow;
  onClose: () => void;
  /** Jumping to a related unit is how an operator walks a dependency chain. */
  onSelectUnit: (unit: string) => void;
}) {
  const [tab, setTab] = useState<Tab>("overview");
  const detail = useServiceDetail(row.unit.name);

  const impact = detail.data?.impact;
  const impactCount = impact ? impact.hard.length + impact.soft.length : 0;

  return (
    <aside
      aria-label={`Service details: ${row.unit.name}`}
      className="elev-3 sticky top-0 flex max-h-[calc(100vh-7rem)] flex-col overflow-hidden rounded-xl"
    >
      <header className="flex items-start justify-between gap-3 border-b border-border p-4">
        <div className="min-w-0">
          <h2 className="truncate text-base font-semibold text-text" title={row.unit.name}>
            {row.unit.name}
          </h2>
          <div className="mt-1 flex flex-wrap items-center gap-1.5">
            <Badge tone={HEALTH_TONE[row.health]} pulse={row.health === "failed"}>
              {HEALTH_LABEL[row.health]}
            </Badge>
            <span className="text-xs text-text-muted">
              {row.unit.active_state} ({row.unit.sub_state})
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
            ["overview", "Overview", 0],
            ["dependencies", "Depends on", detail.data?.dependencies.length ?? 0],
            ["dependents", "Needed by", detail.data?.dependents.length ?? 0],
            ["impact", "Impact", impactCount],
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
        {tab === "overview" ? <Overview row={row} onSelectUnit={onSelectUnit} /> : null}

        {tab !== "overview" && (detail.isPending || detail.error) ? (
          <QueryState
            isPending={detail.isPending}
            error={detail.error}
            onRetry={() => void detail.refetch()}
            rows={4}
          />
        ) : null}

        {tab === "dependencies" && detail.data ? (
          <EdgeList
            edges={detail.data.dependencies}
            side="to"
            emptyText="This unit depends on nothing — it is a leaf of the dependency graph."
            onSelectUnit={onSelectUnit}
          />
        ) : null}

        {tab === "dependents" && detail.data ? (
          <EdgeList
            edges={detail.data.dependents}
            side="from"
            emptyText="Nothing depends on this unit. Stopping it affects no other unit directly."
            onSelectUnit={onSelectUnit}
          />
        ) : null}

        {tab === "impact" && detail.data ? (
          <ImpactPanel impact={detail.data.impact} unit={row.unit.name} onSelectUnit={onSelectUnit} />
        ) : null}
      </div>
    </aside>
  );
}

function Overview({
  row,
  onSelectUnit,
}: {
  row: ServiceRow;
  onSelectUnit: (unit: string) => void;
}) {
  const u = row.unit;

  return (
    <div className="flex flex-col gap-4">
      {/* A degraded verdict is only useful if it names its cause. */}
      {row.failedDependencies.length > 0 ? (
        <div className="surface-alert rounded-lg p-3">
          <div className="mb-1 flex items-center gap-2">
            <AlertTriangle size={13} className="shrink-0 text-danger" aria-hidden="true" />
            <span className="text-xs font-semibold text-text">
              {row.health === "failed" ? "This unit has failed" : "A dependency has failed"}
            </span>
          </div>
          <ul className="flex flex-wrap gap-1.5">
            {row.failedDependencies.map((d) => (
              <li key={d}>
                <button
                  type="button"
                  onClick={() => { onSelectUnit(d); }}
                  className="elev-1 rounded px-1.5 py-0.5 font-mono text-[11px] text-danger hover:bg-surface-hover"
                >
                  {d}
                </button>
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      <dl className="flex flex-col">
        <Field label="Description" value={u.description ?? "—"} />
        <Field label="Active state" value={u.active_state} />
        <Field label="Sub state" value={u.sub_state} />
        <Field label="Load state" value={u.load_state ?? "—"} />
        {/* A running unit that is not enabled vanishes on the next reboot — a
            class of outage invisible until it happens. */}
        <Field
          label="At boot"
          value={
            u.enabled ? (
              "enabled"
            ) : (
              <span className="text-warning" title="This unit will not start on the next boot">
                not enabled
              </span>
            )
          }
        />
        <Field label="Main PID" value={u.main_pid ? String(u.main_pid) : "—"} />
        <Field label="Restarts" value={u.restart_count} />
        <Field
          label="Uptime"
          value={u.uptime_seconds ? formatDuration(u.uptime_seconds) : "—"}
        />
        <Field
          label="Active since"
          value={u.active_since ? new Date(u.active_since).toLocaleString() : "—"}
        />
        <Field label="Memory" value={u.memory_bytes ? formatBytes(u.memory_bytes) : "—"} />
        <Field label="Depends on" value={row.dependencies} />
        <Field label="Needed by" value={row.dependents} />
      </dl>

      <div>
        <div className="mb-1.5 flex items-center justify-between">
          <h3 className="eyebrow">Unit</h3>
          <CopyButton value={u.name} />
        </div>
        <code className="elev-1 block rounded-lg p-2.5 font-mono text-[11px] break-all text-text">
          {u.name}
        </code>
      </div>
    </div>
  );
}

/**
 * Direct edges, grouped by what the relationship means.
 *
 * Requirement edges lead because they are the ones through which failure
 * travels. Ordering edges are listed after and labelled as carrying no
 * requirement — on a typical unit they outnumber requirements three to one,
 * and an undifferentiated list makes the important three invisible.
 */
function EdgeList({
  edges,
  side,
  emptyText,
  onSelectUnit,
}: {
  edges: GraphEdge[];
  side: "from" | "to";
  emptyText: string;
  onSelectUnit: (unit: string) => void;
}) {
  if (edges.length === 0) {
    return <p className="py-6 text-center text-xs leading-relaxed text-text-muted">{emptyText}</p>;
  }

  const requirement = edges.filter((e) => e.class === "requirement");
  const ordering = edges.filter((e) => e.class === "ordering");
  const conflict = edges.filter((e) => e.class === "conflict");

  return (
    <div className="flex flex-col gap-5">
      <Group
        title="Requirements"
        note="Failure travels along these."
        edges={requirement}
        side={side}
        onSelectUnit={onSelectUnit}
      />
      <Group
        title="Ordering only"
        note="Start sequence. Nothing propagates along these."
        edges={ordering}
        side={side}
        onSelectUnit={onSelectUnit}
      />
      <Group
        title="Conflicts"
        note="Starting one stops the other."
        edges={conflict}
        side={side}
        onSelectUnit={onSelectUnit}
      />
    </div>
  );
}

function Group({
  title,
  note,
  edges,
  side,
  onSelectUnit,
}: {
  title: string;
  note: string;
  edges: GraphEdge[];
  side: "from" | "to";
  onSelectUnit: (unit: string) => void;
}) {
  if (edges.length === 0) return null;

  return (
    <section>
      <div className="mb-1.5 flex items-baseline justify-between gap-2">
        <h3 className="eyebrow">{title}</h3>
        <span className="text-[11px] tabular-nums text-text-subtle">{edges.length}</span>
      </div>
      <p className="mb-2 text-[11px] leading-relaxed text-text-subtle">{note}</p>
      <ul className="flex flex-col gap-1.5">
        {edges.map((e) => {
          const unit = side === "to" ? e.to : e.from;
          return (
            <li key={`${e.from}|${e.to}|${e.kind}`}>
              <button
                type="button"
                onClick={() => { onSelectUnit(unit); }}
                className="flex w-full items-center gap-2 rounded px-1.5 py-1 text-left hover:bg-surface-hover"
              >
                <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-text" title={unit}>
                  {unit}
                </span>
                <span
                  className="shrink-0 rounded bg-surface-hover px-1.5 py-0.5 text-[10px] text-text-muted"
                  title={KIND_EXPLANATION[e.kind]}
                >
                  {e.kind.replace("_", " ")}
                </span>
              </button>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

/**
 * The blast radius, hard and soft kept apart.
 *
 * A `Requires` dependent cannot run without this unit. A `Wants` dependent
 * runs without whatever it provides, and everything behind that link is
 * insulated. One combined number would overstate every outage, which is how a
 * dashboard trains people to ignore it.
 */
function ImpactPanel({
  impact,
  unit,
  onSelectUnit,
}: {
  impact: ServiceImpact;
  unit: string;
  onSelectUnit: (unit: string) => void;
}) {
  const total = impact.hard.length + impact.soft.length;

  if (total === 0) {
    return (
      <p className="py-6 text-center text-xs leading-relaxed text-text-muted">
        Nothing depends on {unit} through a requirement. Stopping it would not take anything else
        down — units merely ordered after it are unaffected.
      </p>
    );
  }

  return (
    <div className="flex flex-col gap-5">
      <p className="text-xs leading-relaxed text-text-muted">
        If <span className="font-mono text-text">{unit}</span> fails, {total} unit
        {total === 1 ? "" : "s"} {total === 1 ? "is" : "are"} affected. Units merely ordered after it
        are excluded — ordering carries no requirement.
      </p>

      <ImpactGroup
        title="Cannot run without it"
        tone="danger"
        note="Connected by Requires, BindsTo or PartOf, through an unbroken chain."
        units={impact.hard}
        onSelectUnit={onSelectUnit}
      />
      <ImpactGroup
        title="Degraded but still running"
        tone="warning"
        note="Reached through a Wants link, which the dependent tolerates. Everything behind that link is insulated too."
        units={impact.soft}
        onSelectUnit={onSelectUnit}
      />

      {impact.truncated ? (
        <p className="border-t border-border pt-2.5 text-[11px] leading-relaxed text-text-subtle">
          The traversal hit its node cap, so this list is partial rather than complete.
        </p>
      ) : null}
    </div>
  );
}

function ImpactGroup({
  title,
  tone,
  note,
  units,
  onSelectUnit,
}: {
  title: string;
  tone: Tone;
  note: string;
  units: string[];
  onSelectUnit: (unit: string) => void;
}) {
  if (units.length === 0) return null;

  return (
    <section>
      <div className="mb-1.5 flex items-center gap-2">
        <Badge tone={tone}>{units.length}</Badge>
        <h3 className="text-sm font-medium text-text">{title}</h3>
      </div>
      <p className="mb-2 text-[11px] leading-relaxed text-text-subtle">{note}</p>
      <ul className="flex flex-col gap-1">
        {units.map((u) => (
          <li key={u}>
            <button
              type="button"
              onClick={() => { onSelectUnit(u); }}
              className="w-full truncate rounded px-1.5 py-1 text-left font-mono text-[11px] text-text hover:bg-surface-hover"
              title={u}
            >
              {u}
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3 border-b border-border py-2.5 text-sm last:border-0">
      <dt className="shrink-0 text-text-muted">{label}</dt>
      <dd className="min-w-0 truncate text-right text-text">{value}</dd>
    </div>
  );
}
