import type { ReactNode } from "react";
import { motion } from "framer-motion";
import { fadeUp } from "../lib/motion";
import { patterns, textures } from "../lib/assets";

/**
 * The "there is nothing to show" state.
 *
 * Three conditions produce an empty panel and they are not the same fact.
 * Rendering them identically is how a monitoring tool tells its worst lie —
 * a source it cannot reach looks exactly like a source reporting all-clear.
 *
 *   `empty`       nothing exists yet, and that is a healthy answer.
 *   `filtered`    things exist; this filter matched none of them.
 *   `unavailable` Atlas cannot see the source. Not the same as "none".
 *
 * The tone follows the condition: `unavailable` is amber-keyed and states
 * what it could not reach, because a quiet dashboard that should be alarming
 * is the failure mode this component exists to prevent.
 *
 * Layering follows the asset library's order — pattern, gradient bloom, noise,
 * then glass, then content. The imagery is decorative and marked `aria-hidden`
 * throughout: the heading and body carry the entire message, so a failed image
 * load or a screen reader loses nothing.
 */

export type EmptyKind = "empty" | "filtered" | "unavailable";

const PATTERN: Record<EmptyKind, string> = {
  // The library assigns dots to empty states and onboarding; an unreachable
  // source is a systems condition rather than an empty list, so it takes the
  // infrastructure pattern instead.
  empty: patterns.dots,
  filtered: patterns.dots,
  unavailable: patterns.hex,
};

/** Fades the decorative layers out before they reach the panel edge, so the
 *  texture has no boundary to read as a rectangle. */
const FEATHER = "radial-gradient(ellipse 70% 70% at 50% 42%, #000 0%, transparent 75%)";

const EYEBROW: Record<EmptyKind, string> = {
  empty: "Nothing to show",
  filtered: "No matches",
  unavailable: "Source unavailable",
};

export function EmptyState({
  art,
  title,
  description,
  hint,
  action,
  kind = "empty",
  compact = false,
}: {
  /** A URL from lib/assets. Omitted renders a text-only state. */
  art?: string;
  title: string;
  description?: string;
  /** One quiet line on *why* it is empty — the collection interval, the
   *  permission required, the socket that was not there. This is usually the
   *  sentence that stops someone filing a bug. */
  hint?: string;
  action?: ReactNode;
  kind?: EmptyKind;
  /** Inline within a card or table. Drops the glass panel — glass inside
   *  glass reads as a smudge — and scales the art down. */
  compact?: boolean;
}) {
  const unavailable = kind === "unavailable";

  return (
    <motion.div
      variants={fadeUp}
      initial="hidden"
      animate="visible"
      className={`relative isolate flex flex-col items-center justify-center overflow-hidden rounded-xl text-center ${
        compact ? "px-6 py-10" : "px-8 py-16"
      }`}
    >
      {/* 1 · tiling pattern, feathered.
             The pattern assets are dark artwork; at a low opacity over a
             white card they stop reading as texture and become a flat tint
             with a hard rectangular edge — which looks like a broken image.
             The radial mask removes the edge, so the texture is strongest
             behind the illustration and gone by the panel border. */}
      <span
        aria-hidden="true"
        className="pointer-events-none absolute inset-0 -z-30 opacity-[0.05] dark:opacity-[0.08]"
        style={{
          backgroundImage: `url(${PATTERN[kind]})`,
          backgroundSize: "300px",
          maskImage: FEATHER,
          WebkitMaskImage: FEATHER,
        }}
      />
      {/* 2 · gradient bloom, centred behind the illustration.
             A CSS radial rather than the library's radial-glow asset: that
             file is authored for a black hero, and at any opacity over a
             white panel it renders as a grey rectangle. This is the same
             design intent — a soft brand-coloured bloom — realised in a way
             that survives both themes. */}
      <span
        aria-hidden="true"
        className="pointer-events-none absolute inset-x-0 -top-16 -z-20 mx-auto h-80 max-w-xl"
        style={{
          background: `radial-gradient(ellipse at 50% 40%, ${
            unavailable ? "var(--warning)" : "var(--primary)"
          } 0%, transparent 62%)`,
          opacity: 0.13,
        }}
      />
      {/* 3 · noise, to keep the bloom from banding on wide flat panels.
             Soft-light rather than overlay: overlay drives mid-grey noise
             visibly darker on a light surface. */}
      <span
        aria-hidden="true"
        className="pointer-events-none absolute inset-0 -z-10 opacity-[0.04] mix-blend-soft-light dark:opacity-[0.09]"
        style={{
          backgroundImage: `url(${textures.noise})`,
          backgroundSize: "256px",
          maskImage: FEATHER,
          WebkitMaskImage: FEATHER,
        }}
      />

      {/* 4 · glass panel */}
      <div
        className={
          compact
            ? "flex flex-col items-center"
            : "glass flex flex-col items-center rounded-2xl px-10 py-10"
        }
      >
        {art ? (
          <div className="relative mb-6 flex items-center justify-center">
            <span
              aria-hidden="true"
              className={`absolute h-32 w-32 rounded-full blur-3xl ${
                unavailable ? "bg-warning/20" : "bg-primary/20"
              }`}
            />
            <img
              src={art}
              alt=""
              aria-hidden="true"
              loading="lazy"
              className={`illus relative ${compact ? "w-40" : "w-64"}`}
            />
          </div>
        ) : null}

        {/* Colour set inline: `.eyebrow` carries its own colour and is defined
            after the utilities in styles.css, so `text-warning` loses. */}
        <p className="eyebrow mb-2" style={unavailable ? { color: "var(--warning)" } : undefined}>
          {EYEBROW[kind]}
        </p>

        <h3
          className={`text-balance font-semibold tracking-tight text-text ${
            compact ? "text-base" : "text-xl"
          }`}
        >
          {title}
        </h3>

        {description ? (
          <p className="mt-2.5 max-w-md text-pretty text-sm leading-relaxed text-text-muted">
            {description}
          </p>
        ) : null}

        {hint ? (
          <p className="mt-4 max-w-md border-t border-border pt-4 text-xs leading-relaxed text-text-subtle">
            {hint}
          </p>
        ) : null}

        {action ? <div className="mt-6 flex items-center gap-2">{action}</div> : null}
      </div>
    </motion.div>
  );
}

/**
 * The button an empty state is allowed to offer.
 *
 * Atlas observes and never modifies, so there is no "Create a service" here
 * and never will be. What remains is genuinely useful: clear the filter that
 * hid everything, retry a query that failed, or read what makes a source
 * appear. Those are the honest calls to action for a read-only platform.
 */
export function EmptyAction({
  onClick,
  children,
  variant = "primary",
}: {
  onClick: () => void;
  children: ReactNode;
  variant?: "primary" | "ghost";
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={
        variant === "primary"
          ? "lift rounded-lg bg-primary px-3.5 py-2 text-sm font-medium text-white shadow-sm hover:bg-primary-hover"
          : "rounded-lg border border-border px-3.5 py-2 text-sm font-medium text-text-muted hover:bg-surface-hover hover:text-text"
      }
    >
      {children}
    </button>
  );
}
