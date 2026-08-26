/**
 * Types mirroring Atlas's version 1 HTTP API.
 *
 * These are hand-written rather than generated. Phase 0 has three endpoints,
 * and a code generator would be more machinery than the surface justifies.
 * That trade flips as the API grows: once the schema is large enough that
 * drift between these types and the Go structs becomes likely, generation
 * from an OpenAPI document is the answer. See docs/api/README.md.
 */

/** Stable error classifications. Mirrors errs.Code in the Go source. */
export type ErrorCode =
  | "invalid_argument"
  | "not_found"
  | "method_not_allowed"
  | "already_exists"
  | "unauthenticated"
  | "permission_denied"
  | "failed_precondition"
  | "rate_limited"
  | "deadline_exceeded"
  | "unavailable"
  | "not_implemented"
  | "internal";

/** The body every failed Atlas request returns. */
export interface ApiErrorBody {
  code: ErrorCode;
  message: string;
  details?: Record<string, unknown>;
  request_id?: string;
}

export interface ApiErrorResponse {
  error: ApiErrorBody;
}

/** Health verdicts. Mirrors health.Status in the Go source. */
export type HealthStatus = "healthy" | "degraded" | "unhealthy";

export interface HealthCheck {
  name: string;
  status: HealthStatus;
  critical: boolean;
  error?: string;
  duration_ms: number;
}

/** GET /api/v1/system/info */
export interface SystemInfo {
  version: string;
  commit: string;
  build_time: string;
  go_version: string;
  platform: string;
  dirty: boolean;
  environment: string;
  started_at: string;
  uptime_seconds: number;
  api_version: string;
}

/**
 * The authenticated caller. Returned by POST /api/v1/auth/login and
 * GET /api/v1/auth/me. Deliberately minimal: it carries only what the
 * authorization layer and the UI need, never a password hash — see
 * internal/core/user.Principal, which this mirrors.
 */
export interface CurrentUser {
  user_id: string;
  username: string;
  /** Display hint for the admin Users page's nav entry — never the
   *  enforcement point; every /users endpoint checks this independently. */
  can_manage_users: boolean;
}

// ------------------------------------------------------- Admin: Users ----

/** The fixed role set — see internal/core/user.KnownRoles. Not an open set:
 *  the backend rejects anything else. */
export type Role = "viewer" | "operator" | "admin";

/** One role grant. Scoped to one node, or fleet-wide when `fleet_wide` is
 *  true — the two are mutually exclusive, mirroring the backend's own
 *  no-default-between-them rule (see GrantRoleRequest). */
export interface Grant {
  id: string;
  role: Role;
  node_id?: string;
  fleet_wide: boolean;
  granted_at: string;
  granted_by?: string;
  revoked_at?: string;
  revoked_by?: string;
}

/** GET /api/v1/users, one entry. */
export interface UserAccount {
  id: string;
  username: string;
  email?: string;
  disabled: boolean;
  created_at: string;
  /** Active grants only — a revoked grant does not appear here. */
  grants?: Grant[];
}

export interface ListUsersResponse {
  users: UserAccount[];
  total: number;
}

/** POST /api/v1/users. There is no password field — the server always
 *  generates one, returned exactly once in the response. */
export interface CreateUserRequest {
  username: string;
  email?: string;
}

export interface CreateUserResponse {
  user: UserAccount;
  password: string;
}

/** POST /api/v1/users/{id}/grants. No default between a specific node and
 *  fleet-wide: exactly one of `node_id` or `fleet_wide: true` is required. */
export interface GrantRoleRequest {
  role: Role;
  node_id?: string;
  fleet_wide?: boolean;
}

export interface ResetPasswordResponse {
  password: string;
}

/** GET /api/v1/users/{id}/audit, one entry — who did what, to whom, when. */
export interface AuditEntry {
  id: string;
  actor_user_id: string;
  actor_username?: string;
  target_user_id: string;
  action:
    | "create_user"
    | "grant_role"
    | "revoke_role"
    | "disable_user"
    | "enable_user"
    | "reset_password"
    | "force_logout";
  detail?: Record<string, unknown>;
  created_at: string;
}

export interface ListUserAuditResponse {
  entries: AuditEntry[];
  total: number;
}

