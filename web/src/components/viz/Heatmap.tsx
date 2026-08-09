import { useMemo } from "react";

export interface HeatmapRow {
  key: string;
  label: string;
  /** Ordered oldest-to-newest. Gaps are represented by null, not zero. */
  values: (number | null)[];
}

/**
 * A row-per-subject, column-per-interval heatmap.
 *
 * This is the shape that answers "which of these was busy, and when" — a
 * question a stack of twenty line charts answers badly and a table not at
 * all. Intensity is scaled against the whole grid rather than per row, so a
 * quiet process stays visibly quiet instead of being normalised up into
 * looking as busy as the loudest one.
 *
 * A null cell renders as the empty track, not as zero. A process that did not
 * exist during an interval and a process that used no CPU are different
 * facts, and colouring them identically would invent history.
 */
export function Heatmap({
  rows,
  format,
  emptyLabel = "No data in this range.",
}: {
  rows: HeatmapRow[];
  format: (v: number) => string;
  emptyLabel?: string;
}) {
  const max = useMemo(() => {
    let m = 0;
    for (const r of rows) {
      for (const v of r.values) if (v !== null && v > m) m = v;
    }
    return m || 1;
  }, [rows]);

  if (rows.length === 0) {
    return <p className="py-8 text-center text-sm text-text-muted">{emptyLabel}</p>;
  }

  return (
    <div className="flex flex-col gap-1.5">
      {rows.map((row) => (
        <div key={row.key} className="flex items-center gap-3">
          <span className="w-32 shrink-0 truncate text-xs text-text-muted" title={row.label}>
            {row.label}
          </span>
          <div className="flex flex-1 gap-px">
            {row.values.map((v, i) => (
              <span
                key={i}
                className="h-5 flex-1 rounded-[2px] transition-colors"
                title={v === null ? "no sample" : format(v)}
                style={{
                  background:
                    v === null
                      ? "var(--surface-hover)"
                      : `color-mix(in oklab, var(--primary) ${intensity(v, max)}%, transparent)`,
                }}
              />
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}

/**
 * Maps a value onto a visible intensity.
 *
 * Square-rooted rather than linear: on a host where one process dominates,
 * a linear scale renders everything else as indistinguishable near-black,
 * which hides exactly the mid-range activity worth noticing. The floor keeps
 * a real-but-small sample from vanishing into the background entirely.
 */
function intensity(value: number, max: number): number {
  const ratio = Math.sqrt(Math.max(value, 0) / max);
  return Math.round(8 + ratio * 92);
}
