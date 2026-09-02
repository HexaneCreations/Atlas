import { describe, expect, it } from "vitest";
import type { CurrentUser, PageAccessEntry } from "../api/types";
import { canReachPage } from "./pageAccess";

function user(pageAccess: PageAccessEntry[]): CurrentUser {
  return {
    user_id: "u1",
    username: "abhishek",
    can_manage_users: false,
    is_superadmin: false,
    page_access: pageAccess,
  };
}

describe("canReachPage", () => {
  it("is false for every page when there is no signed-in user", () => {
    expect(canReachPage(null, "overview")).toBe(false);
    expect(canReachPage(null, "containers")).toBe(false);
  });

  it("keeps Overview reachable even with an empty page_access", () => {
    expect(canReachPage(user([]), "overview")).toBe(true);
  });

  it("allows a page the user holds a node-scoped grant for", () => {
    const u = user([{ page: "containers", fleet_wide: false, node_ids: ["cyrene-dev-v2"] }]);
    expect(canReachPage(u, "containers")).toBe(true);
  });

  it("allows a page the user holds a fleet-wide grant for", () => {
    expect(canReachPage(user([{ page: "processes", fleet_wide: true }]), "processes")).toBe(true);
  });

  it("denies a page absent from page_access (grant for one page never leaks to another)", () => {
    const u = user([{ page: "containers", fleet_wide: false, node_ids: ["cyrene-dev-v2"] }]);
    expect(canReachPage(u, "processes")).toBe(false);
    expect(canReachPage(u, "nodes")).toBe(false);
  });
});
