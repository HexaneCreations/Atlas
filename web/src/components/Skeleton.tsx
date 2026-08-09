/**
 * Loading placeholders.
 *
 * A skeleton is shown where the shape of the answer is roughly known — a list
 * of rows, a tile that will hold one number, a page silhouette. It replaced a
 * centred "Loading…", which told an operator nothing and let the layout jump
 * when data landed.
 *
 * These are the only loading shapes in Atlas. [QueryState] renders
 * [ListSkeleton] for every pending panel, so a second implementation living
 * next to a call site is a bug rather than a variant.
 */
export function Skeleton({ className = "" }: { className?: string }) {
  return (
    <span
      aria-hidden="true"
      className={`block animate-pulse rounded bg-surface-hover ${className}`}
    />
  );
}

/**
 * A list of rows arriving: an icon block and two lines of varying width.
 *
 * The widths vary per row so the block reads as text rather than as a
 * progress bar that has stalled, which a column of identical bars does.
 */
export function ListSkeleton({ rows = 3 }: { rows?: number }) {
  // Literal classes, cycled — Tailwind only emits utilities it can see in the
  // source, so a computed `w-[${n}%]` would compile to nothing.
  const wide = ["w-3/4", "w-11/12", "w-2/3", "w-5/6"];
  const narrow = ["w-1/3", "w-2/5", "w-1/4", "w-1/2"];

  return (
    <div className="flex flex-col gap-2.5 p-4" role="status" aria-label="Loading">
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="flex items-center gap-3">
          <Skeleton className="h-8 w-8 shrink-0 rounded-lg" />
          <div className="flex-1">
            <Skeleton className={`h-2.5 ${wide[i % wide.length] ?? "w-3/4"}`} />
            <Skeleton className={`mt-1.5 h-2 opacity-60 ${narrow[i % narrow.length] ?? "w-1/3"}`} />
          </div>
        </div>
      ))}
    </div>
  );
}

/** A skeleton for a KPI tile. */
export function StatSkeleton() {
  return (
    <div className="rounded-xl border border-border bg-surface p-5">
      <Skeleton className="mb-3 h-3 w-20" />
      <Skeleton className="h-8 w-24" />
    </div>
  );
}

/** A skeleton table body, matching the column count it will become. */
export function TableSkeleton({ rows = 5, columns = 4 }: { rows?: number; columns?: number }) {
  return (
    <div className="flex flex-col gap-px overflow-hidden rounded-xl border border-border">
      {Array.from({ length: rows }, (_, r) => (
        <div key={r} className="flex items-center gap-4 bg-surface px-4 py-3.5">
          {Array.from({ length: columns }, (_, c) => (
            <Skeleton key={c} className={`h-3.5 ${c === 0 ? "w-40" : "w-20"}`} />
          ))}
        </div>
      ))}
    </div>
  );
}

/**
 * The fallback while a route's chunk loads.
 *
 * Pages are code-split, so on a cold navigation there is a real gap before
 * anything renders. Showing the page's own silhouette — a hero, a row of
 * cards, a panel — keeps the layout from jumping when the chunk arrives,
 * which over a slow link is the difference between "loading" and "broken".
 */
export function PageSkeleton() {
  return (
    <div>
      <Skeleton className="mb-6 h-32 rounded-2xl" />
      <div className="mb-6 grid grid-cols-2 gap-4 lg:grid-cols-4">
        {Array.from({ length: 4 }, (_, i) => (
          <StatSkeleton key={i} />
        ))}
      </div>
      <Skeleton className="h-64 rounded-xl" />
    </div>
  );
}