/** GET /api/v1/system/health */
export interface SystemHealth {
  status: HealthStatus;
  checks: HealthCheck[];
  checked_at: string;
  version: string;
  uptime_seconds: number;
}

export interface DatabaseStats {
  acquired_conns: number;
  idle_conns: number;
  total_conns: number;
  max_conns: number;
  empty_acquire_count: number;
  canceled_acquire_count: number;
}

export interface EventBusStats {
  subscribers: number;
  published: number;
  delivered: number;
  dropped: number;
}

export interface ProcessRuntime {
  goroutines: number;
  heap_alloc_mb: number;
  heap_sys_mb: number;
  gc_cycles: number;
  num_cpu: number;
  max_procs: number;
}

/** GET /api/v1/system/runtime */
export interface SystemRuntime {
  database: DatabaseStats;
  event_bus: EventBusStats;
  process: ProcessRuntime;
}

// ---------------------------------------------------------------- Phase 1 ----

/** Liveness of a node, derived from how recently it reported. */
export type NodeStatus = "up" | "stale" | "down";

/** GET /api/v1/nodes/{id} */
export interface Node {
  node_id: string;
  hostname: string;
  os?: string;
  platform?: string;
  kernel?: string;
  architecture?: string;
  cpu_cores?: number;
  agent_version?: string;
  /** Operator-assigned tag. Absent when untagged — Atlas never guesses it. */
  environment?: string;
  status: NodeStatus;
  boot_time?: string;
  uptime_seconds?: number;
  first_seen_at: string;
  last_seen_at: string;
  seconds_since_seen: number;
  clock_skew_seconds?: number;
}

export interface ListNodesResponse {
  nodes: Node[];
  total: number;
}

/** How a value may legitimately be aggregated. */
export type MetricKind = "gauge" | "counter";

/** What a value measures. Drives formatting. */
export type MetricUnit =
  | "count"
  | "percent"
  | "bytes"
  | "bytes_per_second"
  | "seconds"
  | "operations_per_second"
  | "ratio";

/** GET /api/v1/metrics/latest */
export interface LatestValue {
  metric: string;
  collector_id: string;
  unit: MetricUnit;
  kind: MetricKind;
  labels?: Record<string, string>;
  value: number;
  time: string;
}

export interface LatestResponse {
  node_id: string;
  values: LatestValue[];
  total: number;
}

export interface SeriesPoint {
  time: string;
  value: number;
  /** Present only when the query read a rollup rather than raw samples. */
  min?: number;
  max?: number;
}

export interface Series {
  node_id: string;
  collector_id: string;
  metric: string;
  unit: MetricUnit;
  kind: MetricKind;
  labels?: Record<string, string>;
  points: SeriesPoint[];
}

/** Which storage answered a query. A chart from hourly rollups tells a
 *  different story from one at raw resolution. */
export type Resolution = "raw" | "1m" | "1h";

/** GET /api/v1/metrics */
export interface MetricsResponse {
  series: Series[];
  resolution: Resolution;
  from: string;
  to: string;
  truncated: boolean;
}

export interface PluginState {
  id: string;
  name: string;
  subject?: string;
  status: "active" | "not_detected" | "detection_failed" | "init_failed" | "disabled";
  error?: string;
  collectors: number;
}

export interface CollectorHealth {
  collector_id: string;
  name: string;
  interval: string;
  last_run?: string;
  last_success?: string;
  last_error?: string;
  consecutive_failures: number;
  total_runs: number;
  total_failures: number;
  series: number;
  series_limit: number;
  refused_series: number;
  healthy: boolean;
}

/** GET /api/v1/collectors */
export interface CollectorsResponse {
  node_id: string;
  plugins: PluginState[];
  collectors: CollectorHealth[];
  scheduler: {
    collectors: number;
    runs: number;
    failures: number;
    samples: number;
    refused_series: number;
  };
  ingest: { envelopes: number; samples: number; failed: number };
}

// ----------------------------------------------------------- Phase 2 ----

export type ContainerState =
  | "created" | "running" | "paused" | "restarting" | "removing" | "exited" | "dead";

/** Distinct from state: a container can be running and unhealthy. */
export type ContainerHealth = "none" | "starting" | "healthy" | "unhealthy";

