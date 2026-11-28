import { ApiError } from "../../api/client";
import type {
  NotificationChannel,
  NotificationChannelRequest,
  NotificationDelivery,
  NotificationChannelType,
  NotificationSeverity,
  TestNotificationDraftRequest,
  TestNotificationResponse,
} from "../../api/types";
import { messageFor } from "../../lib/apiMessage";
import {
  DEFAULT_TEMPLATE,
  DENIED_HEADERS,
  MAX_HEADER_COUNT,
  MAX_HEADER_NAME_LENGTH,
  MAX_HEADER_VALUE_LENGTH,
  MAX_TEMPLATE_LENGTH,
  MAX_URL_LENGTH,
  TEAMS_HOST_SUFFIXES,
} from "./channelRules";

/** The channel form's state. Never holds anything read back from the stored
 *  secret but header names: the server does not serve the rest. */
export interface ChannelDraft {
  type: NotificationChannelType;
  name: string;
  url: string;
  template: string;
  /** JSON object text. */
  headers: string;
  enabled: boolean;
  minSeverity: NotificationSeverity;
  /** Pre-select for new website monitors. */
  monitorDefault: boolean;
}

export type ChannelField = "name" | "url" | "template" | "headers";
export type ChannelErrors = Partial<Record<ChannelField | "form", string>>;

export const TYPE_LABEL: Record<NotificationChannelType, string> = {
  teams: "Microsoft Teams",
  webhook: "Webhook",
  email: "Email",
};

/** The form for a saved channel, or for a new one. */
export function toDraft(channel: NotificationChannel | null): ChannelDraft {
  if (!channel) {
    return {
      type: "teams", name: "", url: "", template: DEFAULT_TEMPLATE, headers: "", enabled: true, minSeverity: "warning",
      monitorDefault: false,
    };
  }
  const { template } = channel.config;
  return {
    type: channel.type,
    name: channel.name,
    url: "",
    template: typeof template === "string" && template !== "" ? template : DEFAULT_TEMPLATE,
    headers: channel.header_names.length > 0
      ? JSON.stringify(Object.fromEntries(channel.header_names.map((n) => [n, ""])), null, 2)
      : "",
    enabled: channel.enabled,
    minSeverity: channel.min_severity,
    monitorDefault: channel.monitor_default,
  };
}

/** Whether an empty URL keeps the stored one. A stored secret the server
 *  cannot read has no host, and cannot be kept in part. */
export function canKeepURL(channel: NotificationChannel | null): boolean {
  return channel !== null && channel.secret_configured && channel.destination_host !== "";
}

/** A secret that is stored but unreadable, so must be entered again. */
export function secretUnreadable(channel: NotificationChannel | null): boolean {
  return channel !== null && channel.type !== "email" && channel.secret_configured && channel.destination_host === "";
}

function byteLength(s: string): number {
  return new TextEncoder().encode(s).length;
}

function urlError(type: NotificationChannelType, raw: string): string | undefined {
  if (byteLength(raw) > MAX_URL_LENGTH) return `The URL is longer than ${MAX_URL_LENGTH} characters.`;
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return "Enter a full URL, starting with https://.";
  }
  if (url.protocol !== "https:") return "The URL must start with https://.";
  if (url.username || url.password) return "The URL must not carry a username or password.";
  if (type === "teams" && !TEAMS_HOST_SUFFIXES.some((s) => url.hostname === s || url.hostname.endsWith(`.${s}`))) {
    return `A Teams URL's host must be, or end with, ${TEAMS_HOST_SUFFIXES.join(", ")}.`;
  }
  return undefined;
}

const HEADER_NAME = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;
// eslint-disable-next-line no-control-regex
const HEADER_VALUE_FORBIDDEN = /[\u0000-\u0008\u000a-\u001f\u007f]/;

/** The headers as an object, or why they are not valid. A message names a
 *  header by its position: a name may be a value pasted in the wrong place. */
