import type { Transition, Variants } from "framer-motion";

/**
 * The motion system.
 *
 * Two rules govern everything here, and both come from what Atlas is.
 *
 * Movement is short and never bouncy. This is a tool people open when
 * something is broken; animation that draws attention to itself competes with
 * the numbers it is wrapping. Durations sit at 0.15–0.25s, which reads as
 * responsive rather than as an effect.
 *
 * Nothing that carries live data animates on update. A CPU figure that slides
 * every five seconds is unreadable, and a chart that re-runs its entry
 * animation on every poll is worse than one that does not move at all.
 * Motion is for things entering and leaving — panels, drawers, page
 * transitions — not for values changing in place.
 */

/** The default easing: decelerating, no overshoot. */
export const EASE: Transition["ease"] = [0.22, 1, 0.36, 1];

export const DURATION = {
  fast: 0.15,
  base: 0.2,
  slow: 0.3,
} as const;

/** A panel or card entering. */
export const fadeUp: Variants = {
  hidden: { opacity: 0, y: 8 },
  visible: { opacity: 1, y: 0, transition: { duration: DURATION.base, ease: EASE } },
};

export const fade: Variants = {
  hidden: { opacity: 0 },
  visible: { opacity: 1, transition: { duration: DURATION.base, ease: EASE } },
  exit: { opacity: 0, transition: { duration: DURATION.fast, ease: EASE } },
};

/**
 * Staggers children into view.
 *
 * The stagger is deliberately small. A long cascade looks considered on a
 * marketing page and obstructive on a dashboard, where the fourth tile is as
 * urgent as the first.
 */
export const stagger: Variants = {
  hidden: {},
  visible: { transition: { staggerChildren: 0.04 } },
};

/** A right-hand drawer sliding in. */
export const slideOver: Variants = {
  hidden: { x: "100%" },
  visible: { x: 0, transition: { duration: DURATION.base, ease: EASE } },
  exit: { x: "100%", transition: { duration: DURATION.fast, ease: EASE } },
};

/** A modal or command palette. */
export const popIn: Variants = {
  hidden: { opacity: 0, scale: 0.97, y: -6 },
  visible: { opacity: 1, scale: 1, y: 0, transition: { duration: DURATION.fast, ease: EASE } },
  exit: { opacity: 0, scale: 0.97, transition: { duration: DURATION.fast, ease: EASE } },
};
