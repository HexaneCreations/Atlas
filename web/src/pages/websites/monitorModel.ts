import { ApiError } from "../../api/client";
import type {
  Monitor,
  MonitorAuthType,
  MonitorHeartbeat,
  MonitorBodyEncoding,
  MonitorFailure,
  MonitorGroup,
  MonitorHeaderRequest,
  MonitorMethod,
  MonitorRequest,
  MonitorStatus,
  NotificationChannel,
  TestMonitorRequest,
  TestMonitorResponse,
} from "../../api/types";
import type { Tone } from "../../components/Badge";
import { messageFor } from "../../lib/apiMessage";

/** Mirrors internal/core/monitor's limits. */
export const RULES = {
  minInterval: 20,
  maxInterval: 86_400,
  maxTimeout: 120,
  maxRedirects: 10,
  maxRetries: 10,
  maxURL: 2048,
  maxName: 100,
  maxDescription: 1000,
  maxKeyword: 500,
  maxTags: 20,
  maxTag: 50,
  maxStatusRanges: 20,
  maxHeaders: 20,
  maxHeaderName: 100,
  maxHeaderValue: 4096,
  maxCredential: 4096,
  maxBody: 64 * 1024,
} as const;

/** Headers a check may not set — mirrors forbiddenHeaders in secret.go. */
export const FORBIDDEN_HEADERS = [
  "Host", "Content-Length", "Transfer-Encoding", "Connection", "Upgrade", "Te", "Trailer", "Keep-Alive",
  "Expect", "Proxy-Authorization", "Proxy-Connection", "Cookie2",
] as const;

export const STATUS_LABEL: Record<MonitorStatus, string> = {
  up: "Up", down: "Down", pending: "Pending", unknown: "Unknown",
};

export const FAILURE_LABEL: Record<MonitorFailure, string> = {
  "": "",
  dns: "DNS lookup failed",
  connect: "Connection failed",
  tls: "TLS error",
  timeout: "Timed out",
  http_status: "Unexpected status code",
  keyword: "Keyword not found",
  redirect: "Redirect refused",
  blocked: "Address not allowed",
};

/** A paused monitor is shown as paused whatever its last status. */
export function displayStatus(m: Pick<Monitor, "active" | "live">): { label: string; tone: Tone } {
  if (!m.active) return { label: "Paused", tone: "neutral" };
  const s = m.live.status;
  return { label: STATUS_LABEL[s], tone: s === "up" ? "success" : s === "down" ? "danger" : s === "pending" ? "warning" : "neutral" };
}

export function formatUptime(pct: number | null | undefined): string {
  if (pct === null || pct === undefined) return "—";
  if (pct >= 100) return "100%";
  return `${(Math.floor(pct * 100) / 100).toFixed(2)}%`;
}

export function uptimeTone(pct: number | null | undefined): Tone {
  if (pct === null || pct === undefined) return "neutral";
  return pct >= 99 ? "success" : pct >= 95 ? "warning" : "danger";
}

/** Whole days until a certificate expires, negative once it has. */
export function certDaysLeft(expiresAt: string | null, now: number = Date.now()): number | null {
  return expiresAt ? Math.floor((new Date(expiresAt).getTime() - now) / 86_400_000) : null;
}

/** Points with a response time: a check that got no response has none. */
export function responsePoints(beats: readonly MonitorHeartbeat[]): { time: number; value: number }[] {
  return beats.flatMap((b) => (b.response_ms === null ? [] : [{ time: new Date(b.time).getTime(), value: b.response_ms }]));
}

// ---------------------------------------------------------------- list ----

export interface MonitorFilters {
  q: string;
  status: "all" | MonitorStatus;
  active: "all" | "active" | "paused";
  /** "all", "none" for ungrouped, or a group id. */
  group: string;
}

export const NO_FILTERS: MonitorFilters = { q: "", status: "all", active: "all", group: "all" };

/** The monitors matching every filter. Search covers name, host and tags,
 *  as the server's ?q does. */
export function filterMonitors(monitors: readonly Monitor[], f: MonitorFilters): Monitor[] {
  const q = f.q.trim().toLowerCase();
  return monitors.filter((m) => {
    if (f.status !== "all" && m.live.status !== f.status) return false;
    if (f.active === "active" && !m.active) return false;
    if (f.active === "paused" && m.active) return false;
    if (f.group === "none" && m.group_id !== "") return false;
    if (f.group !== "all" && f.group !== "none" && m.group_id !== f.group) return false;
    if (q === "") return true;
    return m.name.toLowerCase().includes(q) || m.host.toLowerCase().includes(q) || m.tags.some((t) => t.toLowerCase().includes(q));
  });
}

