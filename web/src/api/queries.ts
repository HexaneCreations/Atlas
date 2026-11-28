import {
  keepPreviousData,
  useInfiniteQuery,
  useMutation,
  useQueries,
  useQuery,
  useQueryClient,
  type QueryClient,
} from "@tanstack/react-query";
import {
  ApiError,
  apiGet,
  assignRoleAccess,
  fetchUserAccess,
  putUserAccess,
  createInstallCommand,
  createMonitor,
  createMonitorGroup,
  createNotificationChannel,
  deleteMonitor,
  deleteMonitorGroup,
  fetchMonitor,
  fetchMonitorEvents,
  fetchMonitorHeartbeats,
  fetchMonitors,
  fetchMonitorAlertRules,
  muteMonitor,
  resetMonitorAlertRule,
  setMonitorActive,
  setMonitorAlertRule,
  setMonitorChannels,
  unmuteMonitor,
  updateMonitor,
  updateMonitorGroup,
  createUser,
  deleteNotificationChannel,
  disableUser,
  enableUser,
  fetchInstallCommands,
  forceLogout,
  fetchNotificationChannels,
  fetchNotificationDeliveries,
  fetchRoleAccessDefinitions,
  fetchUserAudit,
  fetchUserPageAccess,
  fetchUsers,
  grantPageAccess,
  grantRole,
  muteNodeAlerts,
  resetPassword,
  revokeInstallCommand,
  revokePageAccess,
  revokeRole,
  revokeRoleAccess,
  unmuteNodeAlerts,
  fetchNodeAlertRules,
  resetNodeAlertRule,
  setNodeAlertRule,
  updateNotificationChannel,
} from "./client";
import { emptyArray } from "./empty";
import { useSelectedNodeID } from "../lib/selectedNode";
import { applyOverride } from "../lib/alertRules";
import type {
  ListMonitorAlertRulesResponse,
  ListNodeAlertRulesResponse,
  NodeAlertRule,
  PutUserAccessRequest,
  UserAccessResponse,
  CollectorsResponse,
  ContainerDetail,
  ContainerLogsResponse,
  LogRange,
  AssignRoleAccessRequest,
  CreateInstallCommandRequest,
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
  MuteDuration,
  MonitorGroupRequest,
  MonitorRequest,
  NotificationChannelRequest,
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
  logRange: (nodeID: string, id: string, since: string, until: string) =>
    ["containers", nodeID, id, "logs", since, until] as const,
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

/** Lines per page of a ranged log read. */
export const LOG_PAGE_LIMIT = 500;

/** The query string of one page of a log range. */
export function logRangeQuery(range: LogRange, nodeID: string | undefined, limit: number, before?: string): string {
  const q = new URLSearchParams({ since: range.since, limit: String(limit), node: nodeID ?? "" });
  if (range.until) q.set("until", range.until);
  if (before) q.set("before", before);
  return q.toString();
}

/** One container's logs for a time range: the newest page first, then older
 *  pages by cursor ("load more"). Not refetched on a timer: a range that
 *  ends now stays current through the live follow instead. */
export function useContainerLogRange(containerID: string | null, nodeID: string | undefined, range: LogRange) {
  return useInfiniteQuery({
    queryKey: containerKeys.logRange(nodeID ?? "", containerID ?? "", range.since, range.until ?? "now"),
    queryFn: ({ signal, pageParam }) =>
      apiGet<ContainerLogsResponse>(
        `/containers/${encodeURIComponent(containerID ?? "")}/logs?${logRangeQuery(range, nodeID, LOG_PAGE_LIMIT, pageParam)}`,
        { signal },
      ),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => (last.more && last.next_before ? last.next_before : undefined),
    enabled: Boolean(containerID),
    retry: retryPolicy,
    staleTime: Infinity,
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
  access: (userID: string) => ["users", userID, "access"] as const,
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

/** One user's access for the access editor. */
export function useUserAccess(userID: string | null) {
  return useQuery({
    queryKey: userKeys.access(userID ?? ""),
    queryFn: ({ signal }): Promise<UserAccessResponse> => fetchUserAccess(userID ?? "", signal),
    enabled: Boolean(userID),
  });
}

/** Saves the access editor's draft. Invalidates every user query (the list's
 *  role chips, page-access badges, the audit trail, and this user's access). */
export function usePutUserAccess() {
  const invalidate = useInvalidateUsers();
  return useMutation({
    mutationFn: ({ userID, body }: { userID: string; body: PutUserAccessRequest }) => putUserAccess(userID, body),
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

// --------------------------------------------------------- Notifications ----

export const notificationKeys = {
  channels: ["notifications", "channels"] as const,
  deliveries: ["notifications", "deliveries"] as const,
} as const;

/** Whether a notifications request failed because the server has no
 *  encryption key: a setting to change, not an outage to retry. */
export function notificationsDisabled(error: unknown): boolean {
  return error instanceof ApiError && error.status === 503 && error.message.includes("no encryption key");
}

function notificationsRetryPolicy(failureCount: number, error: Error): boolean {
  return !notificationsDisabled(error) && retryPolicy(failureCount, error);
}

export function useNotificationChannels() {
  return useQuery({
    queryKey: notificationKeys.channels,
    queryFn: ({ signal }) => fetchNotificationChannels(signal),
    retry: notificationsRetryPolicy,
  });
}

/** The newest `limit` delivery attempts. */
export function useNotificationDeliveries(limit: number) {
  return useQuery({
    queryKey: [...notificationKeys.deliveries, limit] as const,
    queryFn: ({ signal }) => fetchNotificationDeliveries(limit, signal),
    placeholderData: keepPreviousData,
    refetchInterval: 15_000,
    retry: notificationsRetryPolicy,
  });
}

function useInvalidateNotifications() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: notificationKeys.channels });
    void queryClient.invalidateQueries({ queryKey: notificationKeys.deliveries });
  };
}

/** Creates a channel, or updates it when `channelID` is given. */
export function useSaveNotificationChannel() {
  const invalidate = useInvalidateNotifications();
  return useMutation({
    mutationFn: ({ channelID, body }: { channelID: string | undefined; body: NotificationChannelRequest }) =>
      channelID ? updateNotificationChannel(channelID, body) : createNotificationChannel(body),
    // The variables hold the URL and header values: not kept once unused.
    gcTime: 0,
    onSuccess: invalidate,
  });
}

export function useDeleteNotificationChannel() {
  const invalidate = useInvalidateNotifications();
  return useMutation({
    mutationFn: (channelID: string) => deleteNotificationChannel(channelID),
    onSuccess: invalidate,
  });
}

/** Mutes a node's alerts, or unmutes it when `duration` is null. */
export function useMuteNode() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ nodeID, duration }: { nodeID: string; duration: MuteDuration | null }) => {
      if (duration) await muteNodeAlerts(nodeID, { duration });
      else await unmuteNodeAlerts(nodeID);
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: nodeKeys.all, exact: true }),
  });
}

