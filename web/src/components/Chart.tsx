import { useId, useMemo, useState } from "react";
import type { MetricUnit, Series } from "../api/types";
import { formatTime, formatValue } from "../format";

/**
 * Time-series charts, drawn as inline SVG.
 *
 * No charting library. The shapes Atlas needs are a line, an area, and a
 * sparkline; a library would add a large dependency and its own opinions about
 * axes and tooltips for marks that are a few dozen lines of path arithmetic.
 *
 * Colours come from CSS custom properties defined in styles.css rather than
 * being hard-coded here, so the light and dark palettes — each validated
 * against its own surface — swap in one place.
 */

interface ChartSeries {
  label: string;
  points: { time: number; value: number }[];
  /** Palette slot. 1 and 2 are the validated two-series pair. */
  slot: 1 | 2;
}

const PADDING = { top: 8, right: 8, bottom: 20, left: 44 };

/** Converts an API series into plot-ready points. */
export function toChartSeries(series: Series, label: string, slot: 1 | 2): ChartSeries {
  return {
    label,
    slot,
    points: series.points.map((p) => ({ time: new Date(p.time).getTime(), value: p.value })),
  };
}

interface TimeSeriesChartProps {
  series: ChartSeries[];
  unit: MetricUnit;
  height?: number;
  /** Fixes the y-axis to 0–100, so a percentage chart never auto-scales and
   *  makes a quiet host look busy. */
  percentScale?: boolean;
  /** Fills under a single line. Only meaningful for one series. */
  area?: boolean;
}

