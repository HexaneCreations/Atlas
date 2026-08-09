import { Layers, Package } from "lucide-react";
import type { ProjectSummary } from "./containerModel";
import { projectTone } from "./containerModel";
import { Card, CardHeader } from "../../components/Card";
import { Badge, type Tone } from "../../components/Badge";
import { formatBytes, formatValue } from "../../format";

/**
 * Compose projects, each summarised from its own members.
 *
 * Every figure here is computed from the containers in the project — Docker
 * has no notion of project health, and inferring one from a project-level
 * metric would be inventing a number. The card states the denominator wherever
 * a figure covers only part of the project, which for resource use it almost
 * always does: only running containers report usage.
 *
 * Standalone containers get a card of their own rather than being hidden. On
 * most hosts they are the majority, and a projects view that omits them
 * describes a fraction of what is running while looking complete.
 */
export function ProjectExplorer({
  projects,
  selected,
  onSelect,
}: {
  projects: ProjectSummary[];
  selected: string | null;
  onSelect: (name: string | null) => void;
}) {
  return (
    <Card level="flat" className="mb-6">
      <CardHeader
        title="Projects"
        action={
          <span className="text-xs text-text-muted">
            {projects.length} group{projects.length === 1 ? "" : "s"} · click to filter the explorer
          </span>
        }
      />

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3">
        {projects.map((p) => (
          <ProjectCard
            key={p.name}
            project={p}
            selected={selected === p.name}
            onSelect={() => { onSelect(selected === p.name ? null : p.name); }}
          />
        ))}
      </div>
    </Card>
  );
}

const TONE_LABEL: Record<ReturnType<typeof projectTone>, { tone: Tone; label: string }> = {
  danger: { tone: "danger", label: "unhealthy" },
  warning: { tone: "warning", label: "degraded" },
  success: { tone: "success", label: "healthy" },
  neutral: { tone: "neutral", label: "stopped" },
};

function ProjectCard({
  project: p,
  selected,
  onSelect,
}: {
  project: ProjectSummary;
  selected: boolean;
  onSelect: () => void;
}) {
  const tone = TONE_LABEL[projectTone(p)];

  return (
    <button
      type="button"
      onClick={onSelect}
      aria-pressed={selected}
      className={`rounded-lg border p-4 text-left transition-colors ${
        selected
          ? "border-primary bg-primary/5"
          : p.standalone
            ? "border-dashed border-border hover:bg-surface-hover"
            : "border-border hover:bg-surface-hover"
      }`}
    >
      <div className="mb-3 flex flex-wrap items-center justify-between gap-x-2 gap-y-1.5">
        <span className="flex min-w-0 items-center gap-2">
          {p.standalone ? (
            <Package size={14} className="shrink-0 text-text-subtle" aria-hidden="true" />
          ) : (
            <Layers size={14} className="shrink-0 text-text-muted" aria-hidden="true" />
          )}
          <span
            className={`truncate text-sm font-medium ${p.standalone ? "text-text-subtle" : "text-text"}`}
            title={p.standalone ? "Containers not managed by a Compose project." : p.name}
          >
            {p.name}
          </span>
        </span>
        <Badge tone={tone.tone} pulse={p.unhealthy > 0}>
          {tone.label}
        </Badge>
      </div>

      {/* Lifecycle is stated as counts rather than a single "status", because
          running / created / stopped are different situations and a project can
          be several at once. */}
      <div className="mb-3 flex flex-wrap items-baseline gap-x-3 gap-y-1 text-xs">
        <span className="text-2xl font-semibold tabular-nums text-text">{p.total}</span>
        <span className="text-text-muted">container{p.total === 1 ? "" : "s"}</span>
        <span className="ml-auto flex flex-wrap gap-2 tabular-nums">
          {p.running > 0 ? <span className="text-success">{p.running} running</span> : null}
          {p.restarting > 0 ? <span className="text-warning">{p.restarting} restarting</span> : null}
          {p.paused > 0 ? <span className="text-warning">{p.paused} paused</span> : null}
          {p.created > 0 ? <span className="text-text-muted">{p.created} created</span> : null}
          {p.stopped > 0 ? <span className="text-text-subtle">{p.stopped} stopped</span> : null}
        </span>
      </div>

      <dl className="flex flex-col gap-1 border-t border-border pt-2.5 text-xs">
        <Row label="Images" value={String(p.images.length)} />
        {p.restarts > 0 ? <Row label="Restarts" value={String(p.restarts)} tone="warning" /> : null}
        {p.abnormalExits > 0 ? (
          <Row label="Abnormal exits" value={String(p.abnormalExits)} tone="warning" />
        ) : null}
        {p.unhealthy > 0 ? (
          <Row label="Failing health" value={String(p.unhealthy)} tone="danger" />
        ) : null}

        {p.reporting === 0 ? (
          <p className="mt-1 text-[11px] leading-relaxed text-text-subtle">
            No member is running, so there is no live resource use to report.
          </p>
        ) : (
          <>
            <Row label="CPU" value={p.cpu !== undefined ? formatValue(p.cpu, "percent") : "—"} />
            <Row label="Memory" value={p.memory !== undefined ? formatBytes(p.memory) : "—"} />
            <p className="mt-1 text-[11px] text-text-subtle">
              Summed across {p.reporting} of {p.total} — only running containers report usage.
            </p>
          </>
        )}
      </dl>
    </button>
  );
}

function Row({
  label,
  value,
  tone = "muted",
}: {
  label: string;
  value: string;
  tone?: "muted" | "warning" | "danger";
}) {
  const color =
    tone === "danger" ? "text-danger" : tone === "warning" ? "text-warning" : "text-text";
  return (
    <div className="flex items-center justify-between gap-2">
      <dt className="text-text-muted">{label}</dt>
      <dd className={`font-medium tabular-nums ${color}`}>{value}</dd>
    </div>
  );
}