export const nodeAlertRuleKeys = {
  node: (nodeID: string) => ["node-alert-rules", nodeID] as const,
};

/** Every catalog alert rule with its effective value on one node. */
export function useNodeAlertRules(nodeID: string) {
  return useQuery({
    queryKey: nodeAlertRuleKeys.node(nodeID),
    queryFn: ({ signal }) => fetchNodeAlertRules(nodeID, signal),
  });
}

/** The mutation behind [useSetNodeAlertRule], separate so it can be
 *  exercised against a bare QueryClient. */
export function nodeAlertRuleMutation(queryClient: QueryClient, nodeID: string) {
  return alertRuleMutation<ListNodeAlertRulesResponse>(queryClient, nodeAlertRuleKeys.node(nodeID), "node",
    (key) => resetNodeAlertRule(nodeID, key), (key, enabled) => setNodeAlertRule(nodeID, key, enabled));
}

/** A subject's rule override change, applied to its cached list at once and
 *  put back if the server refuses. */
function alertRuleMutation<T extends { rules: NodeAlertRule[] }>(
  queryClient: QueryClient,
  key: readonly unknown[],
  own: "node" | "monitor",
  reset: (ruleKey: string) => Promise<unknown>,
  set: (ruleKey: string, enabled: boolean) => Promise<unknown>,
) {
  return {
    mutationFn: async ({ ruleKey, override }: { ruleKey: string; override: boolean | null }) => {
      if (override === null) await reset(ruleKey);
      else await set(ruleKey, override);
    },
    onMutate: async ({ ruleKey, override }: { ruleKey: string; override: boolean | null }) => {
      await queryClient.cancelQueries({ queryKey: key });
      const previous = queryClient.getQueryData<T>(key);
      if (previous) {
        queryClient.setQueryData<T>(key, {
          ...previous,
          rules: previous.rules.map((r) => (r.key === ruleKey ? applyOverride(r, override, own) : r)),
        });
      }
      return { previous };
    },
    onError: (_error: unknown, _vars: unknown, context: { previous: T | undefined } | undefined) => {
      if (context?.previous) queryClient.setQueryData(key, context.previous);
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: key }),
  };
}

