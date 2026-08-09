import type { LucideIcon } from "lucide-react";
import { ArrowDownRight, ArrowRight, ArrowUpRight } from "lucide-react";
import { Sparkline } from "../Chart";

const ICON_TONE = {
  primary: "bg-primary/10 text-primary",
  success: "bg-success/10 text-success",
  warning: "bg-warning/10 text-warning",
  danger: "bg-danger/10 text-danger",
  info: "bg-info/10 text-info",
  neutral: "bg-surface-hover text-text-muted",
} as const;

/**
 * A metric card that carries context, not just a number.
 *
 * A bare figure answers "what is it" and nothing else. This adds the two
 * things that make a number actionable: where it has been (the sparkline)
 * and which way it is going (the delta). "Memory 79%" is a fact; "79%, up
 * six points over the last hour" is a reason to look closer.
 *
 * The delta's colour is deliberately not tied to its direction. Rising CPU is
 * bad and rising free space is good, so the caller states the intent through
 * `higherIsBetter` rather than the component assuming that up is red.
 */
export function TrendCard({
  label,
  value,
  icon: Icon,
  iconTone = "primary",
  history,
  format,
  higherIsBetter = false,
  caption,
}: {
  label: string;
  value: string;
  icon?: LucideIcon;
  iconTone?: keyof typeof ICON_TONE;
  /** Oldest to newest. Two or more points draw a sparkline and a delta. */
  history?: number[];
  format?: ((v: number) => string) | undefined;
  higherIsBetter?: boolean;
  caption?: string;
}) {
  const delta = computeDelta(history);

  return (
    <div className="elev-2 lift relative overflow-hidden rounded-xl p-5">
      <div className="mb-2 flex items-start justify-between gap-2">
        <h3 className="eyebrow truncate" title={label}>
          {label}
        </h3>
        {Icon ? (
          <span
            className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-lg ${ICON_TONE[iconTone]}`}
          >
            <Icon size={16} />
          </span>
        ) : null}
      </div>

      <p className="text-3xl font-semibold tracking-tight tabular-nums text-text">{value}</p>

      <div className="mt-2 flex min-h-[1.25rem] items-center gap-2 text-xs">
        {delta ? (
          <DeltaBadge delta={delta} higherIsBetter={higherIsBetter} format={format} />
        ) : caption ? (
          <span className="text-text-muted">{caption}</span>
        ) : null}
      </div>

      {history && history.length > 1 ? (
        <div className="mt-3 -mb-1 opacity-70">
          <Sparkline points={history} />
        </div>
      ) : null}
    </div>
  );
}

interface Delta {
  change: number;
  direction: "up" | "down" | "flat";
}

/**
 * Compares the latest reading against the start of the window.
 *
 * A change under half a percent of the range is reported as flat. Live
 * metrics jitter constantly, and an arrow that flips direction every poll
 * trains people to ignore it.
 */
function computeDelta(history: number[] | undefined): Delta | null {
  if (!history || history.length < 2) return null;

  const first = history[0];
  const last = history[history.length - 1];
  if (first === undefined || last === undefined) return null;

  const change = last - first;
  const span = Math.max(...history) - Math.min(...history);
  if (span === 0 || Math.abs(change) < span * 0.005) {
    return { change: 0, direction: "flat" };
  }
  return { change, direction: change > 0 ? "up" : "down" };
}

function DeltaBadge({
  delta,
  higherIsBetter,
  format,
}: {
  delta: Delta;
  higherIsBetter: boolean;
  format?: ((v: number) => string) | undefined;
}) {
  if (delta.direction === "flat") {
    return (
      <span className="flex items-center gap-1 text-text-muted">
        <ArrowRight size={12} />
        steady
      </span>
    );
  }

  const good = delta.direction === "up" ? higherIsBetter : !higherIsBetter;
  const Icon = delta.direction === "up" ? ArrowUpRight : ArrowDownRight;
  const magnitude = Math.abs(delta.change);

  return (
    <span className={`flex items-center gap-1 font-medium ${good ? "text-success" : "text-warning"}`}>
      <Icon size={12} />
      {format ? format(magnitude) : magnitude.toFixed(1)}
      <span className="font-normal text-text-muted">this window</span>
    </span>
  );
}
