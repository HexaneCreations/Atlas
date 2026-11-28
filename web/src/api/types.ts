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
  /** Every page this caller can reach, with the scope it applies at. Drives
   *  which nav items show and which routes redirect — a navigation hint
   *  only; every page's data endpoints enforce access independently, and
   *  Overview stays reachable regardless of what this contains (see
   *  shell/pageAccess). Always present; may be empty. */
  page_access: PageAccessEntry[];
  /** True when this caller holds the protected `superadmin` role. The admin
   *  Users page uses it to disable per-row actions against the superadmin's
   *  account for everyone else — a display hint only; the backend re-checks
   *  it on every action endpoint (guardSuperadminTarget). */
  is_superadmin: boolean;
  /** True when this caller holds `notifications.manage` (superadmin only).
   *  A display hint only; every /notifications endpoint checks it itself. */
  can_manage_notifications: boolean;
  /** Display hint for the Nodes page's "Add server" action — never the
   *  enforcement point; every /fleet/install-commands endpoint checks
   *  `fleet.enroll` independently. */
  can_enroll_fleet: boolean;
}

/** One reachable page in [CurrentUser.page_access]. `fleet_wide` true means
 *  every node; otherwise `node_ids` names the nodes it applies to. */
export interface PageAccessEntry {
  page: Page;
  fleet_wide: boolean;
  node_ids?: string[];
}

// ------------------------------------------------------- Admin: Users ----

/** The fixed role set — see internal/core/user.KnownRoles. Not an open set:
 *  the backend rejects anything else. `superadmin` is a protected tier that
 *  cannot be granted through the API (POST /users/{id}/grants rejects it);
 *  it appears here only so an existing grant can be read and rendered. */
export type Role = "viewer" | "operator" | "admin" | "superadmin";

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
  /** Initial access, applied in the same transaction as the insert; a
   *  refused body creates no user. Omitted, the user starts with none. */
  access?: AccessBody;
}

/** The roles an access-editor row may hold — never superadmin. */
export type EditableRole = "viewer" | "operator" | "admin";

/** One requested role or direct page grant. Exactly one of `node_id` and
 *  `fleet_wide: true` — the server has no default between them. */
export interface AccessRoleRow {
  role: EditableRole;
  node_id?: string;
  fleet_wide: boolean;
}

export interface AccessPageRow {
  page: Page;
  node_id?: string;
  fleet_wide: boolean;
}

/** A user's full editable access: every role and direct page grant they
 *  should hold. Bundles and superadmin are not part of it. */
export interface AccessBody {
  roles: AccessRoleRow[];
  pages: AccessPageRow[];
}

/** PUT /api/v1/users/{id}/access — replaces the editable access in full. */
export interface PutUserAccessRequest extends AccessBody {
  version: string;
}

export interface AccessRoleGrant {
  id: string;
  role: EditableRole;
  node_id?: string;
  fleet_wide: boolean;
  granted_at: string;
  granted_by?: string;
}

export interface AccessPageGrant {
  id: string;
  page: Page;
  node_id?: string;
  fleet_wide: boolean;
  granted_at: string;
  granted_by?: string;
}

/** A bundle assignment and the pages it covers. Read-only in the editor. */
export interface AccessBundle {
  id: string;
  role_access: string;
  node_id?: string;
  fleet_wide: boolean;
  pages: Page[];
  granted_at: string;
  granted_by?: string;
}

/** GET /api/v1/users/{id}/access. Superadmin grants are never included. */
export interface UserAccessResponse {
  version: string;
  roles: AccessRoleGrant[];
  pages: AccessPageGrant[];
  bundles: AccessBundle[];
}

export interface AccessChange {
  kind: "role" | "page";
  /** Set on revokes: the row that was revoked. */
  id?: string;
  role?: EditableRole;
  page?: Page;
  node_id?: string;
  fleet_wide: boolean;
}

export interface PutUserAccessResponse {
  access: UserAccessResponse;
  changes: { granted: AccessChange[]; revoked: AccessChange[] };
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
    | "force_logout"
    // Page-access actions share the same user_audit_log — see
    // internal/core/pageauthz's Audit* constants.
    | "create_role_access"
    | "add_page_to_role_access"
    | "remove_page_from_role_access"
    | "assign_role_access"
    | "revoke_role_access"
    | "grant_page_access"
    | "revoke_page_access";
  detail?: Record<string, unknown>;
  created_at: string;
}

export interface ListUserAuditResponse {
  entries: AuditEntry[];
  total: number;
}

