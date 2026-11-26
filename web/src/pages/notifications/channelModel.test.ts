import { readFileSync } from "node:fs";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, testNotificationDraft } from "../../api/client";
import { notificationsDisabled } from "../../api/queries";
import type { NotificationChannel, TestNotificationDraftRequest } from "../../api/types";
import {
  deliveryTitle,
  parseHeaders,
  runTest,
  secondsLeft,
  serverErrors,
  shortError,
  toDraft,
  toRequest,
  validateDraft,
  type ChannelDraft,
} from "./channelModel";
import * as rules from "./channelRules";

const TEAMS_URL = "https://prod-1.westus.logic.azure.com/workflows/x?sig=SIGSECRET";

function draft(over: Partial<ChannelDraft> = {}): ChannelDraft {
  return { ...toDraft(null), name: "ops", url: TEAMS_URL, ...over };
}

function hook(over: Partial<ChannelDraft> = {}): ChannelDraft {
  return draft({ type: "webhook", url: "https://hooks.example.com/x", ...over });
}

function saved(over: Partial<NotificationChannel> = {}): NotificationChannel {
  return {
    id: "c1", name: "ops", type: "webhook", enabled: true, min_severity: "critical",
    config: { template: "{{.msg}}", content_type: "text/plain" }, secret_configured: true,
    destination_host: "hooks.example.com", header_names: ["X-Api", "X-Old"],
    created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", ...over,
  };
}

describe("the mirrored rules", () => {
  const go = readFileSync(new URL("../../../../internal/core/notification/config.go", import.meta.url), "utf8");
  const block = (start: string) => {
    const from = go.indexOf(start);
    return Array.from(go.slice(from, go.indexOf("\n}", from)).matchAll(/"([^"]+)"/g), (m) => m[1]);
  };

  it("match the server's source", () => {
    expect(rules.TEAMS_HOST_SUFFIXES).toEqual(block("var TeamsAllowedHostSuffixes"));
    expect(rules.DENIED_HEADERS).toEqual(block("var headerDenyList"));
    for (const [name, value] of [
      ["MaxDestinationURLLength", rules.MAX_URL_LENGTH],
      ["MaxHeaderCount", rules.MAX_HEADER_COUNT],
      ["MaxHeaderNameLength", rules.MAX_HEADER_NAME_LENGTH],
      ["MaxHeaderValueLength", rules.MAX_HEADER_VALUE_LENGTH],
    ] as const) {
      expect(go).toMatch(new RegExp(`${name}\\s+= ${String(value)}\\n`));
    }
    expect(go).toMatch(/MaxTemplateLength\s+= 16 \* 1024\n/);
  });
});

describe("validateDraft", () => {
  it("accepts valid channels", () => {
    expect(validateDraft(draft(), null)).toEqual({});
    expect(validateDraft(draft({ url: "https://logic.azure.com/x" }), null)).toEqual({});
    expect(validateDraft(hook({ headers: '{"Authorization": "Bearer x"}' }), null)).toEqual({});
  });

  it("needs a name and, for a new channel, a URL", () => {
    expect(Object.keys(validateDraft(draft({ name: "  ", url: "" }), null))).toEqual(["name", "url"]);
  });

  it.each([
    "https://hooks.example.com/x",
    "https://evil-logic.azure.com.example.com/x",
    "https://notlogic.azure.com/x",
    "http://logic.azure.com/x",
    "https://u:p@logic.azure.com/x",
    "logic.azure.com/x",
    `https://logic.azure.com/${"a".repeat(2048)}`,
  ])("refuses a Teams URL: %s", (url) => {
    expect(validateDraft(draft({ url }), null).url).toBeDefined();
  });

  it("holds only Teams to the Teams hosts", () => {
    expect(validateDraft(hook(), null)).toEqual({});
  });

  it("needs a request body for a webhook only", () => {
    expect(validateDraft(hook({ template: " \n" }), null).template).toBeDefined();
    expect(validateDraft(hook({ template: "a".repeat(16 * 1024 + 1) }), null).template).toBeDefined();
    expect(validateDraft(draft({ template: "" }), null)).toEqual({});
  });

  it.each([
    ["not JSON", "X-Api: 1"],
    ["an array", "[]"],
    ["null", "null"],
    ["a string", '"x"'],
    ["a non-string value", '{"X-Api": 1}'],
    ["a nested value", '{"X-Api": {}}'],
    ["a space in a name", '{"X Api": "1"}'],
    ["an empty name", '{"": "1"}'],
    ["a newline in a value", '{"X-Api": "1\\r\\nCookie: x"}'],
    ["a long name", `{"${"a".repeat(129)}": "1"}`],
    ["a long value", `{"X-Api": "${"a".repeat(4097)}"}`],
    ["a repeated name", '{"X-Api": "1", "x-api": "2"}'],
    ["an empty value", '{"X-Api": ""}'],
    ["too many", JSON.stringify(Object.fromEntries(Array.from({ length: 21 }, (_, i) => [`X-${String(i)}`, "1"])))],
  ])("refuses headers: %s", (_, headers) => {
    expect(validateDraft(hook({ headers }), null).headers).toBeDefined();
  });

  it.each(["Host", "content-length", "TRANSFER-ENCODING", "Connection", "cookie"])("refuses header %s", (name) => {
    expect(validateDraft(hook({ headers: JSON.stringify({ [name]: "x" }) }), null).headers).toContain("may not be set");
  });

  it("never repeats what was typed into the headers", () => {
    for (const text of ["S3CRET", '{"S3CRET key": "v"}', '{"X": ["S3CRET"]}', '{"X": "S3CRET\\u0000"}']) {
      const parsed = parseHeaders(text);
      expect("error" in parsed && parsed.error).not.toContain("S3CRET");
    }
  });
});

