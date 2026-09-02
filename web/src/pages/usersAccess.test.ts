import { describe, expect, it } from "vitest";
import type { Grant, ListUserPageAccessResponse, Page, PageAccessAssignment } from "../api/types";
import {
  LAST_ADMIN_REVOKE_MESSAGE,
  PROTECTED_SUPERADMIN_MESSAGE,
  SELF_REVOKE_MESSAGE,
  effectivePages,
  holdsSuperadmin,
  revokeGuard,
} from "./usersAccess";

function grant(partial: Partial<Grant>): Grant {
  return {
    id: "g1",
    role: "admin",
    fleet_wide: true,
    granted_at: "2026-01-01T00:00:00Z",
    ...partial,
  };
}

function directGrant(page: Page): PageAccessAssignment {
  return { id: `d-${page}`, fleet_wide: true, granted_at: "2026-01-01T00:00:00Z", page };
}

function bundleAssignment(name: string): PageAccessAssignment {
  return { id: `b-${name}`, fleet_wide: true, granted_at: "2026-01-01T00:00:00Z", role_access: name };
}

function access(partial: Partial<ListUserPageAccessResponse>): ListUserPageAccessResponse {
  return { pages: [], role_access: [], ...partial };
}

describe("revokeGuard", () => {
  it("disables every revoke when the panel shows the signed-in user's own account", () => {
    expect(revokeGuard({ isSelf: true })).toEqual({ disabled: true, reason: SELF_REVOKE_MESSAGE });
  });

  it("leaves revoke clickable when viewing another user", () => {
    expect(revokeGuard({ isSelf: false })).toEqual({ disabled: false });
  });

  it("reports the last-admin reason ahead of the self reason for a role grant", () => {
    expect(revokeGuard({ isSelf: true, lastAdmin: true })).toEqual({
      disabled: true,
      reason: LAST_ADMIN_REVOKE_MESSAGE,
    });
  });

  it("still blocks a last-admin role grant when viewing another user", () => {
    expect(revokeGuard({ isSelf: false, lastAdmin: true })).toEqual({
      disabled: true,
      reason: LAST_ADMIN_REVOKE_MESSAGE,
    });
  });

  it("blocks every action against the superadmin, ahead of both other reasons", () => {
    expect(revokeGuard({ isSelf: true, lastAdmin: true, protectedSuperadmin: true })).toEqual({
      disabled: true,
      reason: PROTECTED_SUPERADMIN_MESSAGE,
    });
    expect(revokeGuard({ isSelf: false, protectedSuperadmin: true })).toEqual({
      disabled: true,
      reason: PROTECTED_SUPERADMIN_MESSAGE,
    });
  });
});

describe("holdsSuperadmin", () => {
  it("is true only for an active fleet-wide superadmin grant", () => {
    expect(holdsSuperadmin([grant({ role: "superadmin", fleet_wide: true })])).toBe(true);
  });

  it("ignores a node-scoped superadmin grant — the protected tier is fleet-wide", () => {
    expect(holdsSuperadmin([grant({ role: "superadmin", fleet_wide: false, node_id: "n1" })])).toBe(false);
  });

  it("is false for admin, and for a user with no grants", () => {
    expect(holdsSuperadmin([grant({ role: "admin", fleet_wide: true })])).toBe(false);
    expect(holdsSuperadmin([])).toBe(false);
    expect(holdsSuperadmin(undefined)).toBe(false);
  });
});

describe("effectivePages", () => {
  const bundles = new Map<string, Page[]>([
    [
      "full-access",
      ["overview", "nodes", "containers", "processes", "services", "cron", "ports", "disks", "users"],
    ],
    ["containers-only", ["containers", "processes"]],
  ]);

  it("counts pages reachable only through an assigned bundle", () => {
    const pa = access({ role_access: [bundleAssignment("full-access")] });
    expect(effectivePages(pa, bundles).size).toBe(9);
  });

  it("is empty only when the user has neither a direct grant nor a bundle", () => {
    expect(effectivePages(access({}), bundles).size).toBe(0);
  });

  it("unions direct grants with bundle pages without double-counting", () => {
    const pa = access({
      pages: [directGrant("containers"), directGrant("disks")],
      role_access: [bundleAssignment("containers-only")],
    });
    expect([...effectivePages(pa, bundles)].sort()).toEqual(["containers", "disks", "processes"]);
  });

  it("ignores a bundle assignment whose definition is unknown", () => {
    const pa = access({ role_access: [bundleAssignment("since-deleted")] });
    expect(effectivePages(pa, bundles).size).toBe(0);
  });
});
