import { useMemo, useState } from "react";
import type { Process, ProcessState } from "../../api/types";

export type SortKey = "name" | "pid" | "user" | "cpu" | "memory" | "threads" | "runtime";
export type SortDirection = "asc" | "desc";

export interface Filters {
  search: string;
  state: "all" | ProcessState;
  user: string;
  /** Minimum CPU percentage. Zero shows everything. */
  minCpu: number;
  /** Minimum resident memory in MiB. Zero shows everything. */
  minMemoryMiB: number;
}

export const DEFAULT_FILTERS: Filters = {
  search: "",
  state: "all",
  user: "all",
  minCpu: 0,
  minMemoryMiB: 0,
};

/**
 * Filtering and sorting for the process explorer.
 *
 * Extracted from the page so the table's behaviour can be reasoned about —
 * and eventually tested — without rendering anything. The page is already
 * large; burying comparator logic inside it would make both harder to read.
 */
export function useProcessTable(processes: Process[]) {
  const [filters, setFilters] = useState<Filters>(DEFAULT_FILTERS);
  const [sortKey, setSortKey] = useState<SortKey>("cpu");
  const [direction, setDirection] = useState<SortDirection>("desc");

  const users = useMemo(() => {
    const seen = new Set<string>();
    for (const p of processes) if (p.username) seen.add(p.username);
    return [...seen].sort((a, b) => a.localeCompare(b));
  }, [processes]);

  const filtered = useMemo(() => {
    const q = filters.search.trim().toLowerCase();
    const minBytes = filters.minMemoryMiB * 1024 * 1024;

    return processes.filter((p) => {
      if (filters.state !== "all" && p.state !== filters.state) return false;
      if (filters.user !== "all" && p.username !== filters.user) return false;
      if (p.cpu_percent < filters.minCpu) return false;
      if (p.memory_rss < minBytes) return false;
      if (!q) return true;
      // The command line is searched too: finding "--config=/etc/foo" is
      // frequently how an operator identifies which of six identically named
      // workers is the one misbehaving.
      return (
        p.name.toLowerCase().includes(q) ||
        String(p.pid).includes(q) ||
        (p.username?.toLowerCase().includes(q) ?? false) ||
        (p.cmdline?.toLowerCase().includes(q) ?? false)
      );
    });
  }, [processes, filters]);

  const sorted = useMemo(() => {
    const sign = direction === "asc" ? 1 : -1;
    return [...filtered].sort((a, b) => sign * compare(a, b, sortKey));
  }, [filtered, sortKey, direction]);

  /** Clicking the active column flips direction; a new column starts in the
   *  direction that is useful for it — descending for magnitudes, ascending
   *  for names. */
  function toggleSort(key: SortKey) {
    if (key === sortKey) {
      setDirection((d) => (d === "asc" ? "desc" : "asc"));
      return;
    }
    setSortKey(key);
    setDirection(key === "name" || key === "user" ? "asc" : "desc");
  }

  return { filters, setFilters, users, sorted, sortKey, direction, toggleSort };
}

function compare(a: Process, b: Process, key: SortKey): number {
  switch (key) {
    case "name":
      return a.name.localeCompare(b.name);
    case "user":
      return (a.username ?? "").localeCompare(b.username ?? "");
    case "pid":
      return a.pid - b.pid;
    case "cpu":
      return a.cpu_percent - b.cpu_percent;
    case "memory":
      return a.memory_rss - b.memory_rss;
    case "threads":
      return a.threads - b.threads;
    case "runtime":
      return (a.running_for_seconds ?? 0) - (b.running_for_seconds ?? 0);
  }
}
