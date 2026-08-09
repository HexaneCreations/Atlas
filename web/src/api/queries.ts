import { useQueries, useQuery } from "@tanstack/react-query";
import { ApiError, apiGet } from "./client";
import { emptyArray } from "./empty";
import { useSelectedNodeID } from "../lib/selectedNode";
import type {
  CollectorsResponse,
  ContainerDetail,
  ContainerLogsResponse,
  ListContainersResponse,
  ListCronJobsResponse,
  ListActivityResponse,
  ListMountsResponse,
  ListPortsResponse,
  ListProcessesResponse,
  ListServicesResponse,
  ServiceDetail,
  ServiceGraph,
  LatestResponse,
  LatestValue,
  ListNodesResponse,
  MetricsResponse,
  SystemHealth,
  SystemInfo,
  SystemRuntime,
} from "./types";

/**
 * Query definitions for the Atlas API.
 *
 * Refresh intervals are chosen per resource rather than globally, because the
 * data moves at very different rates. Build identity changes only on deploy;
 * health and runtime figures are what an operator watches during an incident.
 */

/** Namespaced keys, so a cache invalidation can target one family. */
export const queryKeys = {
  system: {
    info: ["system", "info"] as const,
    health: ["system", "health"] as const,
    runtime: ["system", "runtime"] as const,
  },
} as const;

/** How often live panels re-poll, in milliseconds. */
export const REFRESH_INTERVAL_MS = 5_000;

/**
 * Retries a failed query only when the failure could plausibly resolve.
 *
 * Retrying a 404 wastes requests and delays the moment the operator sees the
 * real message; retrying a temporarily unavailable dependency is exactly what
 * should happen while Atlas restarts.
 */
function retryPolicy(failureCount: number, error: Error): boolean {
  if (error instanceof ApiError && !error.retryable) {
    return false;
  }
  return failureCount < 3;
}

/** Build identity of the running Atlas instance. */
export function useSystemInfo() {
  return useQuery({
    queryKey: queryKeys.system.info,
    queryFn: ({ signal }) => apiGet<SystemInfo>("/system/info", { signal }),
    // Identity changes only across a deploy, and a deploy restarts the page's
    // connection anyway.
    staleTime: 60_000,
    retry: retryPolicy,
  });
}

/** Health of Atlas and its dependencies. */
export function useSystemHealth() {
  return useQuery({
    queryKey: queryKeys.system.health,
    queryFn: ({ signal }) => apiGet<SystemHealth>("/system/health", { signal }),
    refetchInterval: REFRESH_INTERVAL_MS,
    // Keep polling when the tab is in the background: an operator who tabs
    // back during an incident needs current state, not a stale snapshot
    // followed by a visible refresh.
    refetchIntervalInBackground: true,
    retry: retryPolicy,
  });
}

/** Atlas's own resource consumption. */
export function useSystemRuntime() {
  return useQuery({
    queryKey: queryKeys.system.runtime,
    queryFn: ({ signal }) => apiGet<SystemRuntime>("/system/runtime", { signal }),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryPolicy,
  });
}

// ---------------------------------------------------------------- Phase 1 ----

export const nodeKeys = {
  all: ["nodes"] as const,
  latest: (nodeID: string) => ["nodes", nodeID, "latest"] as const,
  series: (nodeID: string, metrics: string[], range: string) =>
    ["nodes", nodeID, "series", metrics.join(","), range] as const,
  collectors: ["collectors"] as const,
} as const;

/** Every machine Atlas has observed. */
export function useNodes() {
  return useQuery({
    queryKey: nodeKeys.all,
    queryFn: ({ signal }) => apiGet<ListNodesResponse>("/nodes", { signal }),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryPolicy,
  });
}

/**
 * The current value of every series on a node.
 *
 * One request backs every stat tile on the page. Fetching per tile would issue
 * a dozen requests for data a single DISTINCT ON already returns.
 */
export function useLatestMetrics(nodeID: string | undefined) {
  return useQuery({
    queryKey: nodeKeys.latest(nodeID ?? ""),
    queryFn: ({ signal }) =>
      apiGet<LatestResponse>(`/metrics/latest?node=${encodeURIComponent(nodeID ?? "")}`, { signal }),
    enabled: Boolean(nodeID),
    refetchInterval: REFRESH_INTERVAL_MS,
    refetchIntervalInBackground: true,
    retry: retryPolicy,
  });
}

/**
 * Time series for a node over a relative range.
 *
 * The range is resolved server-side so every chart on the page shares one
 * definition of "now" — computing it per panel makes them disagree at the edges
 * and look like a data problem.
 */
export function useMetricSeries(nodeID: string | undefined, metrics: string[], range: string) {
  return useQuery({
    queryKey: nodeKeys.series(nodeID ?? "", metrics, range),
    queryFn: ({ signal }) => {
      const params = new URLSearchParams({
        node: nodeID ?? "",
        metric: metrics.join(","),
        range,
      });
      return apiGet<MetricsResponse>(`/metrics?${params.toString()}`, { signal });
    },
    enabled: Boolean(nodeID) && metrics.length > 0,
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryPolicy,
  });
}