export interface Container {
  id: string;
  short_id: string;
  name: string;
  image: string;
  image_id?: string;
  state: ContainerState;
  health: ContainerHealth;
  exit_code?: number;
  restart_count: number;
  created_at?: string;
  started_at?: string;
  finished_at?: string;
  uptime_seconds?: number;
  compose_project?: string;
  compose_service?: string;
  labels?: Record<string, string>;
}

/**
 * One container's configuration, from `GET /api/v1/containers/{id}`.
 *
 * Served by a per-container inspect on the daemon, so it is requested only
 * when the inspector is open rather than for the whole list.
 *
 * There is no environment field, by design rather than omission. Container
 * environments routinely carry database passwords, API tokens and signing
 * keys, and the API deliberately does not serve them — the same boundary that
 * keeps process environments out of the Processes page.
 */
export interface ContainerDetail extends Container {
  entrypoint?: string[];
  command?: string[];
  working_dir?: string;
  user?: string;
  restart_policy: RestartPolicy;
  limits: ContainerLimits;
  mounts?: ContainerMount[];
  networks?: ContainerNetwork[];
  ports?: ContainerPort[];
  platform?: string;
  driver?: string;
}

export interface RestartPolicy {
  /** Docker's policy: "no", "always", "unless-stopped", "on-failure". */
  name: string;
  maximum_retry_count?: number;
}

/** Configured ceilings. Zero means unlimited — Docker's own convention, and a
 *  meaningful answer rather than missing data. */
export interface ContainerLimits {
  nano_cpus: number;
  cpu_shares: number;
  memory_bytes: number;
  memory_swap: number;
  pids_limit: number;
}

export interface ContainerMount {
  type: string;
  name?: string;
  source?: string;
  destination: string;
  driver?: string;
  mode?: string;
  read_write: boolean;
}

export interface ContainerNetwork {
  name: string;
  network_id?: string;
  ip_address?: string;
  gateway?: string;
  mac_address?: string;
  aliases?: string[];
}

/** An absent host_port means the port is exposed by the image but not
 *  published to the host — a different exposure from one bound to an
 *  interface. */
export interface ContainerPort {
  container_port: number;
  protocol: string;
  host_ip?: string;
  host_port?: string;
}

export interface ListContainersResponse {
  containers: Container[];
  total: number;
  running: number;
}

export interface LogLine {
  time: string;
  stream: "stdout" | "stderr";
  message: string;
}

export interface ContainerLogsResponse {
  container_id: string;
  lines: LogLine[];
  total: number;
}

/** Process scheduler state, normalised across platforms. */
export type ProcessState = "running" | "sleeping" | "stopped" | "zombie" | "idle" | "other";

export interface Process {
  pid: number;
  ppid: number;
  name: string;
  executable?: string;
  cmdline?: string;
  username?: string;
  state: ProcessState;
  cpu_percent: number;
  memory_rss: number;
  memory_percent: number;
  threads: number;
  running_for_seconds?: number;
}

/** GET /api/v1/processes */
export interface ListProcessesResponse {
  node_id: string;
  processes: Process[];
  total: number;
  observed_at?: string;
  live?: boolean;
}

export type ActiveState = "active" | "inactive" | "failed" | "activating" | "deactivating" | "unknown";

export interface Service {
  name: string;
  description?: string;
  active_state: ActiveState;
  sub_state: string;
  load_state?: string;
  enabled: boolean;
  failed: boolean;
  running: boolean;
  main_pid?: number;
  restart_count: number;
  active_since?: string;
  uptime_seconds?: number;
  memory_bytes?: number;
}

/**
 * The dependency graph, from `GET /api/v1/services/graph`.
 *
 * Nodes and edges with stable ids, shaped for a layout rather than a table.
 * The server computes health propagation and impact — the rules deciding what
 * "affected" means are the substance of the feature, and re-deriving them here
 * would mean maintaining them twice.
 */
export interface ServiceGraph {
  nodes: GraphNode[];
  edges: GraphEdge[];
  total_nodes: number;
  total_edges: number;
  /** A cap stopped the result short; the view is partial. */
  truncated: boolean;
  /** When the *structure* was read. States are read per request, so this is
   *  not the age of the states shown. */
  collected_at?: string;
  root?: string;
}