export interface MonitorSection {
  /** Null for monitors in no group. */
  group: MonitorGroup | null;
  monitors: Monitor[];
}

/** Monitors by group in the groups' order, ungrouped last. A group with no
 *  matching monitor is left out; so is a monitor whose group is unknown,
 *  which is shown as ungrouped. */
export function groupMonitors(monitors: readonly Monitor[], groups: readonly MonitorGroup[]): MonitorSection[] {
  const known = new Set(groups.map((g) => g.id));
  const byName = (a: Monitor, b: Monitor) => a.name.localeCompare(b.name, undefined, { sensitivity: "base" });
  const ordered = [...groups].sort((a, b) => a.position - b.position || a.name.localeCompare(b.name));
  const sections: MonitorSection[] = ordered
    .map((g) => ({ group: g, monitors: monitors.filter((m) => m.group_id === g.id).sort(byName) }))
    .filter((s) => s.monitors.length > 0);
  const rest = monitors.filter((m) => !known.has(m.group_id)).sort(byName);
  if (rest.length > 0) sections.push({ group: null, monitors: rest });
  return sections;
}

/** Counts by display status, for the page header. */
export function statusCounts(monitors: readonly Monitor[]): { up: number; down: number; other: number; paused: number } {
  const out = { up: 0, down: 0, other: 0, paused: 0 };
  for (const m of monitors) {
    if (!m.active) out.paused++;
    else if (m.live.status === "up") out.up++;
    else if (m.live.status === "down") out.down++;
    else out.other++;
  }
  return out;
}

// ---------------------------------------------------------------- form ----

export interface HeaderDraft {
  name: string;
  /** Empty keeps the stored value of a stored header. */
  value: string;
  /** Whether a value for this name is stored on the server. */
  stored: boolean;
}

/** The form's state. Holds nothing read back from the stored secret but the
 *  header names: the server does not serve the rest. */
export interface MonitorDraft {
  name: string;
  url: string;
  groupID: string;
  description: string;
  tags: string;
  method: MonitorMethod;
  interval: string;
  retries: string;
  retryInterval: string;
  timeout: string;
  maxRedirects: string;
  acceptedStatus: string;
  keyword: string;
  authType: MonitorAuthType;
  basicUsername: string;
  basicPassword: string;
  bearerToken: string;
  headers: HeaderDraft[];
  certNotify: boolean;
  certWarnDays: string;
  certCritDays: string;
  body: string;
  bodyEncoding: MonitorBodyEncoding;
  /** The selected channels; null on a new monitor until changed, which
   *  leaves the server to select every monitor_default channel. */
  channelIDs: string[] | null;
}

/** Error keys are the API's field names, so a server error lands on the
 *  field it names. */
export type MonitorField =
  | "name" | "url" | "group_id" | "description" | "tags" | "method" | "interval_seconds" | "retries"
  | "retry_interval_seconds" | "timeout_seconds" | "max_redirects" | "accepted_status" | "keyword" | "auth_type"
  | "basic_username" | "basic_password" | "bearer_token" | "headers" | "cert_warn_days" | "cert_crit_days" | "body" | "body_encoding";
export type MonitorErrors = Partial<Record<MonitorField | "form", string>>;

const FIELDS: ReadonlySet<string> = new Set<MonitorField>([
  "name", "url", "group_id", "description", "tags", "method", "interval_seconds", "retries", "retry_interval_seconds",
  "timeout_seconds", "max_redirects", "accepted_status", "keyword", "auth_type", "basic_username", "basic_password",
  "bearer_token", "headers", "cert_crit_days", "body", "body_encoding",
]);

/** The form for a saved monitor, or a new one with the server's defaults. */
export function toMonitorDraft(m: Monitor | null): MonitorDraft {
  if (!m) {
    return {
      name: "", url: "", groupID: "", description: "", tags: "", method: "GET", interval: "60", retries: "3",
      retryInterval: "60", timeout: "30", maxRedirects: "10", acceptedStatus: "200-299", keyword: "", authType: "none",
      basicUsername: "", basicPassword: "", bearerToken: "", headers: [], certNotify: true, certWarnDays: "14",
      certCritDays: "3", body: "", bodyEncoding: "json", channelIDs: null,
    };
  }
  return {
    name: m.name,
    url: m.url,
    groupID: m.group_id,
    description: m.description,
    tags: m.tags.join(", "),
    method: m.method,
    interval: String(m.interval_seconds),
    retries: String(m.retries),
    retryInterval: String(m.retry_interval_seconds),
    timeout: String(m.timeout_seconds),
    maxRedirects: String(m.max_redirects),
    acceptedStatus: m.accepted_status.join(", "),
    keyword: m.keyword,
    authType: m.auth_type ?? "none",
    basicUsername: "",
    basicPassword: "",
    bearerToken: "",
    headers: (m.header_names ?? []).map((name) => ({ name, value: "", stored: true })),
    channelIDs: m.channel_ids ?? [],
    certNotify: m.cert_notify,
    certWarnDays: String(m.cert_warn_days),
    certCritDays: String(m.cert_crit_days),
    body: "",
    bodyEncoding: m.body_encoding,
  };
}