/**
 * The node every page reads by default: the operator's explicit choice from
 * the node switcher, or the control plane's own node before agents exist.
 *
 * Every inventory page describes this host, so resolving the node once here
 * keeps each of them from re-deriving it from the node list.
 */
export function usePrimaryNodeID(): string | undefined {
  const selected = useSelectedNodeID();
  const collectors = useCollectors();
  const nodes = useNodes();

  if (selected) return selected;

  // The collectors endpoint reports the node this Atlas instance is actually
  // collecting for. That is the stable answer.
  //
  // This used to be `nodes[0]`, which the API orders by `last_seen_at DESC` —
  // fine with a single node, and wrong the instant a second one reports. Two
  // live nodes swap places every few seconds, which re-keys every metric query
  // on the page and makes the data blank and return roughly twice a second.
  // Observed exactly that with a second Atlas writing to the same database.
  //
  // The node list is only a fallback, for the window before /collectors
  // answers.
  return collectors.data?.node_id ?? nodes.data?.nodes[0]?.node_id;
}

/**
 * Latest metrics for several nodes at once.
 *
 * There is no bulk endpoint — `/metrics/latest` resolves one node against a
 * five-minute window — so this fans out one request per node and shares the
 * cache with [useLatestMetrics], meaning the node the Overview is already
 * showing is not fetched twice.
 *
 * The fan-out is linear in fleet size. That is fine at the scale Atlas targets
 * today and is the wrong shape for hundreds of nodes; the fix when that
 * arrives is a bulk endpoint on the server, not batching in the browser.
 *
 * A node that has stopped reporting resolves to an empty value list rather
 * than an error, which is what lets the UI distinguish "not reporting" from
 * "query failed".
 */
export function useFleetLatestMetrics(nodeIDs: string[]) {
  return useQueries({
    queries: nodeIDs.map((id) => ({
      queryKey: nodeKeys.latest(id),
      queryFn: ({ signal }: { signal: AbortSignal }) =>
        apiGet<LatestResponse>(`/metrics/latest?node=${encodeURIComponent(id)}`, { signal }),
      refetchInterval: REFRESH_INTERVAL_MS,
      retry: retryPolicy,
    })),
    combine: (results) => ({
      // Indexed by node so callers never depend on array order matching the
      // node list they passed in.
      byNode: new Map(
        results.map((r, i) => [nodeIDs[i] ?? "", r.data?.values ?? emptyArray<LatestValue>()]),
      ),
      isPending: results.some((r) => r.isPending),
      error: results.find((r) => r.error)?.error ?? null,
    }),
  });
}

/** Plugin activation and per-collector health. */
export function useCollectors() {
  return useQuery({
    queryKey: nodeKeys.collectors,
    queryFn: ({ signal }) => apiGet<CollectorsResponse>("/collectors", { signal }),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryPolicy,
  });
}

/**
 * As [retryPolicy], but stops immediately on `not_implemented`.
 *
 * A plugin absent on this host — no Docker, no systemd, no readable
 * crontab — is a permanent property of the host, not a transient failure a
 * retry could fix. Shared by every inventory endpoint whose plugin might not
 * be active here.
 */
function retryUnlessNotImplemented(count: number, error: Error): boolean {
  if (error instanceof ApiError && error.code === "not_implemented") return false;
  return retryPolicy(count, error);
}

export const containerKeys = {
  list: (nodeID: string) => ["containers", nodeID] as const,
  logs: (id: string) => ["containers", id, "logs"] as const,
  detail: (id: string) => ["containers", id, "detail"] as const,
} as const;

/** The container-list request path for nodeID. Exported for testing: this is
 *  the one line that decides whether the page can ever show a remote node's
 *  containers instead of the control plane's own. */
export function containersPath(nodeID: string | undefined): string {
  return `/containers?node=${encodeURIComponent(nodeID ?? "")}`;
}

/**
 * Every container on nodeID, running or not.
 *
 * Without a node param this silently resolved to whatever host the control
 * plane itself runs on — never the node actually selected — which reads as
 * "Docker is not available" the moment that host's own Docker differs from
 * (or is absent, unlike) the selected node's. See containerKeys.list: the
 * query key includes nodeID so switching nodes does not show a stale cache
 * from a different one.
 */
export function useContainers(nodeID: string | undefined) {
  return useQuery({
    queryKey: containerKeys.list(nodeID ?? ""),
    queryFn: ({ signal }) => apiGet<ListContainersResponse>(containersPath(nodeID), { signal }),
    enabled: Boolean(nodeID),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryUnlessNotImplemented,
  });
}

/**
 * One container's configuration.
 *
 * Enabled only when a container is selected: the server answers this with a
 * per-container inspect call on the daemon, so requesting it for a list would
 * multiply Docker's load by the container count. Configuration also changes
 * only when a container is recreated, so this polls far slower than the
 * live-state endpoints.
 */