// --------------------------------------------------- Admin: Page Access ----
//
// A second, independent access axis from role grants (see internal/core/
// pageauthz): which pages a user may even reach, per scope. Mirrors
// internal/api/v1's pageaccess.go response shapes.

/** The fixed set of gateable pages — see internal/core/pageauthz.KnownPages
 *  and web/src/shell/pages.ts, which it mirrors. */
export type Page =
  | "overview"
  | "nodes"
  | "containers"
  | "processes"
  | "services"
  | "cron"
  | "ports"
  | "disks"
  | "users"
  | "websites";

/**
 * One page-access grant: a direct page grant (`page` set) or a RoleAccess
 * bundle assignment (`role_access` set) — the backend presents both as this
 * one shape (internal/api/v1.AssignmentResponse). Scoped to one node, or
 * fleet-wide.
 */
export interface PageAccessAssignment {
  id: string;
  node_id?: string;
  fleet_wide: boolean;
  granted_at: string;
  granted_by?: string;
  revoked_at?: string;
  revoked_by?: string;
  /** Set for a bundle assignment; empty for a direct page grant. */
  role_access?: string;
  /** Set for a direct page grant; empty for a bundle assignment. */
  page?: Page;
}

/** GET /api/v1/users/{id}/page-access — one user's full page-access picture. */
export interface ListUserPageAccessResponse {
  role_access: PageAccessAssignment[];
  pages: PageAccessAssignment[];
}

/** One reusable page bundle — GET /api/v1/role-access, one entry. */
export interface RoleAccessDefinition {
  name: string;
  pages: Page[];
}

export interface ListRoleAccessResponse {
  role_access: RoleAccessDefinition[];
  total: number;
}

/** POST /api/v1/users/{id}/page-access. Exactly one of `node_id` or
 *  `fleet_wide: true`, same no-default rule as GrantRoleRequest. */
export interface GrantPageAccessRequest {
  page: Page;
  node_id?: string;
  fleet_wide?: boolean;
}

