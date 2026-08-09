/**
 * Shared table styling, as class-name constants rather than a wrapper
 * component.
 *
 * A wrapper component would need a prop for every variation eight pages
 * actually use — colspan headers, row grouping, per-row click handlers — and
 * would grow a prop for each one. Plain `<table>` markup with these classes
 * gets the consistency without that: every page's table looks identical,
 * and every page still writes ordinary JSX for its own row shape.
 */
export const TABLE_WRAP = "overflow-x-auto rounded-xl border border-border";
export const TABLE = "w-full border-collapse text-sm";
export const THEAD_TR = "border-b border-border bg-surface-hover";
export const TH = "px-4 py-3 text-left text-xs font-semibold uppercase tracking-wider text-text-muted";
export const TR = "border-b border-border last:border-0 hover:bg-surface-hover";
export const TD = "px-4 py-3 align-middle text-text";
export const TD_MUTED = "px-4 py-3 align-middle text-text-muted";
export const TD_NUM = "px-4 py-3 align-middle text-right tabular-nums text-text";