export function parseHeaders(text: string): { headers: Record<string, string> } | { error: string } {
  if (text.trim() === "") return { headers: {} };
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { error: 'The headers are not valid JSON. Use an object, such as {"X-Api-Key": "value"}.' };
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return { error: 'The headers must be a JSON object, such as {"X-Api-Key": "value"}.' };
  }
  const entries: [string, unknown][] = Object.entries(parsed);
  if (entries.length > MAX_HEADER_COUNT) return { error: `At most ${MAX_HEADER_COUNT} headers are allowed.` };

  const headers: Record<string, string> = {};
  const seen = new Set<string>();
  for (const [i, [name, value]] of entries.entries()) {
    const which = `Header ${i + 1}`;
    if (typeof value !== "string") return { error: `${which}: the value must be a string.` };
    if (!HEADER_NAME.test(name)) return { error: `${which}: the name has a character that is not allowed.` };
    if (name.length > MAX_HEADER_NAME_LENGTH) {
      return { error: `${which}: the name is longer than ${MAX_HEADER_NAME_LENGTH} characters.` };
    }
    const denied = DENIED_HEADERS.find((d) => d.toLowerCase() === name.toLowerCase());
    if (denied) return { error: `${denied} may not be set as a header.` };
    if (seen.has(name.toLowerCase())) return { error: `${which}: the name is used twice.` };
    if (byteLength(value) > MAX_HEADER_VALUE_LENGTH) {
      return { error: `${which}: the value is longer than ${MAX_HEADER_VALUE_LENGTH} characters.` };
    }
    if (HEADER_VALUE_FORBIDDEN.test(value)) {
      return { error: `${which}: the value has a character that is not allowed.` };
    }
    seen.add(name.toLowerCase());
    headers[name] = value;
  }
  return { headers };
}

function headersError(draft: ChannelDraft, channel: NotificationChannel | null): string | undefined {
  const parsed = parseHeaders(draft.headers);
  if ("error" in parsed) return parsed.error;
  for (const [i, [name, value]] of Object.entries(parsed.headers).entries()) {
    if (value !== "") continue;
    if (!canKeepURL(channel)) return `Header ${i + 1} needs a value.`;
    if (draft.url.trim() !== "") return `A new URL needs every header value entered again: header ${i + 1} is empty.`;
    if (!channel?.header_names.includes(name)) return `Header ${i + 1} is new, so it needs a value.`;
  }
  return undefined;
}

/** What is wrong with the form, by field. Empty when it may be sent. */
export function validateDraft(draft: ChannelDraft, channel: NotificationChannel | null): ChannelErrors {
  const errors: ChannelErrors = {};
  if (draft.name.trim() === "") errors.name = "Enter a name.";
  if (draft.type === "email") return errors;

  const url = draft.url.trim();
  if (url !== "") {
    const problem = urlError(draft.type, url);
    if (problem) errors.url = problem;
  } else if (!canKeepURL(channel)) {
    errors.url = "Enter the Post URL.";
  }
  if (draft.type === "webhook") {
    if (draft.template.trim() === "") errors.template = "Enter a request body.";
    else if (byteLength(draft.template) > MAX_TEMPLATE_LENGTH) {
      errors.template = `The request body is longer than ${MAX_TEMPLATE_LENGTH} bytes.`;
    }
    const problem = headersError(draft, channel);
    if (problem) errors.headers = problem;
  }
  return errors;
}

/** The secret to send for a valid draft, or undefined to keep the stored one
 *  whole. */
function buildSecret(draft: ChannelDraft, channel: NotificationChannel | null): string | undefined {
  const url = draft.url.trim();
  if (draft.type === "email") return undefined;
  if (draft.type === "teams") return url === "" ? undefined : JSON.stringify({ url });

  const parsed = parseHeaders(draft.headers);
  const headers = "headers" in parsed ? parsed.headers : {};
  if (url === "" && channel) {
    const names = Object.keys(headers);
    const unchanged =
      names.length === channel.header_names.length &&
      names.every((n) => channel.header_names.includes(n) && headers[n] === "");
    if (unchanged) return undefined;
  }
  return JSON.stringify({ url, headers });
}

