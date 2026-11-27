import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import { App } from "../../App";
import { ApiError } from "../../api/client";
import { monitorKeys } from "../../api/queries";
import type { CurrentUser, ListMonitorsResponse, Monitor, MonitorDetail, MonitorGroup } from "../../api/types";
import { AuthProvider } from "../../auth/AuthContext";
import { ToastProvider } from "../../components/Toast";
import { canSeeNavPage } from "../../shell/pageAccess";
import { NAV_PAGES } from "../../shell/pages";
import { WebsitesPage } from "../WebsitesPage";
import { MonitorFields } from "./MonitorForm";
import {
  NO_FILTERS,
  filterMonitors,
  formatUptime,
  groupMonitors,
  hasQueryString,
  runMonitorTest,
  testSummary,
  toMonitorDraft,
  toMonitorRequest,
  toTestRequest,
  validateMonitorDraft,
  type MonitorDraft,
} from "./monitorModel";

vi.mock("../../shell/useTheme", () => ({ useTheme: () => ({ theme: "dark", toggle: () => undefined }) }));
vi.mock("../../shell/Topbar", () => ({ Topbar: () => null }));

const at = "2026-11-27T10:00:00Z";

function monitor(over: Partial<Monitor> = {}): Monitor {
  return {
    id: "m1", name: "Shop", type: "http", url: "https://shop.example.com/health?token=abc", host: "shop.example.com",
    method: "GET", interval_seconds: 60, retries: 3, retry_interval_seconds: 60, timeout_seconds: 30, max_redirects: 10,
    accepted_status: ["200-299"], body_encoding: "json", keyword: "", recovery_successes: 1, cert_notify: true,
    cert_warn_days: 14, cert_crit_days: 3, group_id: "", description: "", tags: [], active: true,
    alerts_muted_until: null, auth_type: "bearer", header_names: ["X-Api-Key"], secret_configured: true,
    created_at: at, updated_at: at, redacted: false, uptime_24h: 99.5,
    live: {
      status: "up", status_since: at, down_since: null, consecutive_failures: 0, last_check_at: at, last_status_code: 200,
      last_response_ms: 87, last_failure: "", next_check_at: at, cert_expires_at: "2027-01-01T00:00:00Z",
    },
    heartbeats: [{ time: at, status: "up", status_code: 200, response_ms: 87, failure: "" }],
    ...over,
  };
}

function viewerCopy(m: Monitor): Monitor {
  const { auth_type: _a, header_names: _h, secret_configured: _s, ...rest } = m;
  return { ...rest, url: "https://shop.example.com/health", redacted: true };
}

const group = (id: string, name: string, position: number): MonitorGroup => ({ id, name, position });

function user(over: Partial<CurrentUser> = {}): CurrentUser {
  return {
    user_id: "u1", username: "someone", can_manage_users: false, is_superadmin: false, can_manage_notifications: false,
    can_enroll_fleet: false, page_access: [{ page: "websites", fleet_wide: true }], ...over,
  };
}