export function TimeSeriesChart({
  series,
  unit,
  height = 160,
  percentScale = false,
  area = false,
}: TimeSeriesChartProps) {
  const gradientId = useId();
  const [hover, setHover] = useState<{ x: number; index: number } | null>(null);

  const width = 600; // viewBox width; the SVG scales to its container

  const scales = useMemo(() => {
    const all = series.flatMap((s) => s.points);
    if (all.length === 0) return null;

    const times = all.map((p) => p.time);
    const minTime = Math.min(...times);
    const maxTime = Math.max(...times);

    let minValue = 0;
    let maxValue = percentScale ? 100 : Math.max(...all.map((p) => p.value));
    if (!percentScale) {
      // Headroom so the peak is not flush against the top edge, and a floor so
      // an all-zero series draws a flat line at the bottom rather than
      // dividing by zero.
      maxValue = maxValue > 0 ? maxValue * 1.15 : 1;
      minValue = 0;
    }

    const plotW = width - PADDING.left - PADDING.right;
    const plotH = height - PADDING.top - PADDING.bottom;
    const spanTime = maxTime - minTime || 1;
    const spanValue = maxValue - minValue || 1;

    return {
      minTime,
      maxTime,
      maxValue,
      plotW,
      plotH,
      x: (t: number) => PADDING.left + ((t - minTime) / spanTime) * plotW,
      y: (v: number) => PADDING.top + plotH - ((v - minValue) / spanValue) * plotH,
    };
  }, [series, height, percentScale]);

  if (!scales) {
    return <p className="py-10 text-center text-sm text-text-muted">No data in this range.</p>;
  }

  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => scales.maxValue * f);

  // The hovered index is resolved against the longest series, so a crosshair
  // lands on a real sample rather than an interpolated position.
  const reference = series.reduce((a, b) => (b.points.length > a.points.length ? b : a));

  function handleMove(event: React.MouseEvent<SVGSVGElement>) {
    const rect = event.currentTarget.getBoundingClientRect();
    const svgX = ((event.clientX - rect.left) / rect.width) * width;
    if (!scales) return;

    let best = 0;
    let bestDistance = Infinity;
    reference.points.forEach((p, i) => {
      const d = Math.abs(scales.x(p.time) - svgX);
      if (d < bestDistance) {
        bestDistance = d;
        best = i;
      }
    });
    setHover({ x: svgX, index: best });
  }

  const hovered = hover ? reference.points[hover.index] : undefined;

  function areaPath(
    s: ChartSeries,
    sc: NonNullable<typeof scales>,
    enabled: boolean,
    id: string,
  ) {
    const first = s.points[0];
    const last = s.points[s.points.length - 1];
    if (!enabled || !first || !last || s.points.length < 2) return null;

    const line = s.points
      .map((p, i) => `${i === 0 ? "M" : "L"}${sc.x(p.time)},${sc.y(p.value)}`)
      .join(" ");
    const floor = PADDING.top + sc.plotH;
    return (
      <path
        d={`${line} L${sc.x(last.time)},${floor} L${sc.x(first.time)},${floor} Z`}
        fill={`url(#${id}-${s.slot})`}
      />
    );
  }

  return (
    <div className="relative">
      <svg
        viewBox={`0 0 ${width} ${height}`}
        className="chart__svg"
        preserveAspectRatio="none"
        role="img"
        aria-label={`${series.map((s) => s.label).join(" and ")} over time`}
        onMouseMove={handleMove}
        onMouseLeave={() => { setHover(null); }}
      >
        <defs>
          {series.map((s) => (
            <linearGradient key={s.slot} id={`${gradientId}-${s.slot}`} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={`var(--series-${s.slot})`} stopOpacity="0.28" />
              <stop offset="100%" stopColor={`var(--series-${s.slot})`} stopOpacity="0.02" />
            </linearGradient>
          ))}
        </defs>

        {/* Grid and axis labels are recessive: they orient the reader without
            competing with the data. */}
        {ticks.map((value, i) => {
          const y = scales.y(value);
          return (
            <g key={i}>
              <line
                x1={PADDING.left}
                x2={width - PADDING.right}
                y1={y}
                y2={y}
                className="chart__grid"
              />
              <text x={PADDING.left - 6} y={y + 3} className="chart__tick" textAnchor="end">
                {formatValue(value, unit)}
              </text>
            </g>
          );
        })}

        {series.map((s) => {
          if (s.points.length === 0) return null;
          const line = s.points
            .map((p, i) => `${i === 0 ? "M" : "L"}${scales.x(p.time)},${scales.y(p.value)}`)
            .join(" ");

          return (
            <g key={s.slot}>
              {areaPath(s, scales, area, gradientId)}
              <path
                d={line}
                fill="none"
                stroke={`var(--series-${s.slot})`}
                strokeWidth={2}
                strokeLinecap="round"
                strokeLinejoin="round"
                vectorEffect="non-scaling-stroke"
              />
            </g>
          );
        })}

        {hover && hovered ? (
          <g>
            <line
              x1={scales.x(hovered.time)}
              x2={scales.x(hovered.time)}
              y1={PADDING.top}
              y2={PADDING.top + scales.plotH}
              className="chart__crosshair"
            />
            {series.map((s) => {
              const p = s.points[hover.index];
              if (!p) return null;
              return (
                <circle
                  key={s.slot}
                  cx={scales.x(p.time)}
                  cy={scales.y(p.value)}
                  r={4}
                  fill={`var(--series-${s.slot})`}
                  className="chart__marker"
                />
              );
            })}
          </g>
        ) : null}

        <line
          x1={PADDING.left}
          x2={width - PADDING.right}
          y1={PADDING.top + scales.plotH}
          y2={PADDING.top + scales.plotH}
          className="chart__axis"
        />
      </svg>

      {hover && hovered ? (
        <div
          role="status"
          className="absolute top-0 right-0 flex flex-col gap-1 rounded-lg border border-border bg-bg px-3 py-2 text-xs shadow-lg"
        >
          <span className="text-text-muted tabular-nums">
            {formatTime(new Date(hovered.time).toISOString())}
          </span>
          {series.map((s) => {
            const p = s.points[hover.index];
            if (!p) return null;
            return (
              <span key={s.slot} className="flex items-center gap-1.5">
                <span
                  aria-hidden="true"
                  className="h-0.5 w-2.5 shrink-0 rounded-full"
                  style={{ background: `var(--series-${s.slot})` }}
                />
                {s.label}
                <strong className="ml-auto tabular-nums">{formatValue(p.value, unit)}</strong>
              </span>
            );
          })}
        </div>
      ) : null}
    </div>
  );
}

/**
 * A sparkline for a stat tile: trend only, no axes.
 *
 * The headline number answers "what is it now"; this answers "and is that
 * normal", which is the question a single number cannot.
 */
export function Sparkline({ points, slot = 1 }: { points: number[]; slot?: 1 | 2 }) {
  if (points.length < 2) return null;

  const w = 100;
  const h = 24;
  const max = Math.max(...points, 0.0001);
  const min = Math.min(...points, 0);
  const span = max - min || 1;

  const d = points
    .map((v, i) => {
      const x = (i / (points.length - 1)) * w;
      const y = h - ((v - min) / span) * h;
      return `${i === 0 ? "M" : "L"}${x.toFixed(2)},${y.toFixed(2)}`;
    })
    .join(" ");

  return (
    <svg viewBox={`0 0 ${w} ${h}`} className="sparkline" preserveAspectRatio="none" aria-hidden="true">
      <path
        d={d}
        fill="none"
        stroke={`var(--series-${slot})`}
        strokeWidth={1.5}
        strokeLinecap="round"
        strokeLinejoin="round"
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}