/** Whether the saved monitor holds credentials for `authType`, so empty
 *  credential fields keep them. */
export function keepsCredentials(m: Monitor | null, authType: MonitorAuthType): boolean {
  return m !== null && m.secret_configured === true && m.auth_type === authType && authType !== "none";
}

export function splitList(text: string): string[] {
  return text.split(",").map((s) => s.trim()).filter((s) => s !== "");
}

function int(text: string): number | null {
  return /^\d+$/.test(text.trim()) ? Number(text.trim()) : null;
}

function byteLength(s: string): number {
  return new TextEncoder().encode(s).length;
}

/** Mirrors ValidateURL, except the address checks, which depend on the
 *  server's allow_private_targets and are left to it. */
export function urlError(raw: string): string | undefined {
  if (raw === "" || raw.length > RULES.maxURL) return `Enter a URL of at most ${RULES.maxURL} characters.`;
  // eslint-disable-next-line no-control-regex
  if (/[\s\u0000-\u001f\u007f]/.test(raw)) return "The URL must not contain spaces or control characters.";
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return "Enter a full URL, starting with http:// or https://.";
  }
  if (u.protocol !== "http:" && u.protocol !== "https:") return "The URL must start with http:// or https://.";
  if (u.username !== "" || u.password !== "" || /^[a-z]+:\/\/[^/?#]*@/i.test(raw)) {
    return "The URL must not contain a user name or password. Use Authentication instead.";
  }
  if (u.hostname === "") return "The URL must name a host.";
  return undefined;
}

/** Whether the URL carries a query string, which is stored as written and
 *  shown to anyone who can manage notifications: not sealed like a secret. */
export function hasQueryString(raw: string): boolean {
  try {
    return new URL(raw).search !== "";
  } catch {
    return raw.includes("?");
  }
}

const HEADER_NAME = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

function canonical(name: string): string {
  return name.trim().toLowerCase().split("-").map((p) => p.charAt(0).toUpperCase() + p.slice(1)).join("-");
}

function headersError(d: MonitorDraft): string | undefined {
  if (d.headers.length > RULES.maxHeaders) return `At most ${RULES.maxHeaders} custom headers.`;
  const seen = new Set<string>();
  for (const [i, h] of d.headers.entries()) {
    const which = `Header ${i + 1}`;
    const name = canonical(h.name);
    if (h.name.trim() === "" || h.name.trim().length > RULES.maxHeaderName || !HEADER_NAME.test(h.name.trim())) {
      return `${which}: the name must be 1 to ${RULES.maxHeaderName} letters, digits or - _ . characters.`;
    }
    if ((FORBIDDEN_HEADERS as readonly string[]).includes(name) || name.startsWith("Proxy-")) {
      return `${which}: ${name} cannot be set on a check.`;
    }
    if (name === "Authorization" && d.authType !== "none") {
      return `${which}: remove the Authorization header or set authentication to none.`;
    }
    if (seen.has(name)) return `${which}: the name is used twice.`;
    seen.add(name);
    if (h.value === "" && !h.stored) return `${which} is new, so it needs a value.`;
    if (byteLength(h.value) > RULES.maxHeaderValue || /[\r\n\0]/.test(h.value)) {
      return `${which}: the value is too long or contains a line break.`;
    }
  }
  return undefined;
}

function statusError(text: string): string | undefined {
  const entries = splitList(text);
  const ok = entries.length > 0 && entries.length <= RULES.maxStatusRanges && entries.every((e) => {
    const m = /^(\d{3})(?:-(\d{3}))?$/.exec(e);
    if (!m) return false;
    const lo = Number(m[1]);
    const hi = Number(m[2] ?? m[1]);
    return lo >= 100 && hi <= 599 && lo <= hi;
  });
  return ok ? undefined : 'Use codes or ranges between 100 and 599, such as "200-299, 301".';
}

