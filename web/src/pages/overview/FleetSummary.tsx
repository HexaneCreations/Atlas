import { Link } from "react-router";
import { ArrowRight } from "lucide-react";
import type { Node, NodeStatus } from "../../api/types";

/**
 * The fleet, as one strip.
 *
 * Overview owns the roll-up; the Nodes page owns the detail. This panel
 * deliberately stops at "how many, in what state, tagged how" and hands off —
 * duplicating per-node comparison here would mean two places to change every
 * time the fleet model grows, and two places to disagree.
 *
 * The node chips double as the selector for the vitals below, so the fleet
 * strip and "which host am I looking at" are one control rather than two
 * stacked rows that happen to list the same machines.
 */

const STATUS_ORDER: NodeStatus[] = ["down", "stale", "up"];

const STATUS_STYLE: Record<NodeStatus, { dot: string; text: string; label: string }> = {
  up: { dot: "bg-success", text: "text-success", label: "reporting" },
  stale: { dot: "bg-warning", text: "text-warning", label: "quiet" },
  down: { dot: "bg-danger", text: "text-danger", label: "not reporting" },
};

/** Untagged is a real state, and usually one worth fixing — never folded into
 *  a guessed environment. */
const UNTAGGED = "untagged";

export function FleetSummary({
  nodes,
  selectedID,
  onSelect,
}: {
  nodes: Node[];
  selectedID: string | undefined;
  onSelect: (id: string) => void;
}) {
  const counts = countByStatus(nodes);
  const environments = countByEnvironment(nodes);

  return (
    <section className="elev-2 mb-6 rounded-xl" aria-labelledby="fleet-heading">
      <header className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-5 py-3.5">
        <h2 id="fleet-heading" className="text-card-title font-semibold text-text">
          Fleet
        </h2>

        <div className="flex flex-wrap items-center gap-3">
          {STATUS_ORDER.filter((s) => counts[s] > 0).map((s) => (
            <span key={s} className="flex items-center gap-1.5 text-xs">
              <span aria-hidden="true" className={`h-1.5 w-1.5 rounded-full ${STATUS_STYLE[s].dot}`} />
              <strong className={`tabular-nums ${STATUS_STYLE[s].text}`}>{counts[s]}</strong>
              <span className="text-text-muted">{STATUS_STYLE[s].label}</span>
            </span>
          ))}
        </div>

        {environments.length > 0 ? (
          <div className="flex flex-wrap items-center gap-1.5">
            {environments.map(([name, n]) => (
              <span
                key={name}
                className={`rounded px-1.5 py-0.5 text-[11px] ${
                  name === UNTAGGED
                    ? "bg-surface-hover text-text-subtle"
                    : "bg-primary/10 text-primary"
                }`}
                title={
                  name === UNTAGGED
                    ? "No environment tag. Atlas never guesses which tier a host belongs to."
                    : undefined
                }
              >
                {name} · {n}
              </span>
            ))}
          </div>
        ) : null}

        <Link
          to="/nodes"
          className="group ml-auto flex items-center gap-1 text-xs font-medium text-text-muted hover:text-text"
        >
          Fleet overview
          <ArrowRight size={13} className="transition-transform group-hover:translate-x-0.5" />
        </Link>
      </header>

      <div className="flex flex-wrap gap-2 p-3">
        {nodes.map((n) => (
          <button
            key={n.node_id}
            type="button"
            aria-pressed={n.node_id === selectedID}
            onClick={() => { onSelect(n.node_id); }}
            className={`flex items-center gap-2 rounded-lg border px-3 py-1.5 text-sm font-medium transition-colors ${
              n.node_id === selectedID
                ? "border-primary bg-primary/10 text-primary"
                : "border-border text-text-muted hover:bg-surface-hover"
            }`}
          >
            <span
              aria-hidden="true"
              className={`h-1.5 w-1.5 rounded-full ${STATUS_STYLE[n.status].dot}`}
            />
            <span className="truncate">{n.hostname}</span>
          </button>
        ))}
      </div>
    </section>
  );
}

function countByStatus(nodes: Node[]): Record<NodeStatus, number> {
  const out: Record<NodeStatus, number> = { up: 0, stale: 0, down: 0 };
  for (const n of nodes) out[n.status]++;
  return out;
}

function countByEnvironment(nodes: Node[]): [string, number][] {
  const out = new Map<string, number>();
  for (const n of nodes) {
    const key = n.environment ?? UNTAGGED;
    out.set(key, (out.get(key) ?? 0) + 1);
  }
  // Untagged sorts last: it is a gap in configuration, not an environment.
  return [...out.entries()].sort((a, b) =>
    a[0] === UNTAGGED ? 1 : b[0] === UNTAGGED ? -1 : a[0].localeCompare(b[0]),
  );
}
