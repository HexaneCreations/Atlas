/**
 * The Atlas design-asset library, addressed by name rather than by path.
 *
 * Assets are produced by `npm run assets`, which optimises the source pack
 * into web/public/assets. Referencing them through this map means a renamed
 * or re-optimised file breaks the build here, once, instead of silently
 * rendering a broken image somewhere in the UI.
 *
 * Every category has a stated purpose in the library's usage rules, and the
 * types below are shaped to keep that legible at the call site: heroes are
 * hero backgrounds, patterns are page-level washes, maps are reserved for
 * infrastructure-wide views. Which asset a given page uses is decided once,
 * in lib/pageChrome.ts, so no page invents its own combination.
 *
 * Imagery is decorative throughout. That is deliberate — an operations tool
 * must remain fully usable when images fail to load, which over a slow link
 * into a bastion host is a real condition rather than a hypothetical one.
 */
const base = "/assets";

export const brand = {
  mark: `${base}/logo/mark.webp`,
  wordmarkDark: `${base}/logo/wordmark-dark.webp`,
  wordmarkLight: `${base}/logo/wordmark-light.webp`,
} as const;

/** Full-bleed hero backgrounds. Every page hero takes one. */
export const heroes = {
  aurora: `${base}/heroes/aurora.webp`,
  horizon: `${base}/heroes/horizon.webp`,
  ocean: `${base}/heroes/ocean.webp`,
  terrain: `${base}/heroes/terrain.webp`,
  mesh: `${base}/heroes/mesh.webp`,
  polygon: `${base}/heroes/polygon.webp`,
  nebula: `${base}/heroes/nebula.webp`,
  glass: `${base}/heroes/glass.webp`,
} as const;

/** Gradient washes, layered over a hero rather than used as flat fills. */
export const gradients = {
  auroraPurple: `${base}/gradients/aurora-purple.webp`,
  auroraBlue: `${base}/gradients/aurora-blue.webp`,
  meshViolet: `${base}/gradients/mesh-violet.webp`,
  meshCyan: `${base}/gradients/mesh-cyan.webp`,
  midnight: `${base}/gradients/midnight.webp`,
  radialGlow: `${base}/gradients/radial-glow.webp`,
} as const;

/**
 * Tiling patterns, applied page-wide at very low opacity.
 *
 * The library assigns these by subject: grid for dashboards and analytics,
 * dots for empty states and onboarding, hex for infrastructure, circuit for
 * services and networking, topography for reports, blueprint for settings
 * and architecture.
 */
export const patterns = {
  grid: `${base}/patterns/grid.webp`,
  dots: `${base}/patterns/dots.webp`,
  hex: `${base}/patterns/hex.webp`,
  circuit: `${base}/patterns/circuit.webp`,
  topography: `${base}/patterns/topography.webp`,
  blueprint: `${base}/patterns/blueprint.webp`,
} as const;

/** Overlay textures. Noise and grain tile; frost and vignette are framed. */
export const textures = {
  noise: `${base}/textures/noise.webp`,
  grain: `${base}/textures/grain.webp`,
  glassFrost: `${base}/textures/glass-frost.webp`,
  vignette: `${base}/textures/vignette.webp`,
} as const;

/**
 * World maps.
 *
 * Reserved for views about the fleet as a whole — the dashboard and nodes
 * heroes — and never dropped into an arbitrary card, where a map implies a
 * geography the card is not reporting.
 */
export const maps = {
  dots: `${base}/maps/world-dots.webp`,
  glow: `${base}/maps/world-glow.webp`,
  grid: `${base}/maps/world-grid.webp`,
  network: `${base}/maps/world-network.webp`,
  night: `${base}/maps/world-night.webp`,
} as const;

/** Illustrations for "this list is legitimately empty". */
export const emptyArt = {
  data: `${base}/empty/no-data.webp`,
  servers: `${base}/empty/no-servers.webp`,
  containers: `${base}/empty/no-containers.webp`,
  services: `${base}/empty/no-services.webp`,
  logs: `${base}/empty/no-logs.webp`,
  alerts: `${base}/empty/no-alerts.webp`,
  reports: `${base}/empty/no-reports.webp`,
  search: `${base}/empty/no-search-results.webp`,
} as const;

export const errorArt = {
  notFound: `${base}/errors/404.webp`,
  forbidden: `${base}/errors/403.webp`,
  unauthorized: `${base}/errors/401.webp`,
  serverError: `${base}/errors/500.webp`,
  offline: `${base}/errors/server-offline.webp`,
} as const;

export const healthArt = {
  healthy: `${base}/health/healthy.webp`,
  warning: `${base}/health/warning.webp`,
  critical: `${base}/health/critical.webp`,
  maintenance: `${base}/health/maintenance.webp`,
} as const;

export const onboardingArt = {
  infrastructure: `${base}/onboarding/infrastructure.webp`,
  monitor: `${base}/onboarding/monitor.webp`,
  analytics: `${base}/onboarding/analytics.webp`,
  automation: `${base}/onboarding/automation.webp`,
} as const;

export type EmptyArtKey = keyof typeof emptyArt;