function tagsError(text: string): string | undefined {
  const tags = splitList(text);
  if (tags.length > RULES.maxTags) return `At most ${RULES.maxTags} tags.`;
  const seen = new Set<string>();
  for (const t of tags) {
    if (t.length > RULES.maxTag) return `A tag must be at most ${RULES.maxTag} characters.`;
    if (seen.has(t.toLowerCase())) return `The tag "${t}" is listed twice.`;
    seen.add(t.toLowerCase());
  }
  return undefined;
}

/** What is wrong with the form, by field; empty when it may be sent. A test
 *  needs no name. Mirrors monitor.Validate and MergeSecret. */
export function validateMonitorDraft(d: MonitorDraft, m: Monitor | null, forTest = false): MonitorErrors {
  const e: MonitorErrors = {};
  if (!forTest && (d.name.trim() === "" || d.name.trim().length > RULES.maxName)) {
    e.name = `Enter a name of at most ${RULES.maxName} characters.`;
  }
  const url = urlError(d.url.trim());
  if (url) e.url = url;
  if (d.description.length > RULES.maxDescription) e.description = `At most ${RULES.maxDescription} characters.`;
  const tags = tagsError(d.tags);
  if (tags) e.tags = tags;

  const interval = int(d.interval);
  const retryInterval = int(d.retryInterval);
  const timeout = int(d.timeout);
  const inRange = (n: number | null, lo: number, hi: number) => n !== null && n >= lo && n <= hi;
  if (!inRange(interval, RULES.minInterval, RULES.maxInterval)) {
    e.interval_seconds = `Between ${RULES.minInterval} and ${RULES.maxInterval} seconds.`;
  }
  if (!inRange(retryInterval, RULES.minInterval, RULES.maxInterval)) {
    e.retry_interval_seconds = `Between ${RULES.minInterval} and ${RULES.maxInterval} seconds.`;
  }
  if (!inRange(timeout, 1, RULES.maxTimeout)) e.timeout_seconds = `Between 1 and ${RULES.maxTimeout} seconds.`;
  else if (interval !== null && timeout !== null && timeout >= interval) {
    e.timeout_seconds = "The timeout must be shorter than the interval.";
  }
  if (!inRange(int(d.retries), 0, RULES.maxRetries)) e.retries = `Between 0 and ${RULES.maxRetries}.`;
  if (!inRange(int(d.maxRedirects), 0, RULES.maxRedirects)) e.max_redirects = `Between 0 and ${RULES.maxRedirects}.`;
  const status = statusError(d.acceptedStatus);
  if (status) e.accepted_status = status;
  if (d.keyword.length > RULES.maxKeyword) e.keyword = `At most ${RULES.maxKeyword} characters.`;
  else if (d.keyword !== "" && d.method === "HEAD") e.keyword = "A HEAD request has no body to search for a keyword.";

  const warn = int(d.certWarnDays);
  const crit = int(d.certCritDays);
  if (warn === null || crit === null || warn < 1 || warn > 365 || crit >= warn) {
    e.cert_crit_days = "Use 0 ≤ critical < warning ≤ 365 days.";
  }

  const keep = keepsCredentials(m, d.authType);
  if (d.authType === "basic") {
    if (d.basicUsername === "" ? !keep : d.basicUsername.includes(":") || d.basicUsername.length > RULES.maxCredential) {
      e.basic_username = "Enter a user name without a colon.";
    }
    if ((d.basicPassword === "" && !keep) || d.basicPassword.length > RULES.maxCredential) {
      e.basic_password = "Enter a password.";
    }
    // Keeping one stored credential while replacing the other is fine; the
    // server keeps whichever is omitted.
  }
  if (d.authType === "bearer") {
    if (d.bearerToken === "" ? !keep : /[\s\0]/.test(d.bearerToken) || d.bearerToken.length > RULES.maxCredential) {
      e.bearer_token = "Enter a token without spaces or line breaks.";
    }
  }
  const headers = headersError(d);
  if (headers) e.headers = headers;
  if (d.body !== "" && d.method !== "POST") e.body = "Only a POST request sends a body.";
  else if (byteLength(d.body) > RULES.maxBody) e.body = `At most ${RULES.maxBody / 1024} KiB.`;
  return e;
}

function headersRequest(d: MonitorDraft, m: Monitor | null): MonitorHeaderRequest[] | undefined {
  const rows = d.headers.map((h) => ({ name: h.name.trim(), value: h.stored && h.value === "" ? null : h.value }));
  if (m) {
    const stored = m.header_names ?? [];
    const unchanged = rows.length === stored.length && rows.every((r) => r.value === null && stored.includes(r.name));
    if (unchanged) return undefined;
  }
  return rows;
}

/** The create or update request for a valid draft. An empty secret field
 *  is left out, so the server keeps what it has. */
