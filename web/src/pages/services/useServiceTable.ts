import { useMemo, useState } from "react";
import type { GraphHealth, GraphNode, Service } from "../../api/types";

export type SortKey = "name" | "health" | "state" | "enabled" | "restarts" | "uptime" | "memory";
export type SortDirection = "asc" | "desc";

export interface Filters {
  search: string;
  health: "all" | GraphHealth;
  state: string;
  enabled: "all" | "enabled" | "disabled";
}

export const DEFAULT_FILTERS: Filters = {
  search: "",
  health: "all",
  state: "all",
  enabled: "all",
};

/**
 * A service joined with its graph node.
 *
 * The unit list carries runtime facts systemd's `show` gives per unit — main
 * pid, restarts, memory, enablement — while the graph carries propagated
 * health and degree. Neither alone answers "which services need attention":
 * the list knows a unit is active, the graph knows its database has failed.
 */
export interface ServiceRow {
  unit: Service;
  /** Absent when the graph has not loaded, or on a host with no service
   *  manager. Health then falls back to the unit's own state. */
  node?: GraphNode | undefined;
  health: GraphHealth;
  failedDependencies: string[];
  dependencies: number;
  dependents: number;
}

/** Health from the unit's own state, for when the graph is unavailable.
 *
 *  Deliberately never reports `degraded`: that verdict requires knowing what a
 *  unit depends on, and claiming it without the graph would be invention. */
export function ownHealth(u: Service): GraphHealth {
  if (u.failed || u.active_state === "failed") return "failed";
  if (u.active_state === "active" || u.active_state === "activating") return "healthy";
  if (u.active_state === "inactive" || u.active_state === "deactivating") return "inactive";
  return "unknown";
}

export function joinServices(units: Service[], nodes: GraphNode[] | undefined): ServiceRow[] {
  const byID = new Map((nodes ?? []).map((n) => [n.id, n]));

  return units.map((unit) => {
    const node = byID.get(unit.name);
    return {
      unit,
      node,
      health: node?.health ?? ownHealth(unit),
      failedDependencies: node?.failed_dependencies ?? [],
      dependencies: node?.dependencies ?? 0,
      dependents: node?.dependents ?? 0,
    };
  });
}

/**
 * Filtering and sorting for the service explorer.
 *
 * Default sort is by health, worst first — the question this page answers is
 * "what needs attention", and a unit that is running while its database is
 * down ranks above one that is merely stopped.
 */
export function useServiceTable(rows: ServiceRow[]) {
  const [filters, setFilters] = useState<Filters>(DEFAULT_FILTERS);
  const [sortKey, setSortKey] = useState<SortKey>("health");
  const [direction, setDirection] = useState<SortDirection>("asc");

  const states = useMemo(() => {
    const seen = new Set<string>();
    for (const r of rows) seen.add(r.unit.active_state);
    return [...seen].sort((a, b) => a.localeCompare(b));
  }, [rows]);

  const filtered = useMemo(() => {
    const q = filters.search.trim().toLowerCase();

    return rows.filter((r) => {
      if (filters.health !== "all" && r.health !== filters.health) return false;
      if (filters.state !== "all" && r.unit.active_state !== filters.state) return false;
      if (filters.enabled === "enabled" && !r.unit.enabled) return false;
      if (filters.enabled === "disabled" && r.unit.enabled) return false;
      if (!q) return true;
      // A failed dependency name is searchable: during an incident the unit
      // somebody knows about is the broken one, not everything downstream.
      return (
        r.unit.name.toLowerCase().includes(q) ||
        (r.unit.description?.toLowerCase().includes(q) ?? false) ||
        r.unit.sub_state.toLowerCase().includes(q) ||
        r.failedDependencies.some((d) => d.toLowerCase().includes(q))
      );
    });
  }, [rows, filters]);

  const sorted = useMemo(() => {
    const sign = direction === "asc" ? 1 : -1;
    // Name breaks every tie. Health and state are tiny domains where ties are
    // the rule, and without this the rows reshuffle under the reader on each
    // poll.
    return [...filtered].sort(
      (a, b) => sign * compare(a, b, sortKey) || a.unit.name.localeCompare(b.unit.name),
    );
  }, [filtered, sortKey, direction]);

  function toggleSort(key: SortKey) {
    if (key === sortKey) {
      setDirection((d) => (d === "asc" ? "desc" : "asc"));
      return;
    }
    setSortKey(key);
    setDirection(key === "name" || key === "health" || key === "state" ? "asc" : "desc");
  }

  return { filters, setFilters, states, sorted, sortKey, direction, toggleSort };
}

/** Worst first. Degraded outranks inactive: a unit that is running but whose
 *  dependency has failed is a live problem; a stopped unit usually is not. */
const HEALTH_RANK: Record<GraphHealth, number> = {
  failed: 0,
  degraded: 1,
  unknown: 2,
  inactive: 3,
  healthy: 4,
};

function compare(a: ServiceRow, b: ServiceRow, key: SortKey): number {
  switch (key) {
    case "name":
      return a.unit.name.localeCompare(b.unit.name);
    case "health":
      return HEALTH_RANK[a.health] - HEALTH_RANK[b.health];
    case "state":
      return a.unit.active_state.localeCompare(b.unit.active_state);
    case "enabled":
      return Number(b.unit.enabled) - Number(a.unit.enabled);
    case "restarts":
      return a.unit.restart_count - b.unit.restart_count;
    case "uptime":
      return (a.unit.uptime_seconds ?? 0) - (b.unit.uptime_seconds ?? 0);
    case "memory":
      return (a.unit.memory_bytes ?? 0) - (b.unit.memory_bytes ?? 0);
  }
}
