import { keepPreviousData, useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ApiError,
  apiGet,
  assignRoleAccess,
  createUser,
  disableUser,
  enableUser,
  forceLogout,
  fetchRoleAccessDefinitions,
  fetchUserAudit,
  fetchUserPageAccess,
  fetchUsers,
  grantPageAccess,
  grantRole,
  resetPassword,
  revokePageAccess,
  revokeRole,
  revokeRoleAccess,
} from "./client";
import { emptyArray } from "./empty";
import { useSelectedNodeID } from "../lib/selectedNode";
import type {
  CollectorsResponse,
  ContainerDetail,
  ContainerLogsResponse,
  AssignRoleAccessRequest,
  CreateUserRequest,
  GrantPageAccessRequest,
  GrantRoleRequest,
  ListContainersResponse,
  ListCronJobsResponse,
  ListActivityResponse,
  ListMountsResponse,
  ListPortsResponse,
  ListProcessesResponse,
  ListRoleAccessResponse,
  ListServicesResponse,
  ListUserAuditResponse,
  ListUserPageAccessResponse,
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
/**
 * Picks the default node id from `visibleNodes` — a pure function so the
 * decision is testable without a query client or a rendered hook.
 *
 * Regression this guards: GET /nodes now returns only the nodes the caller
 * is authorized for (see internal/api/v1.Handler.ListNodes), but the
 * default here used to be `collectorNodeID` unconditionally — the node the
 * control-plane process itself runs on. A node-scoped viewer with no grant
 * for that host defaulted onto it anyway, and every subsequent per-node
 * request (Containers, Processes, …) then 403'd against a node the operator
 * never selected and could not see in their own node list.
 */
export function selectPrimaryNodeID(
  visibleNodes: { node_id: string }[],
  collectorNodeID: string | undefined,
  explicitlySelected: string | null,
): string | undefined {
  if (explicitlySelected) return explicitlySelected;

  // The collectors endpoint reports the node this Atlas instance is actually
  // collecting for — stable across the reporting-order churn `nodes[0]`
  // suffers the instant a second node reports (the API orders by
  // `last_seen_at DESC`; two live nodes swap places every few seconds,
  // re-keying every metric query on the page). Preferred only when it is
  // actually one of the nodes the caller can see, per the regression above.
  if (collectorNodeID && visibleNodes.some((n) => n.node_id === collectorNodeID)) {
    return collectorNodeID;
  }
  return visibleNodes[0]?.node_id;
}

export function usePrimaryNodeID(): string | undefined {
  const selected = useSelectedNodeID();
  const collectors = useCollectors();
  const nodes = useNodes();
  return selectPrimaryNodeID(nodes.data?.nodes ?? [], collectors.data?.node_id, selected);
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
  // tail is part of the key so "load older logs" (a bigger tail request) is
  // its own cache entry rather than silently reusing a smaller one.
  logs: (nodeID: string, id: string, tail: number) => ["containers", nodeID, id, "logs", tail] as const,
  detail: (id: string) => ["containers", id, "detail"] as const,
} as const;

/** Scopes an inventory request path to nodeID. Exported for testing: this is
 *  the one line that decides whether a page reads the selected node's
 *  inventory or silently falls back to the control plane's own host. */
export function inventoryPath(base: string, nodeID: string | undefined): string {
  return `${base}?node=${encodeURIComponent(nodeID ?? "")}`;
}

/** The container-list request path for nodeID. */
export function containersPath(nodeID: string | undefined): string {
  return inventoryPath("/containers", nodeID);
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

/**
 * Every process on nodeID, heaviest first.
 *
 * Without a node param this silently resolved to whatever host the control
 * plane runs on rather than the selected node — see [useContainers] for the
 * same regression. The node id is part of the query key so switching nodes
 * cannot show a stale list from a different one.
 */
export function useProcesses(nodeID: string | undefined) {
  return useQuery({
    queryKey: ["processes", nodeID ?? ""] as const,
    queryFn: ({ signal }) =>
      apiGet<ListProcessesResponse>(inventoryPath("/processes", nodeID), { signal }),
    enabled: Boolean(nodeID),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryUnlessNotImplemented,
  });
}

/** Every systemd unit on nodeID, failed ones first. */
export function useServices(nodeID: string | undefined) {
  return useQuery({
    queryKey: ["services", nodeID ?? ""] as const,
    queryFn: ({ signal }) =>
      apiGet<ListServicesResponse>(inventoryPath("/services", nodeID), { signal }),
    enabled: Boolean(nodeID),
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
  node: string | undefined;
  root?: string | undefined;
  depth?: number | undefined;
  direction?: "dependencies" | "dependents" | undefined;
  edgeClass?: "requirement" | "ordering" | "conflict" | "all" | undefined;
  limit?: number | undefined;
  enabled?: boolean;
}) {
  const { node, root, depth, direction, edgeClass, limit, enabled = true } = params;

  return useQuery({
    queryKey: ["services", "graph", node ?? "", root ?? "", depth ?? 0, direction ?? "", edgeClass ?? "", limit ?? 0] as const,
    queryFn: ({ signal }) => {
      const q = new URLSearchParams();
      if (node) q.set("node", node);
      if (root) q.set("root", root);
      if (depth) q.set("depth", String(depth));
      if (direction) q.set("direction", direction);
      if (edgeClass) q.set("class", edgeClass);
      if (limit) q.set("limit", String(limit));
      const suffix = q.toString();
      return apiGet<ServiceGraph>(`/services/graph${suffix ? `?${suffix}` : ""}`, { signal });
    },
    enabled: enabled && Boolean(node),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryUnlessNotImplemented,
  });
}

/** One unit on nodeID with its direct relationships and blast radius. */
export function useServiceDetail(unit: string | null, nodeID: string | undefined) {
  return useQuery({
    queryKey: ["services", "detail", nodeID ?? "", unit ?? ""] as const,
    queryFn: ({ signal }) =>
      apiGet<ServiceDetail>(
        inventoryPath(`/services/${encodeURIComponent(unit ?? "")}`, nodeID),
        { signal },
      ),
    enabled: Boolean(unit) && Boolean(nodeID),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryUnlessNotImplemented,
  });
}

/** Every readable scheduled job on nodeID. Crontabs change rarely, so this
 *  polls slower than the live-state endpoints. */
export function useCronJobs(nodeID: string | undefined) {
  return useQuery({
    queryKey: ["cron", nodeID ?? ""] as const,
    queryFn: ({ signal }) => apiGet<ListCronJobsResponse>(inventoryPath("/cron", nodeID), { signal }),
    enabled: Boolean(nodeID),
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

/** Every listening port on nodeID, with certificate detail where TLS was
 *  found. */
export function usePorts(nodeID: string | undefined) {
  return useQuery({
    queryKey: ["ports", nodeID ?? ""] as const,
    queryFn: ({ signal }) => apiGet<ListPortsResponse>(inventoryPath("/ports", nodeID), { signal }),
    enabled: Boolean(nodeID),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryUnlessNotImplemented,
  });
}

/** Every mounted filesystem on nodeID. Capacity moves slowly, so this polls
 *  slower than the live-state endpoints. */
export function useMounts(nodeID: string | undefined) {
  return useQuery({
    queryKey: ["mounts", nodeID ?? ""] as const,
    queryFn: ({ signal }) => apiGet<ListMountsResponse>(inventoryPath("/mounts", nodeID), { signal }),
    enabled: Boolean(nodeID),
    refetchInterval: REFRESH_INTERVAL_MS * 6,
    retry: retryUnlessNotImplemented,
  });
}

/** The tail of one container's logs. */
export function useContainerLogs(containerID: string | null, nodeID: string | undefined, tail = 200) {
  return useQuery({
    queryKey: containerKeys.logs(nodeID ?? "", containerID ?? "", tail),
    queryFn: ({ signal }) =>
      apiGet<ContainerLogsResponse>(
        `/containers/${encodeURIComponent(containerID ?? "")}/logs?tail=${tail}&node=${encodeURIComponent(nodeID ?? "")}`,
        { signal },
      ),
    enabled: Boolean(containerID),
    refetchInterval: REFRESH_INTERVAL_MS,
    retry: retryPolicy,
    // tail is part of the key, so "load older logs" (a bigger tail) is a new
    // cache entry — without this, data would go briefly undefined while it
    // loads, collapsing the log viewer's height mid-read.
    placeholderData: keepPreviousData,
  });
}

// ------------------------------------------------------- Admin: Users ----
//
// The one page in this application with real write actions — see
// api/client.ts's doc on the /users exception to "Atlas is read-only".
// Every mutation below invalidates userKeys.all on success rather than
// optimistically patching the cache: a grant can trip the last-admin guard,
// so the list the operator sees next must be what the server actually did,
// not what the UI assumed it would do.

export const userKeys = {
  all: ["users"] as const,
  audit: (userID: string) => ["users", userID, "audit"] as const,
  pageAccess: (userID: string) => ["users", userID, "page-access"] as const,
} as const;

/** RoleAccess bundle definitions — one list, shared by every open user
 *  panel's "Assign bundle" picker and its "show the Bundles section at
 *  all" check. */
export const roleAccessKeys = { definitions: ["role-access"] as const } as const;

/** Every user Atlas knows about, with their current active role grants. */
export function useUsers() {
  return useQuery({
    queryKey: userKeys.all,
    queryFn: ({ signal }) => fetchUsers(signal),
  });
}

/** One user's activity history — who granted, revoked, disabled, enabled,
 *  reset, or force-logged-out them, and when. */
export function useUserAudit(userID: string | null) {
  return useQuery({
    queryKey: userKeys.audit(userID ?? ""),
    queryFn: ({ signal }): Promise<ListUserAuditResponse> => fetchUserAudit(userID ?? "", signal),
    enabled: Boolean(userID),
  });
}

function useInvalidateUsers() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: userKeys.all });
  };
}

export function useCreateUser() {
  const invalidate = useInvalidateUsers();
  return useMutation({
    mutationFn: (body: CreateUserRequest) => createUser(body),
    onSuccess: invalidate,
  });
}

export function useGrantRole() {
  const invalidate = useInvalidateUsers();
  return useMutation({
    mutationFn: ({ userID, body }: { userID: string; body: GrantRoleRequest }) => grantRole(userID, body),
    onSuccess: invalidate,
  });
}

export function useRevokeRole() {
  const invalidate = useInvalidateUsers();
  return useMutation({
    mutationFn: ({ userID, grantID }: { userID: string; grantID: string }) => revokeRole(userID, grantID),
    onSuccess: invalidate,
  });
}

export function useDisableUser() {
  const invalidate = useInvalidateUsers();
  return useMutation({
    mutationFn: (userID: string) => disableUser(userID),
    onSuccess: invalidate,
  });
}

export function useEnableUser() {
  const invalidate = useInvalidateUsers();
  return useMutation({
    mutationFn: (userID: string) => enableUser(userID),
    onSuccess: invalidate,
  });
}

export function useResetPassword() {
  return useMutation({
    mutationFn: (userID: string) => resetPassword(userID),
  });
}

export function useForceLogout() {
  return useMutation({
    mutationFn: (userID: string) => forceLogout(userID),
  });
}

// ------------------------------------------- Admin: Page Access ----
//
// A user's page access is a second grant axis, managed on the same admin
// page. Its mutations invalidate that user's page-access cache (which the
// list column's per-user badge and the open panel both read from the same
// key), never userKeys.all — page access does not touch role grants.

/** Every defined RoleAccess bundle. Rarely changes; a long staleTime keeps
 *  it from refetching each time a user panel opens. */
export function useRoleAccessDefinitions() {
  return useQuery({
    queryKey: roleAccessKeys.definitions,
    queryFn: ({ signal }): Promise<ListRoleAccessResponse> => fetchRoleAccessDefinitions(signal),
    staleTime: 5 * 60_000,
  });
}

/** One user's bundle assignments and direct page grants. Only fetched while
 *  a panel is open (userID non-null). */
export function useUserPageAccess(userID: string | null) {
  return useQuery({
    queryKey: userKeys.pageAccess(userID ?? ""),
    queryFn: ({ signal }): Promise<ListUserPageAccessResponse> => fetchUserPageAccess(userID ?? "", signal),
    enabled: Boolean(userID),
  });
}

/**
 * Page access for several users at once, for the list's per-row badge.
 *
 * There is no bulk endpoint, so this fans out one request per visible user
 * and shares each user's cache entry with [useUserPageAccess] — the panel
 * that opens next reads what the badge already fetched. Linear in the number
 * of users shown, which is fine at the scale this admin page targets; the
 * fix if that changes is a count on the /users response, not batching here.
 * Same trade-off [useFleetLatestMetrics] documents.
 */
export function usePageAccessByUser(userIDs: string[]) {
  return useQueries({
    queries: userIDs.map((id) => ({
      queryKey: userKeys.pageAccess(id),
      queryFn: ({ signal }: { signal: AbortSignal }): Promise<ListUserPageAccessResponse> =>
        fetchUserPageAccess(id, signal),
    })),
    combine: (results) => ({
      byUser: new Map(userIDs.map((id, i) => [id, results[i]?.data])),
      isPending: results.some((r) => r.isPending),
    }),
  });
}

function useInvalidateUserPageAccess() {
  const queryClient = useQueryClient();
  return (userID: string) => {
    void queryClient.invalidateQueries({ queryKey: userKeys.pageAccess(userID) });
  };
}

export function useGrantPageAccess() {
  const invalidate = useInvalidateUserPageAccess();
  return useMutation({
    mutationFn: ({ userID, body }: { userID: string; body: GrantPageAccessRequest }) =>
      grantPageAccess(userID, body),
    onSuccess: (_data, { userID }) => { invalidate(userID); },
  });
}

export function useRevokePageAccess() {
  const invalidate = useInvalidateUserPageAccess();
  return useMutation({
    mutationFn: ({ userID, grantID }: { userID: string; grantID: string }) =>
      revokePageAccess(userID, grantID),
    onSuccess: (_data, { userID }) => { invalidate(userID); },
  });
}

export function useAssignRoleAccess() {
  const invalidate = useInvalidateUserPageAccess();
  return useMutation({
    mutationFn: ({ userID, body }: { userID: string; body: AssignRoleAccessRequest }) =>
      assignRoleAccess(userID, body),
    onSuccess: (_data, { userID }) => { invalidate(userID); },
  });
}

export function useRevokeRoleAccess() {
  const invalidate = useInvalidateUserPageAccess();
  return useMutation({
    mutationFn: ({ userID, assignmentID }: { userID: string; assignmentID: string }) =>
      revokeRoleAccess(userID, assignmentID),
    onSuccess: (_data, { userID }) => { invalidate(userID); },
  });
}