describe("editing", () => {
  it("starts with nothing from the secret but header names", () => {
    const d = toDraft(saved());
    expect(d.url).toBe("");
    expect(JSON.parse(d.headers)).toEqual({ "X-Api": "", "X-Old": "" });
    expect(validateDraft(d, saved())).toEqual({});
  });

  it("sends no secret when neither the URL nor the headers changed", () => {
    expect(toRequest(toDraft(saved()), saved())).toEqual({
      name: "ops", type: "webhook", enabled: true, min_severity: "critical",
      config: { template: "{{.msg}}", content_type: "text/plain" },
    });
    const teams = saved({ type: "teams", config: {}, header_names: [] });
    expect(toRequest(toDraft(teams), teams)).not.toHaveProperty("secret");
  });

  it("sends empty parts for the server to keep", () => {
    const secret = (headers: string) => toRequest({ ...toDraft(saved()), headers }, saved()).secret ?? "";
    expect(JSON.parse(secret('{"X-Api": "", "X-Old": "new"}'))).toEqual({ url: "", headers: { "X-Api": "", "X-Old": "new" } });
    expect(JSON.parse(secret('{"X-Api": ""}'))).toEqual({ url: "", headers: { "X-Api": "" } });
    expect(JSON.parse(secret(""))).toEqual({ url: "", headers: {} });
  });

  it("needs a value for a new header, and every value with a new URL", () => {
    const d = toDraft(saved());
    expect(validateDraft({ ...d, headers: '{"X-New": ""}' }, saved()).headers).toBeDefined();
    expect(validateDraft({ ...d, url: "https://other.example.com/" }, saved()).headers).toContain("new URL");
    expect(validateDraft({ ...d, url: "https://other.example.com/", headers: '{"X-Api": "v"}' }, saved())).toEqual({});
  });

  it("needs a URL again when the stored one cannot be read", () => {
    const unreadable = saved({ destination_host: "", header_names: [] });
    expect(validateDraft(toDraft(unreadable), unreadable).url).toBeDefined();
  });
});

describe("runTest", () => {
  it("sends the form as it stands, with channel_id when editing", async () => {
    const sent: TestNotificationDraftRequest[] = [];
    const send = (body: TestNotificationDraftRequest) => {
      sent.push(body);
      return Promise.resolve({ ok: true, error: "" });
    };
    expect((await runTest(draft({ name: "new" }), null, send)).result).toEqual({ kind: "ok" });
    expect((await runTest({ ...toDraft(saved()), name: "renamed" }, saved(), send)).result).toEqual({ kind: "ok" });
    expect(sent[0]).toMatchObject({ name: "new", secret: JSON.stringify({ url: TEAMS_URL }) });
    expect(sent[0]).not.toHaveProperty("channel_id");
    expect(sent[1]).toMatchObject({ name: "renamed", channel_id: "c1" });
    expect(sent[1]).not.toHaveProperty("secret");
  });

  it("does not send an invalid form", async () => {
    const send = vi.fn();
    expect((await runTest(draft({ url: "" }), null, send)).errors.url).toBeDefined();
    expect(send).not.toHaveBeenCalled();
  });

  it("shows the server's category message", async () => {
    const send = () => Promise.resolve({ ok: false, error: "the destination returned status 500" });
    expect((await runTest(draft(), null, send)).result).toEqual({
      kind: "failed", message: "the destination returned status 500",
    });
  });

  it("maps a 400 to its field", async () => {
    const send = () =>
      Promise.reject(new ApiError(400, { code: "invalid_argument", message: "not public", details: { field: "url" } }));
    expect(await runTest(draft(), null, send)).toEqual({ errors: { url: "not public" }, result: null });
  });

  it("counts down from Retry-After on a 429", async () => {
    const send = () => Promise.reject(new ApiError(429, { code: "rate_limited", message: "slow down" }, 4));
    const { result } = await runTest(draft(), null, send, () => 10_000);
    expect(result).toEqual({ kind: "limited", retryAt: 14_000 });
    expect([10_000, 10_001, 13_500, 14_000, 20_000].map((now) => secondsLeft(result, now))).toEqual([4, 4, 1, 0, 0]);
    expect(secondsLeft({ kind: "ok" }, 0)).toBe(0);
  });
});

