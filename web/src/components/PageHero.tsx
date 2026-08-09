import type { ReactNode } from "react";
import { motion } from "framer-motion";
import { textures } from "../lib/assets";
import { DURATION } from "../lib/motion";
import { CountUp } from "./viz/CountUp";

/**
 * The cinematic hero every page opens with.
 *
 * This is where the layering rule is implemented literally, in the order the
 * asset library specifies:
 *
 *   hero artwork → accent gradient → world map → pattern → noise → vignette
 *   → light wash → glass content panel
 *
 * Layers are absolutely-positioned siblings rather than nested wrappers, so
 * the stack reads top to bottom and any one can be removed without
 * disturbing the others. All are `aria-hidden`: the words and numbers carry
 * the meaning, and the imagery is atmosphere.
 *
 * The content sits on glass rather than directly on the artwork. Text over a
 * photograph is a poster; text over a frosted panel that lets the artwork
 * through is an interface — and it keeps contrast predictable no matter
 * which background a page happens to use.
 */
export function PageHero({
  identity,
  premise,
  hero,
  accent,
  pattern,
  map,
  action,
  stats,
  children,
}: {
  /** The page's operational name — "Mission Control", "Fleet Overview". */
  identity: string;
  premise?: string;
  hero: string;
  accent: string;
  pattern: string;
  map?: string | undefined;
  action?: ReactNode;
  /** Live operational figures, shown inside the hero. A hero that only
   *  states a title wastes the most prominent area on the page. */
  stats?: HeroStat[];
  children?: ReactNode;
}) {
  return (
    <motion.section
      initial={{ opacity: 0, y: 10 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: DURATION.slow, ease: [0.22, 1, 0.36, 1] }}
      className="relative mb-8 overflow-hidden rounded-3xl border border-border"
    >
      <div className="relative min-h-[16rem] p-4 sm:p-6">
        {/* 1 — hero artwork */}
        <Layer image={hero} className="bg-cover bg-center opacity-70 dark:opacity-90" />

        {/* 2 — accent gradient, which is what keeps pages from all reading
                as the same shade of violet */}
        <Layer
          image={accent}
          className="bg-cover bg-center opacity-45 mix-blend-screen dark:opacity-65"
        />

        {/* 3 — world map, on fleet-wide views only */}
        {map ? (
          <Layer
            image={map}
            className="left-auto w-2/3 bg-cover bg-center opacity-55 dark:opacity-80"
            mask="linear-gradient(to right, transparent, black 35%)"
          />
        ) : null}

        {/* 4 — the page's pattern, tying the hero to the body below it */}
        <Layer image={pattern} className="opacity-[0.07]" size="300px" repeat />

        {/* 5 — noise, which is what stops large gradients banding */}
        <Layer image={textures.noise} className="opacity-[0.09] mix-blend-overlay" size="256px" repeat />

        {/* 6 — vignette */}
        <Layer image={textures.vignette} className="bg-cover bg-center opacity-35 dark:opacity-45" />

        {/* 7 — a directional wash, so the glass panel always has something
                dark to sit against */}
        <div
          aria-hidden="true"
          className="pointer-events-none absolute inset-0 bg-gradient-to-r from-bg/80 via-bg/30 via-40% to-transparent to-75%"
        />

        {/* 8 — content, on glass */}
        <div className="glass relative w-full rounded-2xl p-5 sm:p-7 lg:w-[62%]">
          <div className="flex flex-wrap items-start justify-between gap-5">
            <div className="min-w-0">
              <h1 className="text-2xl font-semibold tracking-tight text-text sm:text-4xl">
                {identity}
              </h1>
              {premise ? (
                <p className="mt-2 max-w-xl text-sm text-text-muted">{premise}</p>
              ) : null}
            </div>
            {action ? <div className="shrink-0">{action}</div> : null}
          </div>

          {stats && stats.length > 0 ? (
            <dl className="mt-6 grid grid-cols-2 gap-x-6 gap-y-4 border-t border-border/60 pt-5 sm:grid-cols-4">
              {stats.map((s) => (
                <HeroStatItem key={s.label} stat={s} />
              ))}
            </dl>
          ) : null}

          {children}
        </div>
      </div>
    </motion.section>
  );
}

export interface HeroStat {
  label: string;
  value: string;
  /** Colours the value. Absent reads as ordinary. */
  tone?: "default" | "success" | "warning" | "danger";
  hint?: string;
}

const STAT_TONE = {
  default: "text-text",
  success: "text-success",
  warning: "text-warning",
  danger: "text-danger",
} as const;

function HeroStatItem({ stat }: { stat: HeroStat }) {
  return (
    <div className="min-w-0">
      <dt className="eyebrow truncate" title={stat.label}>
        {stat.label}
      </dt>
      <dd
        className={`mt-1 text-xl font-semibold tracking-tight tabular-nums sm:text-2xl ${
          STAT_TONE[stat.tone ?? "default"]
        }`}
      >
        <AnimatedFigure value={stat.value} />
      </dd>
      {stat.hint ? <p className="mt-0.5 truncate text-[11px] text-text-muted">{stat.hint}</p> : null}
    </div>
  );
}

/**
 * Counts a stat up on arrival where it is a plain number.
 *
 * Values like "1/4" or "27.8 GiB" are left alone: animating only the numeric
 * part of a composite string would produce nonsense mid-flight, and the
 * effect is not worth parsing every format Atlas emits.
 */
function AnimatedFigure({ value }: { value: string }) {
  const numeric = /^-?\d+(\.\d+)?$/.test(value) ? Number(value) : null;
  const decimals = value.includes(".") ? (value.split(".")[1]?.length ?? 0) : 0;

  if (numeric === null) return <>{value}</>;
  return <CountUp value={numeric} format={(v) => v.toFixed(decimals)} />;
}

function Layer({
  image,
  className = "",
  mask,
  repeat = false,
  size,
}: {
  image: string;
  className?: string;
  mask?: string;
  repeat?: boolean;
  size?: string;
}) {
  return (
    <div
      aria-hidden="true"
      className={`pointer-events-none absolute inset-0 ${className}`}
      style={{
        backgroundImage: `url(${image})`,
        ...(repeat ? { backgroundSize: size ?? "300px", backgroundRepeat: "repeat" } : {}),
        ...(mask ? { maskImage: mask, WebkitMaskImage: mask } : {}),
      }}
    />
  );
}
