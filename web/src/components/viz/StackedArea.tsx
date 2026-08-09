import { useId, useMemo, useState } from "react";
import { formatTime } from "../../format";

export interface StackedSeries {
  key: string;
  label: string;
  /** Any CSS colour, including a `var(--token)`, so the palette follows the theme. */
  color: string;
}

const PADDING = { top: 8, right: 8, bottom: 20, left: 46 };
const WIDTH = 600; // viewBox width; the SVG scales to its container

interface Model {
  rows: { time: number; values: number[]; tops: number[]; total: number }[];
  drawn: number[];
  maxValue: number;
  plotW: number;
  plotH: number;
  x: (t: number) => number;
  y: (v: number) => number;
}

/**
 * The stroked top edge of one band, broken wherever the band has no height.
 *
 * Without the break, a band that is zero for part of the range still draws its
 * outline along the top of the band below — so the topmost series paints its
 * colour over the whole stack and the chart claims a spike belongs to the
 * wrong state. Each run of non-zero samples is stroked separately, extended by
 * one sample either side so a single-sample spike closes down to the baseline
 * instead of rendering as an invisible point.
 */
function edgePath(model: Model, band: number): string {
  const active = model.rows.map(
    (r, i) =>
      (r.values[band] ?? 0) > 0 ||
      ((model.rows[i - 1]?.values[band] ?? 0) > 0) ||
      ((model.rows[i + 1]?.values[band] ?? 0) > 0),
  );

  const parts: string[] = [];
  let open = false;
  model.rows.forEach((r, i) => {
    if (!active[i]) {
      open = false;
      return;
    }
    const point = `${model.x(r.time)},${model.y(r.tops[band] ?? 0)}`;
    parts.push(`${open ? "L" : "M"}${point}`);
    open = true;
  });

  return parts.join(" ");
}

/**
 * A stacked area chart, for compositions that add up to a meaningful whole —
 * processes by state, containers by status over time.
 *
 * Drawn as inline SVG, like every other chart in Atlas. This was briefly a
 * Recharts chart, on the reasoning that stacking and shared tooltips are real
 * arithmetic worth importing. The arithmetic turned out to be a running sum;
 * the import turned out to be 113 kB gzipped for one panel on one page, more
 * than the rest of the application put together. A running sum is cheaper to
 * maintain than a dependency that size.
 *
 * Bands are drawn in the order given, first at the bottom. Callers are
 * expected to put the largest band first so the shape reads as a mass with
 * detail on top rather than a thin line riding a thick one.
 */
