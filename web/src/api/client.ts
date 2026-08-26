import type {
  ApiErrorBody,
  ApiErrorResponse,
  CreateUserRequest,
  CreateUserResponse,
  CurrentUser,
  ErrorCode,
  GrantRoleRequest,
  ListUserAuditResponse,
  ListUsersResponse,
  ResetPasswordResponse,
} from "./types";

/**
 * The single HTTP client for the Atlas API.
 *
 * Every request goes through here so that error handling exists in one place.
 * Because the server returns one error envelope for every failure — including
 * router-level 404s and 405s — the client can turn any failed response into
 * the same [ApiError], and a caller never has to distinguish "the endpoint
 * failed" from "the request never reached an endpoint".
 *
 * apiGet remains the only verb for monitored-infrastructure data — Atlas is
 * read-only toward what it observes, and this client makes that guarantee
 * visible by offering no write verb for it. The /auth and /users functions
 * below are the deliberate exception: they write to Atlas's own control-plane
 * state — sessions, and the human-user accounts and roles that govern access
 * to Atlas itself — never to a monitored host.
 */

/** Base path for version 1. Requests are same-origin; see vite.config.ts. */
const API_BASE = "/api/v1";

/** An error returned by the Atlas API, carrying its stable code. */
export class ApiError extends Error {
  readonly code: ErrorCode;
  readonly status: number;
  readonly details: Record<string, unknown> | undefined;
  /** Quote this in a bug report to find the exact server-side request. */
  readonly requestId: string | undefined;

  constructor(status: number, body: ApiErrorBody) {
    super(body.message);
    this.name = "ApiError";
    this.status = status;
    this.code = body.code;
    this.details = body.details;
    this.requestId = body.request_id;
  }

  /**
   * Whether retrying might succeed. A dependency that is temporarily down or
   * a request that timed out are worth another attempt; a malformed request
   * or a missing resource never is.
   */
  get retryable(): boolean {
    return (
      this.code === "unavailable" ||
      this.code === "deadline_exceeded" ||
      this.code === "rate_limited"
    );
  }
}

/** Raised when the server could not be reached at all. */
export class NetworkError extends Error {
  constructor(cause: unknown) {
    super("Could not reach Atlas. It may be restarting, or the network path may be down.");
    this.name = "NetworkError";
    this.cause = cause;
  }
}

interface RequestOptions {
  signal?: AbortSignal | undefined;
}

/**
 * Performs a GET against the Atlas API and decodes the result.
 *
 * Atlas is read-only, so this client offers no write verb. That is not an
 * oversight to be filled in later: the API answers any write with 405, and a
 * client that cannot express one makes the guarantee visible in the frontend
 * as well as the backend.
 */
export async function apiGet<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const url = path.startsWith("/api/") ? path : `${API_BASE}${path}`;

  let response: Response;
  try {
    response = await fetch(url, {
      method: "GET",
      headers: { Accept: "application/json" },
      // Atlas serves the frontend from its own origin, so credentials are
      // same-origin by default and no cross-origin exception is needed.
      credentials: "same-origin",
      ...(options.signal ? { signal: options.signal } : {}),
    });
  } catch (cause) {
    // An aborted request is a caller decision, not a failure to report.
    if (cause instanceof DOMException && cause.name === "AbortError") {
      throw cause;
    }
    throw new NetworkError(cause);
  }

  if (!response.ok) {
    throw await toApiError(response);
  }

  return (await response.json()) as T;
}

/**
 * Converts a failed response into an ApiError.
 *
 * A response that is not the expected envelope — a proxy's HTML error page, a
 * gateway timeout from infrastructure in front of Atlas — still has to become
 * an ApiError, or the UI would show "unexpected token < in JSON" to an
 * operator who needs to know the gateway is down.
 */
async function toApiError(response: Response): Promise<ApiError> {
  try {
    // Parsed as unknown and narrowed, not asserted. The body is untrusted
    // input: an asserted type would let a proxy's error page flow through as
    // if it were an Atlas envelope, and fail later at a confusing place.
    const body: unknown = await response.json();
    if (isApiErrorResponse(body)) {
      return new ApiError(response.status, body.error);
    }
  } catch {
    // Fall through to the synthesised error below.
  }

  return new ApiError(response.status, {
    code: response.status >= 500 ? "unavailable" : "invalid_argument",
    message: `Atlas returned ${response.status} ${response.statusText || ""}`.trim(),
  });
}

/** Narrows an untrusted JSON body to Atlas's error envelope. */
function isApiErrorResponse(value: unknown): value is ApiErrorResponse {
  if (typeof value !== "object" || value === null || !("error" in value)) {
    return false;
  }
  const { error } = value;
  return (
    typeof error === "object" &&
    error !== null &&
    "code" in error &&
    typeof error.code === "string" &&
    "message" in error &&
    typeof error.message === "string"
  );
}