export function useContainerDetail(containerID: string | null) {
  return useQuery({
    queryKey: containerKeys.detail(containerID ?? ""),
    queryFn: ({ signal }) =>
      apiGet<ContainerDetail>(`/containers/${encodeURIComponent(containerID ?? "")}`, { signal }),
    enabled: Boolean(containerID),
    refetchInterval: REFRESH_INTERVAL_MS * 6,
    retry: retryUnlessNotImplemented,
  });
}

/** Every process on the host, heaviest first. */
export function useProcesses() {
  return useQuery({
    queryKey: ["processes"] as const,
    queryFn: ({ signal }) => apiGet<ListProcessesResponse>("/processes", { signal }),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryUnlessNotImplemented,
  });
}

/** Every systemd unit, failed ones first. */
export function useServices() {
  return useQuery({
    queryKey: ["services"] as const,
    queryFn: ({ signal }) => apiGet<ListServicesResponse>("/services", { signal }),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryUnlessNotImplemented,
  });
}

/**
 * The systemd dependency graph.
 *
 * Rooted requests are the normal case. The full graph on a plain Debian host
 * is 202 nodes and 1,165 edges with a single 236-degree hub, which renders as
 * an unreadable hairball; a rooted, depth-limited, requirement-class view is
 * 4–35 nodes and is the shape this data actually supports.
 *
 * Structure is cached server-side and states are overlaid per request, so this
 * polls at the normal live interval without re-reading unit files.
 */
export function useServiceGraph(params: {
  root?: string | undefined;
  depth?: number | undefined;
  direction?: "dependencies" | "dependents" | undefined;
  edgeClass?: "requirement" | "ordering" | "conflict" | "all" | undefined;
  limit?: number | undefined;
  enabled?: boolean;
}) {
  const { root, depth, direction, edgeClass, limit, enabled = true } = params;

  return useQuery({
    queryKey: ["services", "graph", root ?? "", depth ?? 0, direction ?? "", edgeClass ?? "", limit ?? 0] as const,
    queryFn: ({ signal }) => {
      const q = new URLSearchParams();
      if (root) q.set("root", root);
      if (depth) q.set("depth", String(depth));
      if (direction) q.set("direction", direction);
      if (edgeClass) q.set("class", edgeClass);
      if (limit) q.set("limit", String(limit));
      const suffix = q.toString();
      return apiGet<ServiceGraph>(`/services/graph${suffix ? `?${suffix}` : ""}`, { signal });
    },
    enabled,
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryUnlessNotImplemented,
  });
}

/** One unit with its direct relationships and blast radius. */
export function useServiceDetail(unit: string | null) {
  return useQuery({
    queryKey: ["services", "detail", unit ?? ""] as const,
    queryFn: ({ signal }) =>
      apiGet<ServiceDetail>(`/services/${encodeURIComponent(unit ?? "")}`, { signal }),
    enabled: Boolean(unit),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryUnlessNotImplemented,
  });
}

/** Every readable scheduled job. Crontabs change rarely, so this polls
 *  slower than the live-state endpoints. */
export function useCronJobs() {
  return useQuery({
    queryKey: ["cron"] as const,
    queryFn: ({ signal }) => apiGet<ListCronJobsResponse>("/cron", { signal }),
    refetchInterval: REFRESH_INTERVAL_MS * 6,
    retry: retryUnlessNotImplemented,
  });
}

/** The recent-activity feed. */
export function useActivity(limit = 12) {
  return useQuery({
    queryKey: ["activity", limit] as const,
    queryFn: ({ signal }) => apiGet<ListActivityResponse>(`/activity?limit=${limit}`, { signal }),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryUnlessNotImplemented,
  });
}

/** Every listening port, with certificate detail where TLS was found. */
export function usePorts() {
  return useQuery({
    queryKey: ["ports"] as const,
    queryFn: ({ signal }) => apiGet<ListPortsResponse>("/ports", { signal }),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryUnlessNotImplemented,
  });
}

/** Every mounted filesystem. Capacity moves slowly, so this polls slower
 *  than the live-state endpoints. */
export function useMounts() {
  return useQuery({
    queryKey: ["mounts"] as const,
    queryFn: ({ signal }) => apiGet<ListMountsResponse>("/mounts", { signal }),
    refetchInterval: REFRESH_INTERVAL_MS * 6,
    retry: retryUnlessNotImplemented,
  });
}

/** The tail of one container's logs. */
export function useContainerLogs(containerID: string | null, tail = 200) {
  return useQuery({
    queryKey: containerKeys.logs(containerID ?? ""),
    queryFn: ({ signal }) =>
      apiGet<ContainerLogsResponse>(
        `/containers/${encodeURIComponent(containerID ?? "")}/logs?tail=${tail}`,
        { signal },
      ),
    enabled: Boolean(containerID),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryPolicy,
  });
}
