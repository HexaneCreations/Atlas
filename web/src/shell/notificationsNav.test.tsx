import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import { App } from "../App";
import type { CurrentUser, Node } from "../api/types";
import { AuthProvider } from "../auth/AuthContext";
import { AlertMute } from "../pages/nodes/AlertMute";
import { ToastProvider } from "../components/Toast";
import { canSeeNavPage } from "./pageAccess";
import { NAV_PAGES } from "./pages";

// Both read the browser while rendering; neither is what is tested here.
vi.mock("./useTheme", () => ({ useTheme: () => ({ theme: "dark", toggle: () => undefined }) }));
vi.mock("./Topbar", () => ({ Topbar: () => null }));

function user(over: Partial<CurrentUser>): CurrentUser {
  return {
    user_id: "u1", username: "someone", can_manage_users: false, is_superadmin: false,
    can_manage_notifications: false, can_enroll_fleet: false, page_access: [], ...over,
  };
}

function render(me: CurrentUser, path: string, children = <App />): string {
  const client = new QueryClient();
  client.setQueryData(["auth", "me"], me);
  return renderToStaticMarkup(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <AuthProvider><ToastProvider>{children}</ToastProvider></AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const main = (html: string) => /<main[^>]*>(.*)<\/main>/s.exec(html)?.[1] ?? "";

describe("the Notifications page", () => {
  it("is in the sidebar for can_manage_notifications alone", () => {
    expect(render(user({ can_manage_notifications: true }), "/")).toContain('href="/notifications"');
    expect(render(user({ can_manage_users: true, is_superadmin: true }), "/")).not.toContain('href="/notifications"');
    expect(render(user({}), "/")).not.toContain('href="/notifications"');
  });

  it("is routable for can_manage_notifications alone", () => {
    // Allowed: the lazy page is loading. Refused: redirected, nothing renders.
    expect(main(render(user({ can_manage_notifications: true }), "/notifications"))).toContain("animate-pulse");
    expect(main(render(user({ can_manage_users: true }), "/notifications"))).not.toContain("animate-pulse");
  });

  it("leaves the other nav entries as they were", () => {
    const seen = (u: CurrentUser | null) => NAV_PAGES.filter((p) => canSeeNavPage(u, p)).map((p) => p.label);
    expect(seen(null)).toEqual([]);
    expect(seen(user({}))).toEqual(["Overview"]);
    expect(seen(user({ can_manage_users: true, page_access: [{ page: "users", fleet_wide: true }] }))).toEqual(["Overview", "Users"]);
    expect(seen(user({ page_access: [{ page: "users", fleet_wide: true }, { page: "nodes", fleet_wide: true }] }))).toEqual(["Overview", "Nodes"]);
  });
});

describe("the node inspector's mute", () => {
  it("is there for can_manage_notifications alone", () => {
    const node = { node_id: "n1", hostname: "web-1", alerts_muted_until: null } as Node;
    expect(render(user({ can_manage_notifications: true }), "/", <AlertMute node={node} />)).toContain("Until I unmute");
    expect(render(user({ can_manage_users: true }), "/", <AlertMute node={node} />)).not.toContain("Mute");
  });
});
