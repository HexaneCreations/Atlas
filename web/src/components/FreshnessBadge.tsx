import { Radio } from "lucide-react";
import { formatAgo } from "../format";

/**
 * Discloses whether inventory came straight from the host or from a snapshot
 * an agent pushed. See docs/architecture/agent-design.md §5 — this is the
 * field that stops a stale remote snapshot from being read as current.
 */
export function FreshnessBadge({
  live,
  observedAt,
}: {
  live: boolean | undefined;
  observedAt: string | undefined;
}) {
  if (live === undefined) return null;

  if (live) {
    return (
      <span className="flex items-center gap-1.5 rounded-full border border-success/30 bg-success/10 px-2.5 py-1 text-[11px] font-medium text-success">
        <Radio size={11} />
        Live
      </span>
    );
  }

  const ageSeconds = observedAt ? (Date.now() - new Date(observedAt).getTime()) / 1000 : undefined;
  return (
    <span
      className="flex items-center gap-1.5 rounded-full border border-border bg-surface px-2.5 py-1 text-[11px] font-medium text-text-muted"
      title="Read from the latest snapshot this node's agent pushed, not live from the host."
    >
      <Radio size={11} />
      {ageSeconds !== undefined ? `As of ${formatAgo(ageSeconds)}` : "Snapshot"}
    </span>
  );
}
