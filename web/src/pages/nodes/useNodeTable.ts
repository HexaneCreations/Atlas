import { useMemo, useState } from "react";
import type { Node, NodeStatus } from "../../api/types";

export type SortKey =
  | "hostname"
  | "status"
  | "environment"
  | "platform"
  | "cores"
  | "uptime"
  | "lastSeen";
export type SortDirection = "asc" | "desc";

/** The untagged bucket. A node with no environment is a real state — a gap in
 *  configuration — and is never folded into a guessed tier. */
export const UNTAGGED = "untagged";

export interface Filters {
  search: string;
  status: "all" | NodeStatus;
  environment: string;
  platform: string;
}

export const DEFAULT_FILTERS: Filters = {
  search: "",
  status: "all",
  environment: "all",
  platform: "all",
};

/**
 * Filtering and sorting for the node explorer.
 *
 * Extracted from the page for the same reason as the process explorer's
 * equivalent: comparator and predicate logic is worth reading and testing on
 * its own, and a page that already renders an inspector and four summary
 * panels should not also be where sort order is decided.
 */
export function useNodeTable(nodes: Node[]) {
  const [filters, setFilters] = useState<Filters>(DEFAULT_FILTERS);
  const [sortKey, setSortKey] = useState<SortKey>("status");
  const [direction, setDirection] = useState<SortDirection>("asc");

  const environments = useMemo(() => {
    const seen = new Set<string>();
    for (const n of nodes) seen.add(n.environment ?? UNTAGGED);
    return [...seen].sort(untaggedLast);
  }, [nodes]);

  const platforms = useMemo(() => {
    const seen = new Set<string>();
    for (const n of nodes) if (n.platform) seen.add(n.platform);
    return [...seen].sort((a, b) => a.localeCompare(b));
  }, [nodes]);

  const filtered = useMemo(() => {
    const q = filters.search.trim().toLowerCase();

    return nodes.filter((n) => {
      if (filters.status !== "all" && n.status !== filters.status) return false;
      if (filters.environment !== "all" && (n.environment ?? UNTAGGED) !== filters.environment) {
        return false;
      }
      if (filters.platform !== "all" && n.platform !== filters.platform) return false;
      if (!q) return true;
      // The node id is searchable as well as the hostname: it is what appears
      // in logs and in metric queries, and is often the only identifier
      // somebody has to hand.
      return (
        n.hostname.toLowerCase().includes(q) ||
        n.node_id.toLowerCase().includes(q) ||
        (n.platform?.toLowerCase().includes(q) ?? false) ||
        (n.os?.toLowerCase().includes(q) ?? false) ||
        (n.kernel?.toLowerCase().includes(q) ?? false) ||
        (n.architecture?.toLowerCase().includes(q) ?? false) ||
        (n.environment?.toLowerCase().includes(q) ?? false)
      );
    });
  }, [nodes, filters]);

  const sorted = useMemo(() => {
    const sign = direction === "asc" ? 1 : -1;
    // Hostname breaks every tie, and always ascending regardless of the
    // primary direction. Ties are the common case rather than the exception
    // here — a fleet of identical VMs shares a core count exactly, and hosts
    // booted together share an uptime — so without this the rows fell back to
    // whatever order the API returned and reshuffled under the reader.
    return [...filtered].sort(
      (a, b) => sign * compare(a, b, sortKey) || a.hostname.localeCompare(b.hostname),
    );
  }, [filtered, sortKey, direction]);

  function toggleSort(key: SortKey) {
    if (key === sortKey) {
      setDirection((d) => (d === "asc" ? "desc" : "asc"));
      return;
    }
    setSortKey(key);
    // Text sorts ascending; magnitudes and "worst first" sort descending —
    // except status, whose ascending order is already down → stale → up.
    setDirection(
      key === "hostname" || key === "environment" || key === "platform" || key === "status"
        ? "asc"
        : "desc",
    );
  }

  return {
    filters, setFilters, environments, platforms,
    sorted, sortKey, direction, toggleSort,
  };
}

/** Worst first, so the default sort surfaces what needs attention. */
const STATUS_RANK: Record<NodeStatus, number> = { down: 0, stale: 1, up: 2 };

function compare(a: Node, b: Node, key: SortKey): number {
  switch (key) {
    case "hostname":
      return a.hostname.localeCompare(b.hostname);
    case "status":
      return STATUS_RANK[a.status] - STATUS_RANK[b.status];
    case "environment":
      return (a.environment ?? UNTAGGED).localeCompare(b.environment ?? UNTAGGED);
    case "platform":
      return (a.platform ?? "").localeCompare(b.platform ?? "");
    case "cores":
      return (a.cpu_cores ?? 0) - (b.cpu_cores ?? 0);
    case "uptime":
      return (a.uptime_seconds ?? 0) - (b.uptime_seconds ?? 0);
    case "lastSeen":
      // Larger "seconds since seen" is worse, so descending puts the most
      // out-of-date node first.
      return a.seconds_since_seen - b.seconds_since_seen;
  }
}

export function untaggedLast(a: string, b: string): number {
  if (a === UNTAGGED) return 1;
  if (b === UNTAGGED) return -1;
  return a.localeCompare(b);
}

/** Status counts for the fleet. Lives here rather than beside the summary
 *  panel so a component module does not export non-components. */
export function countByStatus(nodes: Node[]): Record<NodeStatus, number> {
  const out: Record<NodeStatus, number> = { up: 0, stale: 0, down: 0 };
  for (const n of nodes) out[n.status]++;
  return out;
}
