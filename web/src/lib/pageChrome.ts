import { gradients, heroes, maps, patterns } from "./assets";

/**
 * Each page's identity: what it is called, what it is about, and the imagery
 * that carries that.
 *
 * Centralised deliberately. A page's chrome is a design decision about the
 * subject, not a per-component choice, and keeping the whole set in one place
 * means it can be read at a glance to check it stays coherent — and that no
 * page quietly drifts onto imagery that contradicts what it shows.
 *
 * `accent` exists so the palette is not uniformly violet. Purple is Atlas's
 * primary and stays the interaction colour throughout, but a page about
 * storage and a page about networking should not feel identical, and the
 * accent is what separates them without inventing a second brand.
 */
export interface PageChrome {
  /** The page's operational name — the hero's headline. */
  identity: string;
  /** One line on what the page is for, under the headline. */
  premise: string;
  /** The hero background image. */
  hero: string;
  /** The page-wide tiling pattern. */
  pattern: string;
  /** A gradient wash tinting the hero, so pages differ in temperature. */
  accent: string;
  /** A world map, on fleet-wide views only. */
  map?: string;
}

/** Mission Control's chrome, which also backs unknown routes. */
const OVERVIEW_CHROME: PageChrome = {
  identity: "Mission Control",
  premise: "Everything Atlas observes, at a glance.",
  hero: heroes.aurora,
  pattern: patterns.grid,
  accent: gradients.auroraPurple,
  map: maps.dots,
};

export const PAGE_CHROME: Record<string, PageChrome> = {
  "/": OVERVIEW_CHROME,
  "/nodes": {
    identity: "Fleet Overview",
    premise: "Every machine reporting to this Atlas instance.",
    hero: heroes.horizon,
    // The library assigns the world map to Nodes: this is the page about the
    // fleet as a whole, and the map is the only place it genuinely means
    // something.
    pattern: patterns.grid,
    accent: gradients.auroraBlue,
    map: maps.network,
  },
  "/containers": {
    identity: "Container Orchestration",
    premise: "Runtime lifecycle, images, and resource allocation.",
    hero: heroes.mesh,
    pattern: patterns.hex,
    accent: gradients.meshCyan,
  },
  "/processes": {
    identity: "System Activity",
    premise: "What is consuming this machine, and since when.",
    hero: heroes.polygon,
    pattern: patterns.hex,
    accent: gradients.meshViolet,
  },
  "/services": {
    identity: "Service Health",
    premise: "Units, their state, and what keeps them running.",
    hero: heroes.ocean,
    pattern: patterns.circuit,
    accent: gradients.auroraBlue,
  },
  "/ports": {
    identity: "Network Security",
    premise: "What this host exposes, and what protects it.",
    hero: heroes.nebula,
    pattern: patterns.circuit,
    accent: gradients.meshCyan,
  },
  "/disks": {
    identity: "Storage Analytics",
    premise: "Capacity, growth, and where the space has gone.",
    hero: heroes.terrain,
    pattern: patterns.topography,
    accent: gradients.midnight,
  },
  "/cron": {
    identity: "Automation Center",
    premise: "Scheduled work, and who it runs as.",
    hero: heroes.glass,
    pattern: patterns.topography,
    accent: gradients.radialGlow,
  },
};

/** The chrome for a path. Unknown routes fall back to Mission Control's, so
 *  an error page still looks like Atlas. */
export function chromeFor(pathname: string): PageChrome {
  return PAGE_CHROME[pathname] ?? OVERVIEW_CHROME;
}
