export interface DonutSegment {
  label: string;
  value: number;
  /** A CSS colour — a token like "var(--success)", or a literal. */
  color: string;
}

/**
 * A ring chart for a whole broken into a handful of named parts — the same
 * shape as the health/status breakdowns throughout Atlas's mockups. Built
 * from plain SVG circles rather than a charting library, the same choice
 * [TimeSeriesChart] makes: a handful of arcs is not worth a dependency.
 */
export function DonutChart({
  segments,
  total,
  centerLabel,
  size = 160,
  strokeWidth = 16,
}: {
  segments: DonutSegment[];
  total: number;
  centerLabel: string;
  size?: number;
  strokeWidth?: number;
}) {
  const radius = (size - strokeWidth) / 2;
  const circumference = 2 * Math.PI * radius;
  const center = size / 2;

  let cumulative = 0;

  return (
    <div className="relative" style={{ width: size, height: size }}>
      <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`}>
        <circle
          cx={center}
          cy={center}
          r={radius}
          fill="none"
          stroke="var(--border)"
          strokeWidth={strokeWidth}
        />
        {total > 0
          ? segments.map((seg) => {
              if (seg.value <= 0) return null;
              const fraction = seg.value / total;
              const dash = fraction * circumference;
              const offset = -cumulative * circumference;
              cumulative += fraction;
              return (
                <circle
                  key={seg.label}
                  cx={center}
                  cy={center}
                  r={radius}
                  fill="none"
                  stroke={seg.color}
                  strokeWidth={strokeWidth}
                  strokeDasharray={`${dash} ${circumference - dash}`}
                  strokeDashoffset={offset}
                  strokeLinecap="butt"
                  transform={`rotate(-90 ${center} ${center})`}
                />
              );
            })
          : null}
      </svg>
      <div className="absolute inset-0 flex flex-col items-center justify-center">
        <span className="text-2xl font-semibold tracking-tight text-text">{total}</span>
        <span className="text-xs text-text-muted">{centerLabel}</span>
      </div>
    </div>
  );
}

/** The legend a [DonutChart] is usually paired with — kept separate so a
 *  caller can lay it beside or below the ring as the page needs. */
export function DonutLegend({ segments, total }: { segments: DonutSegment[]; total: number }) {
  return (
    <ul className="flex flex-col gap-2.5">
      {segments.map((seg) => (
        <li key={seg.label} className="flex items-center gap-2 text-sm">
          <span className="h-2.5 w-2.5 shrink-0 rounded-full" style={{ background: seg.color }} />
          <span className="text-text-muted">{seg.label}</span>
          <span className="ml-auto font-medium text-text">{seg.value.toLocaleString()}</span>
          <span className="w-12 text-right text-xs text-text-muted">
            {total > 0 ? `${((seg.value / total) * 100).toFixed(1)}%` : "—"}
          </span>
        </li>
      ))}
    </ul>
  );
}