describe("the test request", () => {
  afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

  function respond(status: number, body: unknown, headers: Record<string, string> = {}) {
    const fetchMock = vi.fn((_url: string, _init: RequestInit) =>
      Promise.resolve(new Response(JSON.stringify(body), { status, headers })));
    vi.stubGlobal("fetch", fetchMock);
    return fetchMock;
  }

  it("goes to the draft endpoint", async () => {
    const fetchMock = respond(200, { ok: true, error: "" });
    await runTest(draft(), null, testNotificationDraft);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/notifications/channels/test");
    expect(fetchMock.mock.calls[0]?.[1].method).toBe("POST");
  });

  it("reads Retry-After", async () => {
    respond(429, { error: { code: "rate_limited", message: "slow down" } }, { "Retry-After": "7" });
    const { result } = await runTest(draft(), null, testNotificationDraft, () => 0);
    expect(secondsLeft(result, 0)).toBe(7);
  });

  it("keeps the URL out of the console and the outcome when it fails", async () => {
    const logged = (["log", "info", "warn", "error", "debug"] as const).map((m) => vi.spyOn(console, m));
    vi.stubGlobal("fetch", () => Promise.reject(new TypeError(`failed: ${TEAMS_URL}`)));
    const outcome = await runTest(draft(), null, testNotificationDraft);
    expect(JSON.stringify(outcome)).not.toContain("SIGSECRET");
    for (const spy of logged) expect(spy).not.toHaveBeenCalled();
  });
});

describe("serverErrors", () => {
  const refused = (details: Record<string, unknown>) =>
    serverErrors(new ApiError(400, { code: "invalid_argument", message: "bad", details }));

  it("places a message on the field it is about", () => {
    expect(refused({ field: "headers" })).toEqual({ headers: "bad" });
    expect(refused({ field: "template", reason: "1: unclosed action" })).toEqual({ template: "bad: 1: unclosed action" });
    expect(refused({ field: "config" })).toEqual({ form: "bad" });
    expect(refused({})).toEqual({ form: "bad" });
    expect(serverErrors(new ApiError(409, { code: "already_exists", message: "taken" }))).toEqual({ name: "taken" });
    expect(serverErrors(new TypeError(TEAMS_URL))).toEqual({ form: "Could not reach Atlas." });
  });
});

describe("notificationsDisabled", () => {
  it("is the missing key alone, not any 503", () => {
    const at = (message: string) => new ApiError(503, { code: "unavailable", message });
    expect(notificationsDisabled(at("notifications are disabled: no encryption key is configured"))).toBe(true);
    expect(notificationsDisabled(at("could not list notification channels"))).toBe(false);
    expect(notificationsDisabled(null)).toBe(false);
  });
});

describe("shortError", () => {
  it("drops a destination's own words", () => {
    expect(shortError("webhook destination returned status 500: cannot POST /T0KEN")).toBe("The destination returned status 500");
    expect(shortError('deliver webhook: Post h: malformed HTTP response "/T0KEN')).not.toContain("T0KEN");
    expect(shortError("a".repeat(500))).toHaveLength(120);
    expect(shortError(undefined)).toBe("");
  });
});

describe("deliveryTitle", () => {
  const base = { title: "Container exited", trigger: "container_exit" };
  it("prefixes the title by kind like the Teams card", () => {
    expect(deliveryTitle({ ...base, kind: "firing" })).toBe("Container exited");
    expect(deliveryTitle({ ...base, kind: "escalated" })).toBe("Escalated: Container exited");
    expect(deliveryTitle({ ...base, kind: "recovered" })).toBe("Recovered: Container exited");
    expect(deliveryTitle({ ...base, kind: "flapping" })).toBe("Flapping: Container exited");
    expect(deliveryTitle({ kind: "test", title: "Atlas test notification", trigger: "" })).toBe("Atlas test notification");
  });
  it("falls back to the trigger when a row has no title", () => {
    expect(deliveryTitle({ kind: "recovered", trigger: "node_down" })).toBe("Recovered: node_down");
  });
});