export function StackedArea({
  data,
  series,
  height = 200,
  format,
}: {
  /** Each row: `{ time: iso, [seriesKey]: number }`. */
  data: Record<string, string | number>[];
  series: StackedSeries[];
  height?: number;
  format?: (v: number) => string;
}) {
  const gradientId = useId();
  const [hoverIndex, setHoverIndex] = useState<number | null>(null);

  const model = useMemo(() => {
    if (data.length === 0 || series.length === 0) return null;

    // One cumulative row per sample: bounds[i][j] is the top edge of band j.
    // Computing it once here keeps the path builders below to pure lookups.
    const rows = data.map((row) => {
      const time = new Date(String(row.time)).getTime();
      const values = series.map((s) => Number(row[s.key] ?? 0));
      let running = 0;
      const tops = values.map((v) => (running += v));
      return { time, values, tops, total: running };
    });

    // A band that is zero at every sample has the same top edge as the band
    // below it, so drawing it paints its stroke over the real top of the
    // stack — with four of five states empty, the visible outline was the
    // colour of a series that contributes nothing. Empty bands are not drawn.
    // They stay in the tooltip: "zero zombies" is worth reading.
    const drawn = series
      .map((_, band) => band)
      .filter((band) => rows.some((r) => (r.values[band] ?? 0) > 0));

    const times = rows.map((r) => r.time);
    const minTime = Math.min(...times);
    const maxTime = Math.max(...times);
    // Headroom so the peak is not flush against the top edge, and a floor so an
    // all-zero range draws flat at the bottom rather than dividing by zero.
    const peak = Math.max(...rows.map((r) => r.total));
    const maxValue = peak > 0 ? peak * 1.15 : 1;

    const plotW = WIDTH - PADDING.left - PADDING.right;
    const plotH = height - PADDING.top - PADDING.bottom;
    const spanTime = maxTime - minTime || 1;

    return {
      rows,
      drawn,
      maxValue,
      plotW,
      plotH,
      x: (t: number) => PADDING.left + ((t - minTime) / spanTime) * plotW,
      y: (v: number) => PADDING.top + plotH - (v / maxValue) * plotH,
    };
  }, [data, series, height]);

  if (!model) {
    return <p className="py-10 text-center text-sm text-text-muted">No data in this range.</p>;
  }

  const fmt = format ?? ((v: number) => String(Math.round(v)));
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => model.maxValue * f);
  const floor = PADDING.top + model.plotH;

  function handleMove(event: React.MouseEvent<SVGSVGElement>) {
    if (!model) return;
    const rect = event.currentTarget.getBoundingClientRect();
    const svgX = ((event.clientX - rect.left) / rect.width) * WIDTH;

    let best = 0;
    let bestDistance = Infinity;
    model.rows.forEach((r, i) => {
      const d = Math.abs(model.x(r.time) - svgX);
      if (d < bestDistance) {
        bestDistance = d;
        best = i;
      }
    });
    setHoverIndex(best);
  }

  const hovered = hoverIndex === null ? undefined : model.rows[hoverIndex];

  return (
    <div className="relative">
      <svg
        viewBox={`0 0 ${WIDTH} ${height}`}
        className="chart__svg"
        preserveAspectRatio="none"
        role="img"
        aria-label={`${series.map((s) => s.label).join(", ")} over time`}
        onMouseMove={handleMove}
        onMouseLeave={() => { setHoverIndex(null); }}
      >
        <defs>
          {series.map((s) => (
            <linearGradient key={s.key} id={`${gradientId}-${s.key}`} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={s.color} stopOpacity="0.55" />
              <stop offset="100%" stopColor={s.color} stopOpacity="0.08" />
            </linearGradient>
          ))}
        </defs>

        {ticks.map((value, i) => {
          const y = model.y(value);
          return (
            <g key={i}>
              <line x1={PADDING.left} x2={WIDTH - PADDING.right} y1={y} y2={y} className="chart__grid" />
              <text x={PADDING.left - 6} y={y + 3} className="chart__tick" textAnchor="end">
                {fmt(value)}
              </text>
            </g>
          );
        })}

        {model.drawn.map((band) => {
          const s = series[band];
          if (!s) return null;

          // Upper edge left-to-right, then the band below it right-to-left.
          // Band 0 closes along the floor.
          const top = model.rows
            .map((r, i) => `${i === 0 ? "M" : "L"}${model.x(r.time)},${model.y(r.tops[band] ?? 0)}`)
            .join(" ");

          const bottom =
            band === 0
              ? `L${model.x(model.rows[model.rows.length - 1]?.time ?? 0)},${floor} L${model.x(model.rows[0]?.time ?? 0)},${floor}`
              : [...model.rows]
                  .reverse()
                  .map((r) => `L${model.x(r.time)},${model.y(r.tops[band - 1] ?? 0)}`)
                  .join(" ");

          return (
            <g key={s.key}>
              <path d={`${top} ${bottom} Z`} fill={`url(#${gradientId}-${s.key})`} />
              <path
                d={edgePath(model, band)}
                fill="none"
                stroke={s.color}
                strokeWidth={1.5}
                strokeLinecap="round"
                strokeLinejoin="round"
                vectorEffect="non-scaling-stroke"
              />
            </g>
          );
        })}

        {hovered ? (
          <line
            x1={model.x(hovered.time)}
            x2={model.x(hovered.time)}
            y1={PADDING.top}
            y2={floor}
            className="chart__crosshair"
          />
        ) : null}

        <line
          x1={PADDING.left}
          x2={WIDTH - PADDING.right}
          y1={floor}
          y2={floor}
          className="chart__axis"
        />
      </svg>

      {hovered ? (
        <div
          role="status"
          className="absolute top-0 right-0 flex flex-col gap-1 rounded-lg border border-border bg-bg px-3 py-2 text-xs shadow-lg"
        >
          <span className="tabular-nums text-text-muted">
            {formatTime(new Date(hovered.time).toISOString())}
          </span>
          {/* Top band first, so the list reads in the order the stack does. */}
          {series
            .map((s, band) => ({ s, band }))
            .reverse()
            .map(({ s, band }) => (
              <span key={s.key} className="flex items-center gap-1.5">
                <span
                  aria-hidden="true"
                  className="h-2 w-2 shrink-0 rounded-sm"
                  style={{ background: s.color }}
                />
                {s.label}
                <strong className="ml-auto tabular-nums">{fmt(hovered.values[band] ?? 0)}</strong>
              </span>
            ))}
          {series.length > 1 ? (
            <span className="mt-0.5 flex items-center gap-1.5 border-t border-border pt-1 text-text-muted">
              Total
              <strong className="ml-auto tabular-nums text-text">{fmt(hovered.total)}</strong>
            </span>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