/** Sets (true/false) or clears (null) one rule's override on a node. The
 *  list updates at once and is put back if the server refuses. */
export function useSetNodeAlertRule(nodeID: string) {
  const queryClient = useQueryClient();
  return useMutation(nodeAlertRuleMutation(queryClient, nodeID));
}

// ------------------------------------------------- Fleet: install codes ----

export const installCommandKeys = { all: ["install-commands"] as const };

/** Every install code Atlas knows about, without any code — the "Add
 *  server" panel's list. */
export function useInstallCommands() {
  return useQuery({
    queryKey: installCommandKeys.all,
    queryFn: ({ signal }) => fetchInstallCommands(signal),
  });
}

function useInvalidateInstallCommands() {
  const queryClient = useQueryClient();
  return () => { void queryClient.invalidateQueries({ queryKey: installCommandKeys.all }); };
}

/** Generates a one-time install code. The response's `command` is the only
 *  place its plaintext ever appears — see the "Add server" reveal modal. */
export function useCreateInstallCommand() {
  const invalidate = useInvalidateInstallCommands();
  return useMutation({
    mutationFn: (body: CreateInstallCommandRequest) => createInstallCommand(body),
    onSuccess: invalidate,
  });
}

export function useRevokeInstallCommand() {
  const invalidate = useInvalidateInstallCommands();
  return useMutation({
    mutationFn: (id: string) => revokeInstallCommand(id),
    onSuccess: invalidate,
  });
}

// ------------------------------------------------------ Website monitors ----

/** How often the Websites page re-reads monitors: a check runs at most
 *  every 20s, so this shows each one within half an interval. */
export const MONITOR_REFRESH_MS = 10_000;

export const monitorKeys = {
  all: ["monitors"] as const,
  list: ["monitors", "list"] as const,
  detail: (id: string) => ["monitors", "detail", id] as const,
  heartbeats: (id: string) => ["monitors", "heartbeats", id] as const,
  events: (id: string) => ["monitors", "events", id] as const,
  alertRules: (id: string) => ["monitors", "alert-rules", id] as const,
};

export function useMonitors() {
  return useQuery({
    queryKey: monitorKeys.list,
    queryFn: ({ signal }) => fetchMonitors(signal),
    refetchInterval: MONITOR_REFRESH_MS,
  });
}

export function useMonitor(id: string) {
  return useQuery({
    queryKey: monitorKeys.detail(id),
    queryFn: ({ signal }) => fetchMonitor(id, signal),
    refetchInterval: MONITOR_REFRESH_MS,
  });
}

/** The last day of raw checks, for the response time chart. */
export function useMonitorHeartbeats(id: string) {
  return useQuery({
    queryKey: monitorKeys.heartbeats(id),
    queryFn: ({ signal }) => fetchMonitorHeartbeats(id, new Date(Date.now() - 86_400_000).toISOString(), signal),
    refetchInterval: MONITOR_REFRESH_MS * 3,
  });
}

