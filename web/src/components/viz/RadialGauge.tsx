import { motion } from "framer-motion";

/**
 * A capacity ring.
 *
 * Reads as a fill level at a glance in a way a number cannot: 93% and 39%
 * are three characters apart as text and unmistakable as arcs. Used wherever
 * a bounded resource is being reported — disk capacity, inode usage, a
 * quota.
 *
 * The arc animates from empty on mount and then holds. It does not re-animate
 * when the value updates, because a ring that redraws itself every five
 * seconds is a distraction on a page an operator is reading, not a delight.
 */
export function RadialGauge({
  value,
  label,
  caption,
  size = 132,
  thickness = 10,
}: {
  /** Percentage, 0–100. */
  value: number;
  /** The big number in the middle. Defaults to the rounded percentage. */
  label?: string;
  caption?: string;
  size?: number;
  thickness?: number;
}) {
  const clamped = Math.max(0, Math.min(value, 100));
  const radius = (size - thickness) / 2;
  const circumference = 2 * Math.PI * radius;
  const centre = size / 2;

  // Thresholds match the rest of Atlas: 75% warns, 90% is critical. A disk
  // does not become urgent gradually — it becomes urgent at the point where
  // the time left to react gets short.
  const stroke =
    clamped >= 90 ? "var(--danger)" : clamped >= 75 ? "var(--warning)" : "var(--primary)";

  return (
    <div className="flex flex-col items-center">
      <div className="relative" style={{ width: size, height: size }}>
        <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} className="-rotate-90">
          <circle
            cx={centre}
            cy={centre}
            r={radius}
            fill="none"
            stroke="var(--border)"
            strokeWidth={thickness}
          />
          <motion.circle
            cx={centre}
            cy={centre}
            r={radius}
            fill="none"
            stroke={stroke}
            strokeWidth={thickness}
            strokeLinecap="round"
            strokeDasharray={circumference}
            initial={{ strokeDashoffset: circumference }}
            animate={{ strokeDashoffset: circumference * (1 - clamped / 100) }}
            transition={{ duration: 0.7, ease: [0.22, 1, 0.36, 1] }}
            style={{ filter: `drop-shadow(0 0 6px ${stroke})` }}
          />
        </svg>
        <div className="absolute inset-0 flex flex-col items-center justify-center">
          <span className="text-xl font-semibold tabular-nums text-text">
            {label ?? `${Math.round(clamped)}%`}
          </span>
        </div>
      </div>
      {caption ? (
        <span className="mt-2 max-w-[10rem] truncate text-center text-xs text-text-muted" title={caption}>
          {caption}
        </span>
      ) : null}
    </div>
  );
}
