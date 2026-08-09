import { useMemo, useState } from "react";
import type { Container, ContainerState } from "../../api/types";
import { STANDALONE, effectiveHealth, type ContainerUsage, type EffectiveHealth } from "./containerModel";

export type SortKey =
  | "name" | "state" | "health" | "project" | "image"
  | "cpu" | "memory" | "restarts" | "created" | "uptime";
export type SortDirection = "asc" | "desc";

export interface Filters {
  search: string;
  state: "all" | ContainerState;
  health: "all" | EffectiveHealth;
  project: string;
  image: string;
}

export const DEFAULT_FILTERS: Filters = {
  search: "",
  state: "all",
  health: "all",
  project: "all",
  image: "all",
};

/**
 * Filtering and sorting for the container explorer.
 *
 * The health filter matches *effective* health, so filtering to "unhealthy"
 * returns containers that are failing right now rather than every container
 * that ever failed a check before it stopped.
 */
export function useContainerTable(containers: Container[], usage: Map<string, ContainerUsage>) {
  const [filters, setFilters] = useState<Filters>(DEFAULT_FILTERS);
  const [sortKey, setSortKey] = useState<SortKey>("state");
  const [direction, setDirection] = useState<SortDirection>("asc");

  const projects = useMemo(() => {
    const seen = new Set<string>();
    for (const c of containers) seen.add(c.compose_project ?? STANDALONE);
    return [...seen].sort(standaloneLast);
  }, [containers]);

  const images = useMemo(() => {
    const seen = new Set<string>();
    for (const c of containers) seen.add(c.image);
    return [...seen].sort((a, b) => a.localeCompare(b));
  }, [containers]);

  const filtered = useMemo(() => {
    const q = filters.search.trim().toLowerCase();

    return containers.filter((c) => {
      if (filters.state !== "all" && c.state !== filters.state) return false;
      if (filters.health !== "all" && effectiveHealth(c) !== filters.health) return false;
      if (filters.project !== "all" && (c.compose_project ?? STANDALONE) !== filters.project) {
        return false;
      }
      if (filters.image !== "all" && c.image !== filters.image) return false;
      if (!q) return true;
      // The short id is searchable because it is what `docker ps` prints and
      // what an operator has copied from a terminal.
      return (
        c.name.toLowerCase().includes(q) ||
        c.image.toLowerCase().includes(q) ||
        c.short_id.toLowerCase().includes(q) ||
        (c.compose_project?.toLowerCase().includes(q) ?? false) ||
        (c.compose_service?.toLowerCase().includes(q) ?? false)
      );
    });
  }, [containers, filters]);

  const sorted = useMemo(() => {
    const sign = direction === "asc" ? 1 : -1;
    // Name breaks every tie, always ascending. Most sorts here are over small
    // integer domains — state, restart count, health — where ties are the norm,
    // and without a tie-break the rows reshuffle under the reader on each poll.
    return [...filtered].sort(
      (a, b) => sign * compare(a, b, sortKey, usage) || a.name.localeCompare(b.name),
    );
  }, [filtered, sortKey, direction, usage]);

  function toggleSort(key: SortKey) {
    if (key === sortKey) {
      setDirection((d) => (d === "asc" ? "desc" : "asc"));
      return;
    }
    setSortKey(key);
    // Text and severity orders read ascending; magnitudes read descending.
    setDirection(
      key === "name" || key === "project" || key === "image" || key === "state" || key === "health"
        ? "asc"
        : "desc",
    );
  }

  return { filters, setFilters, projects, images, sorted, sortKey, direction, toggleSort };
}

/** Worst-first, so the default sort surfaces what needs attention. */
const STATE_RANK: Record<ContainerState, number> = {
  restarting: 0, dead: 1, paused: 2, exited: 3, removing: 4, created: 5, running: 6,
};

const HEALTH_RANK: Record<EffectiveHealth, number> = {
  unhealthy: 0, starting: 1, healthy: 2, none: 3, unknown: 4,
};

function compare(
  a: Container,
  b: Container,
  key: SortKey,
  usage: Map<string, ContainerUsage>,
): number {
  switch (key) {
    case "name":
      return a.name.localeCompare(b.name);
    case "state":
      return STATE_RANK[a.state] - STATE_RANK[b.state];
    case "health":
      return HEALTH_RANK[effectiveHealth(a)] - HEALTH_RANK[effectiveHealth(b)];
    case "project":
      return (a.compose_project ?? STANDALONE).localeCompare(b.compose_project ?? STANDALONE);
    case "image":
      return a.image.localeCompare(b.image);
    case "restarts":
      return a.restart_count - b.restart_count;
    case "created":
      return time(a.created_at) - time(b.created_at);
    case "uptime":
      return (a.uptime_seconds ?? 0) - (b.uptime_seconds ?? 0);
    case "cpu":
      return (usage.get(a.name)?.cpu ?? -1) - (usage.get(b.name)?.cpu ?? -1);
    case "memory":
      return (usage.get(a.name)?.memory ?? -1) - (usage.get(b.name)?.memory ?? -1);
  }
}

function time(iso: string | undefined): number {
  return iso ? new Date(iso).getTime() : 0;
}

export function standaloneLast(a: string, b: string): number {
  if (a === STANDALONE) return 1;
  if (b === STANDALONE) return -1;
  return a.localeCompare(b);
}