function renderPage(me: CurrentUser, list: ListMonitorsResponse, detail?: MonitorDetail): string {
  const client = new QueryClient({ defaultOptions: { queries: { retryOnMount: false, staleTime: Infinity } } });
  client.setQueryData(["auth", "me"], me);
  client.setQueryData(monitorKeys.list, list);
  if (detail) {
    client.setQueryData(monitorKeys.detail(detail.id), detail);
    client.setQueryData(monitorKeys.heartbeats(detail.id), { monitor_id: detail.id, heartbeats: detail.heartbeats ?? [] });
    client.setQueryData(monitorKeys.events(detail.id), {
      monitor_id: detail.id,
      events: [{ id: "e1", time: at, kind: "down", status_code: 503, failure: "http_status", duration_seconds: null }],
    });
  }
  return renderToStaticMarkup(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/websites"]}>
        <AuthProvider><ToastProvider><WebsitesPage /></ToastProvider></AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const detailOf = (m: Monitor): MonitorDetail => ({ ...m, uptime: { day: 99.5, week: 98, month: null } });
const offline = { offline: false, since: null };

describe("filterMonitors", () => {
  const list = [
    monitor({ id: "a", name: "Shop", tags: ["prod"] }),
    monitor({ id: "b", name: "Blog", host: "blog.example.org", group_id: "g1", live: { ...monitor().live, status: "down" } }),
    monitor({ id: "c", name: "Docs", active: false, group_id: "g2" }),
  ];
  const ids = (f: Partial<typeof NO_FILTERS>) => filterMonitors(list, { ...NO_FILTERS, ...f }).map((m) => m.id);

  it("searches name, host and tags", () => {
    expect(ids({ q: "blog.example" })).toEqual(["b"]);
    expect(ids({ q: "PROD" })).toEqual(["a"]);
    expect(ids({ q: "docs" })).toEqual(["c"]);
  });

  it("filters by status, active and group", () => {
    expect(ids({ status: "down" })).toEqual(["b"]);
    expect(ids({ active: "paused" })).toEqual(["c"]);
    expect(ids({ active: "active" })).toEqual(["a", "b"]);
    expect(ids({ group: "g1" })).toEqual(["b"]);
    expect(ids({ group: "none" })).toEqual(["a"]);
  });
});

describe("groupMonitors", () => {
  it("orders groups by position, drops empty ones, puts ungrouped and unknown groups last", () => {
    const sections = groupMonitors(
      [monitor({ id: "z", name: "zeta", group_id: "g2" }), monitor({ id: "a", name: "Alpha", group_id: "g2" }),
        monitor({ id: "x", name: "x", group_id: "gone" }), monitor({ id: "y", name: "y", group_id: "g1" })],
      [group("g2", "Second", 2), group("g1", "First", 1), group("g3", "Empty", 0)],
    );
    expect(sections.map((s) => [s.group?.name ?? null, s.monitors.map((m) => m.id)])).toEqual([
      ["First", ["y"]], ["Second", ["a", "z"]], [null, ["x"]],
    ]);
  });
});

describe("validateMonitorDraft", () => {
  const draft = (over: Partial<MonitorDraft> = {}): MonitorDraft => ({ ...toMonitorDraft(null), name: "A", url: "https://a.example.com/", ...over });
  const fields = (d: MonitorDraft, m: Monitor | null = null, test = false) => Object.keys(validateMonitorDraft(d, m, test)).sort();

  it("accepts the defaults with a name and URL", () => {
    expect(fields(draft())).toEqual([]);
  });

  it("mirrors the server's URL rules", () => {
    for (const url of ["ftp://a.example.com/", "https://user:pw@a.example.com/", "https://@a.example.com/", "a.example.com", "https://a b.com/"]) {
      expect(fields(draft({ url }))).toEqual(["url"]);
    }
  });

  it("mirrors the interval, timeout and other request rules", () => {
    expect(fields(draft({ interval: "10", timeout: "5" }))).toEqual(["interval_seconds"]);
    expect(fields(draft({ interval: "30", timeout: "30" }))).toEqual(["timeout_seconds"]);
    expect(fields(draft({ timeout: "121", interval: "600" }))).toEqual(["timeout_seconds"]);
    expect(fields(draft({ retries: "11", maxRedirects: "-1" }))).toEqual(["max_redirects", "retries"]);
    expect(fields(draft({ acceptedStatus: "200-299, 99" }))).toEqual(["accepted_status"]);
    expect(fields(draft({ acceptedStatus: "301, 200-204" }))).toEqual([]);
    expect(fields(draft({ method: "HEAD", keyword: "ok" }))).toEqual(["keyword"]);
    expect(fields(draft({ certWarnDays: "3", certCritDays: "3" }))).toEqual(["cert_crit_days"]);
    expect(fields(draft({ tags: "a, A" }))).toEqual(["tags"]);
    expect(fields(draft({ body: "{}" }))).toEqual(["body"]);
  });

  it("needs credentials and header values unless they are stored", () => {
    expect(fields(draft({ authType: "bearer" }))).toEqual(["bearer_token"]);
    expect(fields(draft({ authType: "basic" }))).toEqual(["basic_password", "basic_username"]);
    const saved = monitor();
    expect(fields({ ...toMonitorDraft(saved) }, saved)).toEqual([]);
    expect(fields({ ...toMonitorDraft(saved), authType: "basic" }, saved)).toEqual(["basic_password", "basic_username"]);
    expect(fields(draft({ headers: [{ name: "X-New", value: "", stored: false }] }))).toEqual(["headers"]);
    expect(fields(draft({ headers: [{ name: "host", value: "x", stored: false }] }))).toEqual(["headers"]);
    expect(fields(draft({ authType: "bearer", bearerToken: "t", headers: [{ name: "authorization", value: "x", stored: false }] }))).toEqual(["headers"]);
  });

  it("does not need a name to test", () => {
    expect(fields(draft({ name: "" }), null, true)).toEqual([]);
    expect(fields(draft({ name: "" }))).toEqual(["name"]);
  });
});

describe("secret handling", () => {
  it("a new monitor sends every secret it was given", () => {
    const req = toMonitorRequest({ ...toMonitorDraft(null), name: "A", url: "https://a.example.com/", method: "POST", authType: "bearer",
      bearerToken: "tok", body: "{}", headers: [{ name: "X-K", value: "v", stored: false }] }, null);
    expect(req).toMatchObject({ bearer_token: "tok", body: "{}", headers: [{ name: "X-K", value: "v" }] });
  });

  it("an edit leaves out what was not retyped, so the server keeps it", () => {
    const saved = monitor();
    const req = toMonitorRequest(toMonitorDraft(saved), saved);
    expect(req).not.toHaveProperty("bearer_token");
    expect(req).not.toHaveProperty("headers");
    expect(req).not.toHaveProperty("body");
    expect(req).not.toHaveProperty("basic_password");
  });

  it("an edit keeps a stored header by name with a null value, and sends new or removed ones", () => {
    const saved = monitor({ header_names: ["X-Api-Key", "X-Old"] });
    const d = toMonitorDraft(saved);
    const headers = [...d.headers.slice(0, 1), { name: "X-New", value: "n", stored: false }];
    expect(toMonitorRequest({ ...d, headers }, saved).headers).toEqual([
      { name: "X-Api-Key", value: null }, { name: "X-New", value: "n" },
    ]);
  });

  it("leaving POST drops a stored body; staying keeps it", () => {
    const saved = monitor({ method: "POST" });
    expect(toMonitorRequest({ ...toMonitorDraft(saved), method: "GET" }, saved).body).toBe("");
    expect(toMonitorRequest(toMonitorDraft(saved), saved)).not.toHaveProperty("body");
  });

  it("a test of a saved monitor names it and sends only the draft's changes", () => {
    const saved = monitor();
    const req = toTestRequest({ ...toMonitorDraft(saved), name: "" }, saved);
    expect(req.monitor_id).toBe("m1");
    expect(req).not.toHaveProperty("name");
    expect(req).not.toHaveProperty("bearer_token");
  });

  it("the form shows header names and a configured state, never a stored value", () => {
    const saved = monitor();
    const html = renderToStaticMarkup(
      <MonitorFields draft={toMonitorDraft(saved)} monitor={saved} groups={[]} errors={{}} onChange={() => undefined} />,
    );
    expect(html).toContain('value="X-Api-Key"');
    expect(html).toContain("Configured — leave empty to keep");
    expect(html).toContain("A secret is configured");
    expect(html).toContain("not sealed like a secret");
    expect(html).not.toMatch(/type="password"[^>]*value="[^"]+"/);
  });

  it("warns about a query string only when there is one", () => {
    expect(hasQueryString("https://a.example.com/x?token=1")).toBe(true);
    expect(hasQueryString("https://a.example.com/x")).toBe(false);
  });
});

describe("runMonitorTest", () => {
  const d = { ...toMonitorDraft(null), url: "https://a.example.com/" };

  it("keeps only the category, status code and response time", async () => {
    const send = vi.fn().mockResolvedValue({ status: "down", failure: "http_status", status_code: 503, response_ms: 40, body: "leak" });
    const out = await runMonitorTest(d, null, send);
    expect(out.result).toEqual({ kind: "result", result: { status: "down", failure: "http_status", status_code: 503, response_ms: 40 } });
    if (out.result?.kind === "result") expect(testSummary(out.result.result)).toBe("Down · Unexpected status code · status 503 · 40 ms");
  });

  it("counts down when limited and puts a refused field on the field", async () => {
    const limited = await runMonitorTest(d, null, () => Promise.reject(new ApiError(429, { code: "rate_limited", message: "x" }, 7)), () => 1000);
    expect(limited.result).toEqual({ kind: "limited", retryAt: 8000 });
    const refused = await runMonitorTest(d, null, () =>
      Promise.reject(new ApiError(400, { code: "invalid_argument", message: "the URL's address is not allowed", details: { field: "url" } })));
    expect(refused.errors).toEqual({ url: "the URL's address is not allowed" });
  });

  it("does not send an invalid draft", async () => {
    const send = vi.fn();
    expect((await runMonitorTest({ ...d, url: "ftp://x" }, null, send)).errors.url).toBeDefined();
    expect(send).not.toHaveBeenCalled();
  });
});

describe("formatUptime", () => {
  it("never rounds up to 100%", () => {
    expect(formatUptime(99.999)).toBe("99.99%");
    expect(formatUptime(100)).toBe("100%");
    expect(formatUptime(null)).toBe("—");
  });
});

describe("WebsitesPage", () => {
  const manager = user({ can_manage_notifications: true });

  it("has an empty state with a call to action for managers only", () => {
    const empty: ListMonitorsResponse = { monitors: [], groups: [], probe: offline };
    expect(renderPage(manager, empty)).toContain("No websites yet");
    expect(renderPage(manager, empty)).toContain("Add website");
    expect(renderPage(user(), empty)).not.toContain("Add website");
  });

  it("lists monitors in groups with uptime and heartbeats, and shows the first in detail", () => {
    const m = monitor({ group_id: "g1" });
    const html = renderPage(manager, { monitors: [m, monitor({ id: "m2", name: "Blog" })], groups: [group("g1", "Shops", 0)], probe: offline }, detailOf(m));
    for (const text of ["Shops", "No group", "Shop", "Blog", "99.50%", "Last 1 checks", "Uptime", "98.00%", "Went down", "Unexpected status code", "87 ms", "Pause", "Edit", "Delete", "Groups", "Add website"]) {
      expect(html).toContain(text);
    }
  });

  it("shows a viewer the redacted monitor and no write controls", () => {
    const m = viewerCopy(monitor());
    const html = renderPage(user(), { monitors: [m], groups: [], probe: offline }, detailOf(m));
    expect(html).toContain("hidden from your role");
    for (const text of ["token=abc", "X-Api-Key", "bearer", ">Pause<", ">Edit<", ">Delete<", "Add website", ">Groups<"]) {
      expect(html).not.toContain(text);
    }
  });

  it("says when the probe is offline", () => {
    const m = monitor();
    const html = renderPage(user(), { monitors: [m], groups: [], probe: { offline: true, since: at } }, detailOf(m));
    expect(html).toContain("The probe is offline");
  });
});

describe("the Websites nav entry", () => {
  it("is under Monitor for the websites page grant alone", () => {
    const nav = NAV_PAGES.find((p) => p.to === "/websites");
    if (!nav) throw new Error("no Websites nav entry");
    expect(nav.section).toBe("Monitor");
    expect(canSeeNavPage(user(), nav)).toBe(true);
    expect(canSeeNavPage(user({ page_access: [], can_manage_notifications: true }), nav)).toBe(false);
  });

  it("redirects a visit without the grant", () => {
    const render = (me: CurrentUser) => {
      const client = new QueryClient();
      client.setQueryData(["auth", "me"], me);
      return renderToStaticMarkup(
        <QueryClientProvider client={client}>
          <MemoryRouter initialEntries={["/websites"]}>
            <AuthProvider><ToastProvider><App /></ToastProvider></AuthProvider>
          </MemoryRouter>
        </QueryClientProvider>,
      );
    };
    const main = (html: string) => /<main[^>]*>(.*)<\/main>/s.exec(html)?.[1] ?? "";
    expect(render(user())).toContain('href="/websites"');
    expect(main(render(user()))).toContain("animate-pulse");
    expect(main(render(user({ page_access: [] })))).not.toContain("animate-pulse");
  });
});