/** POST /api/v1/users/{id}/role-access. */
export interface AssignRoleAccessRequest {
  role_access_name: string;
  node_id?: string;
  fleet_wide?: boolean;
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
  /** The source address the control plane observed this node's most recent
   *  connection from — server-observed, not agent-reported. Absent for the
   *  local node Atlas runs on, and for a node that has not connected over a
   *  path that captures it. For a node behind NAT, this is the NAT egress
   *  address, not the host's own address. */
  public_ip?: string;
  /** null: not muted. [MUTED_INDEFINITELY]: until unmuted. Otherwise when the
   *  mute ends. */
  alerts_muted_until: string | null;
  archived_at: string | null;
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

// --------------------------------------------------------- Notifications ----

export type NotificationChannelType = "email" | "teams" | "webhook";
export type NotificationSeverity = "warning" | "critical";

/** GET /api/v1/notifications/channels/{id}. The destination URL and header
 *  values are never served: only the URL's host and the headers' names. Both
 *  are empty when the stored secret cannot be read. */
export interface NotificationChannel {
  id: string;
  name: string;
  type: NotificationChannelType;
  enabled: boolean;
  min_severity: NotificationSeverity;
  /** The non-secret settings: `template` and `content_type` for a webhook. */
  config: Record<string, unknown>;
  secret_configured: boolean;
  /** Pre-selected for new website monitors. */
  monitor_default: boolean;
  destination_host: string;
  header_names: string[];
  created_at: string;
  updated_at: string;
}

/** GET /api/v1/notifications/channels */
export interface ListNotificationChannelsResponse {
  channels: NotificationChannel[];
  total: number;
}

/** POST /api/v1/notifications/channels, PUT /api/v1/notifications/channels/{id} */
export interface NotificationChannelRequest {
  name: string;
  type: NotificationChannelType;
  enabled: boolean;
  min_severity: NotificationSeverity;
  config: Record<string, unknown>;
  /** Write-only JSON text, `{"url", "headers"}`. Omitted on update, the stored
   *  one is kept; with an empty url the stored url is kept and so is the value
   *  of any header left empty. */
  secret?: string;
  /** Omitted on update keeps the stored value. */
  monitor_default?: boolean;
}

/** POST /api/v1/notifications/channels/test */
export interface TestNotificationDraftRequest extends NotificationChannelRequest {
  /** The saved channel whose stored secret fills in what `secret` leaves out. */
  channel_id?: string;
}

/** `error` is one of the server's fixed category messages, empty when ok. */
export interface TestNotificationResponse {
  ok: boolean;
  error: string;
}

export interface NotificationDelivery {
  id: string;
  event_id: string;
  channel_id: string;
  channel_type: NotificationChannelType;
  kind: "firing" | "escalated" | "recovered" | "flapping" | "test";
  trigger: string;
  node_id?: string;
  subject?: string;
  severity?: string;
  title?: string;
  message?: string;
  status: "pending" | "delivered" | "failed";
  attempts: number;
  next_attempt_at?: string;
  last_error?: string;
  delivered_at?: string;
  created_at: string;
  updated_at: string;
}

/** GET /api/v1/notifications/deliveries. `total` is this page's length. */
export interface ListNotificationDeliveriesResponse {
  deliveries: NotificationDelivery[];
  total: number;
}

export type MuteDuration = "1h" | "4h" | "24h" | "7d" | "until_unmuted";

/** PUT /api/v1/nodes/{nodeID}/alerts-mute */
export interface MuteNodeRequest {
  duration: MuteDuration;
}

/** Where a node's effective alert rule value comes from. */
export type AlertRuleSource = "node" | "monitor" | "global" | "catalog";

/** One catalog alert rule as it applies to a node, or to a website monitor. */
export interface NodeAlertRule {
  key: string;
  area: string;
  title: string;
  description: string;
  default_severity: NotificationSeverity;
  default_enabled: boolean;
  /** The global rule's value; null when there is none. */
  global_enabled: boolean | null;
  /** The node's own value; null when it inherits. */
  override: boolean | null;
  /** The effective value. */
  enabled: boolean;
  source: AlertRuleSource;
}

/** GET /api/v1/nodes/{id}/alert-rules. */
export interface ListNodeAlertRulesResponse {
  node_id: string;
  rules: NodeAlertRule[];
}

/** The answer to a mute. */
export interface NodeAlertsResponse {
  node_id: string;
  alerts_muted_until: string | null;
  archived_at: string | null;
}

// --------------------------------------------------- Fleet: install codes ----
//
// "Add server": a one-time code an admin generates, which install.sh
// redeems to enroll a new machine with no manual token or peer
// authorization. Gated by `fleet.enroll` — see [CurrentUser.can_enroll_fleet].

export type InstallCodeStatus = "pending" | "redeemed" | "expired" | "revoked";

/** POST /api/v1/fleet/install-commands body. */
export interface CreateInstallCommandRequest {
  environment: string;
  /** Minutes until expiry; omit for the server's default (30, max 60). */
  ttl_minutes?: number;
}

/** The only place the install code's plaintext ever appears — embedded in
 *  `command`, never returned again. */
export interface CreateInstallCommandResponse {
  id: string;
  node_id: string;
  expires_at: string;
  command: string;
}

/** One install code as listed — never carries the code itself. */
export interface InstallCommand {
  id: string;
  node_id: string;
  environment: string;
  created_by: string;
  status: InstallCodeStatus;
  created_at: string;
  expires_at: string;
  revoked_at?: string;
  revoked_by?: string;
  redeemed_at?: string;
  redeemed_peer_id?: string;
  redeemed_ip?: string;
}

/** GET /api/v1/fleet/install-commands */
export interface ListInstallCommandsResponse {
  install_commands: InstallCommand[];
  total: number;
}

// ------------------------------------------------------ Website monitors ----
// internal/api/v1's monitors.go response shapes.

export type MonitorStatus = "unknown" | "up" | "pending" | "down";
/** Why a check failed: a fixed category, never text from the site. */
export type MonitorFailure = "" | "dns" | "connect" | "tls" | "timeout" | "http_status" | "keyword" | "redirect" | "blocked";
export type MonitorMethod = "GET" | "HEAD" | "POST";
export type MonitorAuthType = "none" | "basic" | "bearer";
export type MonitorBodyEncoding = "json" | "form" | "text";

export interface MonitorHeartbeat {
  time: string;
  status: MonitorStatus;
  status_code: number | null;
  response_ms: number | null;
  failure: MonitorFailure;
}

export interface MonitorLive {
  status: MonitorStatus;
  status_since: string;
  down_since: string | null;
  consecutive_failures: number;
  last_check_at: string | null;
  last_status_code: number | null;
  last_response_ms: number | null;
  last_failure: MonitorFailure;
  next_check_at: string;
  cert_expires_at: string | null;
}

/**
 * One monitor. Without notifications.manage the server redacts it: `url`
 * loses its query string, and `auth_type`, `header_names`,
 * `secret_configured`, `created_by` and `updated_by` are absent.
 */
export interface Monitor {
  id: string;
  name: string;
  type: "http";
  url: string;
  host: string;
  method: MonitorMethod;
  interval_seconds: number;
  retries: number;
  retry_interval_seconds: number;
  timeout_seconds: number;
  max_redirects: number;
  accepted_status: string[];
  body_encoding: MonitorBodyEncoding;
  keyword: string;
  recovery_successes: number;
  cert_notify: boolean;
  cert_warn_days: number;
  cert_crit_days: number;
  group_id: string;
  description: string;
  tags: string[];
  active: boolean;
  alerts_muted_until: string | null;
  auth_type?: MonitorAuthType;
  header_names?: string[];
  secret_configured?: boolean;
  created_at: string;
  created_by?: string;
  updated_at: string;
  updated_by?: string;
  redacted: boolean;
  /** False when no channel is selected: the monitor notifies no one. */
  notifications_configured: boolean;
  /** The selected channels; only for readers who manage notifications. */
  channel_ids?: string[];
  live: MonitorLive;
  /** Absent when nothing was counted in the last day. */
  uptime_24h?: number | null;
  /** The newest checks, oldest first; only in the list. */
  heartbeats?: MonitorHeartbeat[];
}

export interface MonitorGroup {
  id: string;
  name: string;
  position: number;
}

export interface MonitorProbeState {
  offline: boolean;
  since: string | null;
}

/** GET /api/v1/monitors */
export interface ListMonitorsResponse {
  monitors: Monitor[];
  groups: MonitorGroup[];
  probe: MonitorProbeState;
}

/** GET /api/v1/monitors/{id}: null where nothing was counted. */
export interface MonitorDetail extends Monitor {
  uptime: { day: number | null; week: number | null; month: number | null };
}

export interface MonitorHeartbeatsResponse {
  monitor_id: string;
  heartbeats: MonitorHeartbeat[];
}

export type MonitorEventKind = "down" | "up" | "cert_warn" | "cert_crit" | "cert_renewed";

export interface MonitorEvent {
  id: string;
  time: string;
  kind: MonitorEventKind;
  status_code: number | null;
  failure: MonitorFailure;
  duration_seconds: number | null;
  /** Set on the certificate events. */
  cert_expires_at: string | null;
}

export interface MonitorEventsResponse {
  monitor_id: string;
  events: MonitorEvent[];
}

/** A null value keeps the stored value for that header name. */
export interface MonitorHeaderRequest {
  name: string;
  value: string | null;
}

/**
 * POST /api/v1/monitors and PUT /api/v1/monitors/{id}. An omitted field
 * takes its default on create and keeps its value on update; an omitted
 * secret field keeps what is stored, and `headers` replaces the whole set.
 */
export interface MonitorRequest {
  name?: string;
  url?: string;
  method?: MonitorMethod;
  interval_seconds?: number;
  retries?: number;
  retry_interval_seconds?: number;
  timeout_seconds?: number;
  max_redirects?: number;
  accepted_status?: string[];
  body_encoding?: MonitorBodyEncoding;
  keyword?: string;
  cert_notify?: boolean;
  cert_warn_days?: number;
  cert_crit_days?: number;
  group_id?: string;
  description?: string;
  tags?: string[];
  auth_type?: MonitorAuthType;
  headers?: MonitorHeaderRequest[];
  basic_username?: string;
  basic_password?: string;
  bearer_token?: string;
  body?: string;
  /** Omitted on create selects the monitor_default channels; on update,
   *  keeps the selection. */
  channel_ids?: string[];
}

/** PUT /api/v1/monitors/{id}/channels */
export interface MonitorChannelsResponse {
  monitor_id: string;
  channel_ids: string[];
}

/** GET /api/v1/monitors/{id}/alert-rules */
export interface ListMonitorAlertRulesResponse {
  monitor_id: string;
  rules: NodeAlertRule[];
}

/** The answer to PUT /api/v1/monitors/{id}/mute. */
export interface MonitorMuteResponse {
  monitor_id: string;
  alerts_muted_until: string | null;
}

/** POST /api/v1/monitors/test: a draft, plus the saved monitor whose
 *  configuration and secrets fill in what it leaves out. */
export interface TestMonitorRequest extends MonitorRequest {
  monitor_id?: string;
}

/** The whole of a test's outcome. */
export interface TestMonitorResponse {
  status: "up" | "down";
  failure: MonitorFailure;
  status_code: number | null;
  response_ms: number | null;
}

export interface MonitorGroupRequest {
  name?: string;
  position?: number;
}