export function toMonitorRequest(d: MonitorDraft, m: Monitor | null): MonitorRequest {
  const req: MonitorRequest = {
    name: d.name.trim(),
    url: d.url.trim(),
    method: d.method,
    interval_seconds: Number(d.interval),
    retries: Number(d.retries),
    retry_interval_seconds: Number(d.retryInterval),
    timeout_seconds: Number(d.timeout),
    max_redirects: Number(d.maxRedirects),
    accepted_status: splitList(d.acceptedStatus),
    keyword: d.keyword,
    cert_notify: d.certNotify,
    cert_warn_days: Number(d.certWarnDays),
    cert_crit_days: Number(d.certCritDays),
    group_id: d.groupID,
    description: d.description,
    tags: splitList(d.tags),
    auth_type: d.authType,
    body_encoding: d.bodyEncoding,
  };
  if (d.authType === "basic") {
    if (d.basicUsername !== "") req.basic_username = d.basicUsername;
    if (d.basicPassword !== "") req.basic_password = d.basicPassword;
  }
  if (d.authType === "bearer" && d.bearerToken !== "") req.bearer_token = d.bearerToken;
  const headers = headersRequest(d, m);
  if (headers) req.headers = headers;
  if (d.body !== "") req.body = d.body;
  // A stored body is only sent with POST: leaving POST drops it.
  else if (m?.method === "POST" && d.method !== "POST") req.body = "";
  if (d.channelIDs !== null) req.channel_ids = d.channelIDs;
  return req;
}

/** The channels shown as selected: the draft's own choice, or, on a new
 *  monitor not yet changed, every channel flagged monitor_default. */
export function selectedChannels(d: Pick<MonitorDraft, "channelIDs">, channels: readonly NotificationChannel[]): string[] {
  return d.channelIDs ?? channels.filter((c) => c.monitor_default).map((c) => c.id);
}

/** The test request: the draft as it stands, on top of the saved monitor. */
export function toTestRequest(d: MonitorDraft, m: Monitor | null): TestMonitorRequest {
  const { name, channel_ids: _channels, ...rest } = toMonitorRequest(d, m);
  return { ...rest, ...(name ? { name } : {}), ...(m ? { monitor_id: m.id } : {}) };
}

/** A refused request's errors, by field where the server named one. */
export function serverErrors(error: unknown): MonitorErrors {
  if (!(error instanceof ApiError)) return { form: messageFor(error) };
  const raw = error.code === "already_exists" ? "name" : error.details?.field;
  const field = raw === "cert_warn_days" ? "cert_crit_days" : raw;
  return typeof field === "string" && FIELDS.has(field) ? { [field]: error.message } : { form: error.message };
}

export type MonitorTestResult =
  | { kind: "result"; result: TestMonitorResponse }
  | { kind: "failed"; message: string }
  | { kind: "limited"; retryAt: number };

/** Tests the form as it stands. `send` is the API call. */
export async function runMonitorTest(
  d: MonitorDraft,
  m: Monitor | null,
  send: (body: TestMonitorRequest) => Promise<TestMonitorResponse>,
  now: () => number = Date.now,
): Promise<{ errors: MonitorErrors; result: MonitorTestResult | null }> {
  const errors = validateMonitorDraft(d, m, true);
  if (Object.keys(errors).length > 0) return { errors, result: null };
  try {
    const r = await send(toTestRequest(d, m));
    // Only these four fields are shown, whatever else a response holds.
    const result: TestMonitorResponse = { status: r.status, failure: r.failure, status_code: r.status_code, response_ms: r.response_ms };
    return { errors: {}, result: { kind: "result", result } };
  } catch (error) {
    if (error instanceof ApiError && error.status === 429) {
      return { errors: {}, result: { kind: "limited", retryAt: now() + (error.retryAfterSeconds ?? 5) * 1000 } };
    }
    if (error instanceof ApiError && error.status === 400) return { errors: serverErrors(error), result: null };
    return { errors: {}, result: { kind: "failed", message: messageFor(error) } };
  }
}

/** One line for a test's outcome: up or down, why, the code and the time. */
export function testSummary(r: TestMonitorResponse): string {
  const parts = [r.status === "up" ? "Up" : "Down"];
  if (r.failure !== "") parts.push(FAILURE_LABEL[r.failure]);
  if (r.status_code !== null) parts.push(`status ${r.status_code}`);
  if (r.response_ms !== null) parts.push(`${r.response_ms} ms`);
  return parts.join(" · ");
}

export function secondsLeft(result: MonitorTestResult | null, now: number): number {
  return result?.kind === "limited" ? Math.max(0, Math.ceil((result.retryAt - now) / 1000)) : 0;
}
