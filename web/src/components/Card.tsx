import type { ReactNode } from "react";

export type CardLevel = "flat" | "raised" | "floating" | "glass" | "alert" | "warn";

/**
 * Surface levels.
 *
 * A page where every panel sits at the same elevation gives the eye nowhere
 * to start. These map onto the four levels defined in styles.css plus the two
 * semantic surfaces, so a page can say "this matters more" structurally
 * rather than by making a heading bigger.
 *
 *   flat      supporting detail — tables, secondary lists
 *   raised    the default panel
 *   floating  the thing the page is actually about
 *   glass     content over imagery
 *   alert     something is wrong
 *   warn      something needs watching
 */
const LEVEL: Record<CardLevel, string> = {
  flat: "elev-1",
  raised: "elev-2",
  floating: "elev-3",
  glass: "glass",
  alert: "surface-alert",
  warn: "surface-warn",
};

export function Card({
  children,
  className = "",
  level = "raised",
  hover = false,
}: {
  children: ReactNode;
  className?: string;
  level?: CardLevel;
  /** Adds pointer lift. For cards that lead somewhere; a static panel that
   *  rises under the cursor promises an interaction it does not have. */
  hover?: boolean;
}) {
  return (
    <div className={`rounded-xl p-5 ${LEVEL[level]} ${hover ? "lift" : ""} ${className}`}>
      {children}
    </div>
  );
}

export function CardHeader({
  title,
  action,
}: {
  title: string;
  action?: ReactNode;
}) {
  return (
    <div className="mb-4 flex items-center justify-between gap-3">
      <h3 className="text-card-title font-semibold text-text">{title}</h3>
      {action}
    </div>
  );
}