/**
 * Logs in with a username and password, and returns the authenticated
 * principal on success. The server sets the session cookie on the response;
 * this function never sees or handles the cookie itself, since it is
 * HttpOnly by design.
 */
export async function login(username: string, password: string): Promise<CurrentUser> {
  const response = await fetch(`${API_BASE}/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    credentials: "same-origin",
    body: JSON.stringify({ username, password }),
  });
  if (!response.ok) {
    throw await toApiError(response);
  }
  return (await response.json()) as CurrentUser;
}

/**
 * Logs out. Succeeds even with no active session — the caller's intent
 * ("I am logged out") holds either way, mirroring the server's own
 * idempotent revoke.
 */
export async function logout(): Promise<void> {
  await fetch(`${API_BASE}/auth/logout`, { method: "POST", credentials: "same-origin" });
}

/**
 * Fetches the authenticated caller, or throws an [ApiError] with code
 * "unauthenticated" if there is none. This is how the app learns its own
 * login state on load: the session cookie is HttpOnly, so the frontend
 * cannot read it directly and must ask the server.
 */
export async function fetchCurrentUser(signal?: AbortSignal): Promise<CurrentUser> {
  return apiGet<CurrentUser>("/auth/me", signal ? { signal } : {});
}

/**
 * Performs a write against the Atlas API and returns the raw response —
 * shared plumbing for the named functions below, each of which stays the
 * exported, self-describing unit a caller reaches for (createUser,
 * disableUser, …) rather than a generic verb a caller could point at
 * anything.
 */
async function request(path: string, init: RequestInit): Promise<Response> {
  let response: Response;
  try {
    response = await fetch(`${API_BASE}${path}`, { credentials: "same-origin", ...init });
  } catch (cause) {
    if (cause instanceof DOMException && cause.name === "AbortError") {
      throw cause;
    }
    throw new NetworkError(cause);
  }
  if (!response.ok) {
    throw await toApiError(response);
  }
  return response;
}

const JSON_HEADERS = { "Content-Type": "application/json", Accept: "application/json" };

/** Every user Atlas knows about, with their current active role grants. */
export async function fetchUsers(signal?: AbortSignal): Promise<ListUsersResponse> {
  return apiGet<ListUsersResponse>("/users", signal ? { signal } : {});
}

/** Creates a user. The server always generates the password — there is no
 *  field for the caller to supply one — and returns it exactly once. */
export async function createUser(body: CreateUserRequest): Promise<CreateUserResponse> {
  const response = await request("/users", { method: "POST", headers: JSON_HEADERS, body: JSON.stringify(body) });
  return (await response.json()) as CreateUserResponse;
}

/** Grants a role to a user, scoped to one node or fleet-wide. */
export async function grantRole(userID: string, body: GrantRoleRequest): Promise<void> {
  await request(`/users/${encodeURIComponent(userID)}/grants`, {
    method: "POST",
    headers: JSON_HEADERS,
    body: JSON.stringify(body),
  });
}

/** Revokes one specific grant, by its own id — a user may hold several. */
export async function revokeRole(userID: string, grantID: string): Promise<void> {
  await request(`/users/${encodeURIComponent(userID)}/grants/${encodeURIComponent(grantID)}`, {
    method: "DELETE",
  });
}

/** Prevents a user from authenticating and ends their active sessions. */
export async function disableUser(userID: string): Promise<void> {
  await request(`/users/${encodeURIComponent(userID)}/disable`, { method: "POST" });
}

/** Reverses [disableUser]. */
export async function enableUser(userID: string): Promise<void> {
  await request(`/users/${encodeURIComponent(userID)}/enable`, { method: "POST" });
}

/** Invalidates a user's current password and issues a new, generated one. */
export async function resetPassword(userID: string): Promise<ResetPasswordResponse> {
  const response = await request(`/users/${encodeURIComponent(userID)}/reset-password`, { method: "POST" });
  return (await response.json()) as ResetPasswordResponse;
}

/** Terminates every session a user currently holds, without disabling the
 *  account — they can log back in immediately. */
export async function forceLogout(userID: string): Promise<void> {
  await request(`/users/${encodeURIComponent(userID)}/force-logout`, { method: "POST" });
}

/** One user's recorded activity: every grant, revoke, disable, enable,
 *  reset-password and force-logout taken against their account. */
export async function fetchUserAudit(userID: string, signal?: AbortSignal): Promise<ListUserAuditResponse> {
  return apiGet<ListUserAuditResponse>(`/users/${encodeURIComponent(userID)}/audit`, signal ? { signal } : {});
}

/**
 * Probes readiness. Used by the connection banner rather than by data views,
 * because a failing dependency should be reported once and prominently, not
 * as an error on every panel.
 */
export async function fetchReadiness(signal?: AbortSignal): Promise<boolean> {
  try {
    const response = await fetch("/readyz", {
      method: "GET",
      ...(signal ? { signal } : {}),
    });
    return response.ok;
  } catch {
    return false;
  }
}
