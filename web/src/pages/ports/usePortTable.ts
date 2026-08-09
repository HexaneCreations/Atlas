import { useMemo, useState } from "react";
import type { Port, PortProtocol } from "../../api/types";
import {
  exposureOf, tlsStateOf, wellKnownName, type Exposure, type TLSState,
} from "./portModel";

export type SortKey = "port" | "protocol" | "address" | "process" | "exposure" | "tls";
export type SortDirection = "asc" | "desc";

export interface Filters {
  search: string;
  protocol: "all" | PortProtocol;
  exposure: "all" | Exposure;
  tls: "all" | TLSState;
  process: string;
}

export const DEFAULT_FILTERS: Filters = {
  search: "",
  protocol: "all",
  exposure: "all",
  tls: "all",
  process: "all",
};

/**
 * Filtering and sorting for the socket explorer.
 *
 * The default sort is by exposure rather than by port number, because the
 * question this page exists to answer is "what can be reached", not "what is
 * listening in numeric order". Port number is the tie-break, so within a
 * risk tier the list still reads like `netstat`.
 */
export function usePortTable(ports: Port[]) {
  const [filters, setFilters] = useState<Filters>(DEFAULT_FILTERS);
  const [sortKey, setSortKey] = useState<SortKey>("exposure");
  const [direction, setDirection] = useState<SortDirection>("asc");

  const processes = useMemo(() => {
    const seen = new Set<string>();
    for (const p of ports) if (p.process) seen.add(p.process);
    return [...seen].sort((a, b) => a.localeCompare(b));
  }, [ports]);

  const filtered = useMemo(() => {
    const q = filters.search.trim().toLowerCase();

    return ports.filter((p) => {
      if (filters.protocol !== "all" && p.protocol !== filters.protocol) return false;
      if (filters.exposure !== "all" && exposureOf(p) !== filters.exposure) return false;
      if (filters.tls !== "all" && tlsStateOf(p) !== filters.tls) return false;
      if (filters.process !== "all" && p.process !== filters.process) return false;
      if (!q) return true;
      // The well-known name is searchable so "postgres" finds 5432 even when
      // the owning process is called something else entirely.
      return (
        String(p.port).includes(q) ||
        p.address.toLowerCase().includes(q) ||
        (p.process?.toLowerCase().includes(q) ?? false) ||
        (p.pid !== undefined && String(p.pid).includes(q)) ||
        (wellKnownName(p.port)?.toLowerCase().includes(q) ?? false) ||
        (p.tls?.subject?.toLowerCase().includes(q) ?? false) ||
        (p.tls?.issuer?.toLowerCase().includes(q) ?? false)
      );
    });
  }, [ports, filters]);

  const sorted = useMemo(() => {
    const sign = direction === "asc" ? 1 : -1;
    // Port number breaks every tie. Exposure and protocol are tiny domains
    // where ties are the rule, and without this the rows reshuffle on each
    // poll while somebody is reading them.
    return [...filtered].sort((a, b) => sign * compare(a, b, sortKey) || a.port - b.port);
  }, [filtered, sortKey, direction]);

  function toggleSort(key: SortKey) {
    if (key === sortKey) {
      setDirection((d) => (d === "asc" ? "desc" : "asc"));
      return;
    }
    setSortKey(key);
    // Every column here is either a name or a severity rank; both read
    // ascending, with the worst first.
    setDirection("asc");
  }

  return { filters, setFilters, processes, sorted, sortKey, direction, toggleSort };
}

/** Widest reach first. */
const EXPOSURE_RANK: Record<Exposure, number> = { world: 0, network: 1, loopback: 2 };

/** Worst posture first, with the two "not a finding" states last. */
const TLS_RANK: Record<TLSState, number> = {
  expired: 0, expiring: 1, "self-signed": 2, valid: 3, plaintext: 4, unprobed: 5,
};

function compare(a: Port, b: Port, key: SortKey): number {
  switch (key) {
    case "port":
      return a.port - b.port;
    case "protocol":
      return a.protocol.localeCompare(b.protocol);
    case "address":
      return a.address.localeCompare(b.address);
    case "process":
      return (a.process ?? "").localeCompare(b.process ?? "");
    case "exposure":
      return EXPOSURE_RANK[exposureOf(a)] - EXPOSURE_RANK[exposureOf(b)];
    case "tls":
      return TLS_RANK[tlsStateOf(a)] - TLS_RANK[tlsStateOf(b)];
  }
}

/** A stable identity for a socket. Port alone is not unique — the same number
 *  can be bound on several addresses and on both protocols. */
export function socketKey(p: Port): string {
  return `${p.protocol}:${p.address}:${String(p.port)}`;
}