/** The create or update request for a valid draft. */
export function toRequest(draft: ChannelDraft, channel: NotificationChannel | null): NotificationChannelRequest {
  const secret = buildSecret(draft, channel);
  let config: Record<string, unknown> = {};
  if (draft.type === "email") config = channel?.config ?? {};
  if (draft.type === "webhook") {
    const contentType = channel?.config.content_type;
    config = { template: draft.template, ...(contentType === undefined ? {} : { content_type: contentType }) };
  }
  return {
    name: draft.name.trim(),
    type: draft.type,
    enabled: draft.enabled,
    min_severity: draft.minSeverity,
    monitor_default: draft.monitorDefault,
    config,
    ...(secret === undefined ? {} : { secret }),
  };
}

const FIELDS: readonly string[] = ["name", "url", "template", "headers"] satisfies ChannelField[];

/** A refused request's errors, by field where the server named one. */
export function serverErrors(error: unknown): ChannelErrors {
  if (!(error instanceof ApiError)) return { form: messageFor(error) };
  const field = error.code === "already_exists" ? "name" : error.details?.field;
  const reason = error.details?.reason;
  const message = typeof reason === "string" ? `${error.message}: ${reason}` : error.message;
  return typeof field === "string" && FIELDS.includes(field) ? { [field]: message } : { form: message };
}

export type TestResult =
  | { kind: "ok" }
  | { kind: "failed"; message: string }
  | { kind: "limited"; retryAt: number };

/** Tests the form as it stands. `send` is the API call. */
export async function runTest(
  draft: ChannelDraft,
  channel: NotificationChannel | null,
  send: (body: TestNotificationDraftRequest) => Promise<TestNotificationResponse>,
  now: () => number = Date.now,
): Promise<{ errors: ChannelErrors; result: TestResult | null }> {
  const errors = validateDraft(draft, channel);
  if (Object.keys(errors).length > 0) return { errors, result: null };
  try {
    const outcome = await send({ ...toRequest(draft, channel), ...(channel ? { channel_id: channel.id } : {}) });
    return { errors: {}, result: outcome.ok ? { kind: "ok" } : { kind: "failed", message: outcome.error } };
  } catch (error) {
    if (error instanceof ApiError && error.status === 429) {
      return { errors: {}, result: { kind: "limited", retryAt: now() + (error.retryAfterSeconds ?? 5) * 1000 } };
    }
    if (error instanceof ApiError && error.status === 400) return { errors: serverErrors(error), result: null };
    return { errors: {}, result: { kind: "failed", message: messageFor(error) } };
  }
}

/** Whole seconds until Test may be used again. */
export function secondsLeft(result: TestResult | null, now: number): number {
  return result?.kind === "limited" ? Math.max(0, Math.ceil((result.retryAt - now) / 1000)) : 0;
}

/**
 * A delivery's error, short enough for a table cell. A destination's own
 * words can repeat the request it was sent, so a status is shown without its
 * body and anything quoted is dropped.
 */
export function shortError(text: string | undefined): string {
  if (!text) return "";
  const status = /returned status (\d+)/.exec(text);
  if (status?.[1]) return `The destination returned status ${status[1]}`;
  const plain = text.replace(/"[^"]*"?/g, "…");
  return plain.length > 120 ? `${plain.slice(0, 119)}…` : plain;
}

const KIND_PREFIX: Partial<Record<NotificationDelivery["kind"], string>> = {
  escalated: "Escalated: ",
  recovered: "Recovered: ",
  flapping: "Flapping: ",
};

/** A delivery's title as the Teams card shows it: prefixed by its kind. */
export function deliveryTitle(d: Pick<NotificationDelivery, "kind" | "title" | "trigger">): string {
  const title = d.title === undefined || d.title === "" ? d.trigger : d.title;
  return (KIND_PREFIX[d.kind] ?? "") + title;
}
