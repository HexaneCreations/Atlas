/** What Tab can land on. */
export const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled])';

/**
 * Where Tab must be sent to stay inside a dialog, or null when the browser's
 * own next stop is already inside it.
 */
export function focusAfterTab<T>(inside: readonly T[], active: unknown, backwards: boolean): T | null {
  const first = inside[0];
  const last = inside.at(-1);
  if (first === undefined || last === undefined) return null;
  if (!inside.some((e) => e === active)) return backwards ? last : first;
  if (backwards) return active === first ? last : null;
  return active === last ? first : null;
}