export type GraphHealth = "healthy" | "degraded" | "failed" | "inactive" | "unknown";

export interface GraphNode {
  id: string;
  /** Unit suffix: service, target, socket, mount, timer, slice, device. */
  type: string;
  description?: string;
  load_state?: string;
  active_state?: string;
  sub_state?: string;
  health: GraphHealth;
  /** The failed units behind a degraded verdict, so the UI can explain it
   *  rather than only colour it. */
  failed_dependencies?: string[];
  /** False for a unit referenced by an edge that the manager does not have —
   *  a typo in a unit file, or a package removed without its dependents. */
  known: boolean;
  dependencies: number;
  dependents: number;
  /** Hops from the traversal root. Present only on rooted responses. */
  depth?: number;
}

/** Systemd directives, canonicalised to point dependent → dependency. */
export type EdgeKind = "requires" | "wants" | "binds_to" | "part_of" | "after" | "conflicts";
export type EdgeClass = "requirement" | "ordering" | "conflict";

export interface GraphEdge {
  from: string;
  to: string;
  kind: EdgeKind;
  /** What the kind means for failure analysis. Sent by the server so the
   *  client never hard-codes the mapping. */
  class: EdgeClass;
}

/** GET /api/v1/services/{unit} */
export interface ServiceDetail {
  node: GraphNode;
  dependencies: GraphEdge[];
  dependents: GraphEdge[];
  impact: ServiceImpact;
}

/**
 * The blast radius of a unit failing.
 *
 * Hard and soft stay separate: a `Requires` dependent cannot run without this
 * unit, a `Wants` dependent runs without whatever it provides. One number
 * would overstate every outage.
 */
export interface ServiceImpact {
  hard: string[];
  soft: string[];
  truncated: boolean;
}

/** GET /api/v1/services */
export interface ListServicesResponse {
  services: Service[];
  total: number;
  failed: number;
}

export type CronSource = "system" | "cron.d" | "user" | "periodic";

export interface CronJob {
  schedule: string;
  command: string;
  user?: string;
  source: CronSource;
  file?: string;
  line?: number;
  root: boolean;
}

/** GET /api/v1/cron */
export interface ListCronJobsResponse {
  jobs: CronJob[];
  total: number;
  root: number;
}

export type PortProtocol = "tcp" | "udp";

export interface Certificate {
  subject?: string;
  issuer?: string;
  sans?: string[];
  not_before?: string;
  not_after?: string;
  days_until_expiry: number;
  expired: boolean;
  self_signed: boolean;
}

export interface Port {
  protocol: PortProtocol;
  address: string;
  port: number;
  /** Absent, not zero, when Atlas could not resolve the socket's owner —
   *  usually a permission boundary, not a failure. */
  pid?: number;
  process?: string;
  /**
   * Whether the last probe cycle attempted a TLS handshake here.
   *
   * Disambiguates an absent `tls`: probing is budgeted and TCP-only, so a
   * socket with no certificate is either genuinely plaintext or simply not
   * looked at, and those mean opposite things on a security view.
   */
  tls_probed: boolean;
  tls?: Certificate;
}

/** GET /api/v1/ports */
export interface ListPortsResponse {
  ports: Port[];
  total: number;
}

export interface Mount {
  device: string;
  mountpoint: string;
  fstype: string;
  total: number;
  used: number;
  free: number;
  used_percent: number;
  inodes_total?: number;
  inodes_used_percent?: number;
}

/** GET /api/v1/mounts */
export interface ListMountsResponse {
  mounts: Mount[];
  total: number;
}

export type ActivitySeverity = "info" | "success" | "warning" | "danger";

export interface ActivityEntry {
  id: string;
  time: string;
  topic: string;
  severity: ActivitySeverity;
  title: string;
  detail?: string;
  source?: string;
}

/** GET /api/v1/activity */
export interface ListActivityResponse {
  entries: ActivityEntry[];
  total: number;
  /** When this Atlas instance began recording. The feed lives in memory, so
   *  it starts empty after a restart. */
  since: string;
  /** Events the bus dropped because the recorder fell behind. Non-zero means
   *  the feed has gaps. */
  dropped: number;
}