export function useMonitorEvents(id: string) {
  return useQuery({
    queryKey: monitorKeys.events(id),
    queryFn: ({ signal }) => fetchMonitorEvents(id, signal),
    refetchInterval: MONITOR_REFRESH_MS * 3,
  });
}

function useInvalidateMonitors() {
  const queryClient = useQueryClient();
  return () => queryClient.invalidateQueries({ queryKey: monitorKeys.all });
}

/** Creates a monitor, or updates it when `monitorID` is given. */
export function useSaveMonitor() {
  const invalidate = useInvalidateMonitors();
  return useMutation({
    mutationFn: ({ monitorID, body }: { monitorID: string | undefined; body: MonitorRequest }) =>
      monitorID ? updateMonitor(monitorID, body) : createMonitor(body),
    // The variables hold credentials, header values and the body.
    gcTime: 0,
    onSuccess: invalidate,
  });
}

export function useSetMonitorActive() {
  const invalidate = useInvalidateMonitors();
  return useMutation({
    mutationFn: ({ monitorID, active }: { monitorID: string; active: boolean }) => setMonitorActive(monitorID, active),
    onSuccess: invalidate,
  });
}

/** Every monitor alert rule with its effective value on one monitor. */
export function useMonitorAlertRules(monitorID: string) {
  return useQuery({
    queryKey: monitorKeys.alertRules(monitorID),
    queryFn: ({ signal }) => fetchMonitorAlertRules(monitorID, signal),
  });
}

/** The mutation behind [useSetMonitorAlertRule]. */
export function monitorAlertRuleMutation(queryClient: QueryClient, monitorID: string) {
  return alertRuleMutation<ListMonitorAlertRulesResponse>(queryClient, monitorKeys.alertRules(monitorID), "monitor",
    (key) => resetMonitorAlertRule(monitorID, key), (key, enabled) => setMonitorAlertRule(monitorID, key, enabled));
}

/** Sets (true/false) or clears (null) one rule's override on a monitor. */
export function useSetMonitorAlertRule(monitorID: string) {
  const queryClient = useQueryClient();
  return useMutation(monitorAlertRuleMutation(queryClient, monitorID));
}

/** Mutes a monitor's alerts, or unmutes it when `duration` is null. */
export function useMuteMonitor() {
  const invalidate = useInvalidateMonitors();
  return useMutation({
    mutationFn: async ({ monitorID, duration }: { monitorID: string; duration: MuteDuration | null }) => {
      if (duration) await muteMonitor(monitorID, { duration });
      else await unmuteMonitor(monitorID);
    },
    onSuccess: invalidate,
  });
}

/** Replaces a monitor's channel selection. */
export function useSetMonitorChannels() {
  const invalidate = useInvalidateMonitors();
  return useMutation({
    mutationFn: ({ monitorID, channelIDs }: { monitorID: string; channelIDs: string[] }) => setMonitorChannels(monitorID, channelIDs),
    onSuccess: invalidate,
  });
}

export function useDeleteMonitor() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (monitorID: string) => deleteMonitor(monitorID),
    onSuccess: (_data, monitorID) => {
      queryClient.removeQueries({ queryKey: monitorKeys.detail(monitorID) });
      return queryClient.invalidateQueries({ queryKey: monitorKeys.list });
    },
  });
}

/** Creates a group, or renames it when `groupID` is given. */
export function useSaveMonitorGroup() {
  const invalidate = useInvalidateMonitors();
  return useMutation({
    mutationFn: ({ groupID, body }: { groupID: string | undefined; body: MonitorGroupRequest }) =>
      groupID ? updateMonitorGroup(groupID, body) : createMonitorGroup(body),
    onSuccess: invalidate,
  });
}

export function useDeleteMonitorGroup() {
  const invalidate = useInvalidateMonitors();
  return useMutation({
    mutationFn: (groupID: string) => deleteMonitorGroup(groupID),
    onSuccess: invalidate,
  });
}
