const EMPTY: unknown[] = [];

/**
 * A stable empty-array fallback for `data?.things ?? emptyArray()`.
 *
 * `?? []` allocates a fresh array on every render in which `data` is still
 * undefined — harmless for rendering, but it breaks referential equality for
 * anything that depends on the result, which is exactly what a `useMemo`
 * dependency array checks. One shared, frozen-in-spirit empty array read
 * back through a generic keeps the fallback cheap and identity-stable.
 */
export function emptyArray<T>(): T[] {
  return EMPTY as T[];
}
