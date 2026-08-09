/** A ranked horizontal-bar list — "Top CPU Consumers", "Top Memory
 *  Consumers" — the same shape the reference design uses for it. Bars are
 *  scaled against the leader, not against a fixed maximum, so the list is
 *  legible whether the top process is using 3% or 90%. */
export function TopList({
  items,
}: {
  items: { key: string; label: string; value: string; fraction: number }[];
}) {
  if (items.length === 0) {
    return <p className="py-6 text-center text-sm text-text-muted">Nothing to show.</p>;
  }

  return (
    <ol className="flex flex-col gap-3">
      {items.map((item, i) => (
        <li key={item.key} className="flex items-center gap-3">
          <span className="w-4 shrink-0 text-xs text-text-muted">{i + 1}</span>
          <span className="w-28 shrink-0 truncate text-sm text-text" title={item.label}>
            {item.label}
          </span>
          <span className="h-1.5 flex-1 overflow-hidden rounded-full bg-surface-hover">
            <span
              className="block h-full rounded-full bg-primary"
              style={{ width: `${Math.max(item.fraction * 100, 2)}%` }}
            />
          </span>
          <span className="w-16 shrink-0 text-right text-xs tabular-nums text-text-muted">{item.value}</span>
        </li>
      ))}
    </ol>
  );
}
