import { describe, expect, it } from "vitest";
import type { AccessBundle, AccessPageGrant, AccessRoleGrant, EditableRole, Page, UserAccessResponse } from "../api/types";
import {
  EMPTY_ACCESS,
  FLEET,
  applyEdit,
  changeText,
  diff,
  editorReducer,
  initialEditorState,
  nodeRows,
  normalize,
  pageLock,
  problems,
  saveErrorKind,
  toBody,
  toDraft,
} from "./accessEditor";

let seq = 0;
const at = "2026-01-01T00:00:00Z";

function role(r: EditableRole, node?: string): AccessRoleGrant {
  seq++;
  return node ? { id: `r${seq}`, role: r, node_id: node, fleet_wide: false, granted_at: at } : { id: `r${seq}`, role: r, fleet_wide: true, granted_at: at };
}

function page(p: Page, node?: string): AccessPageGrant {
  seq++;
  return node ? { id: `p${seq}`, page: p, node_id: node, fleet_wide: false, granted_at: at } : { id: `p${seq}`, page: p, fleet_wide: true, granted_at: at };
}

function bundle(name: string, pages: Page[], node?: string): AccessBundle {
  return node
    ? { id: `b-${name}`, role_access: name, node_id: node, fleet_wide: false, pages, granted_at: at }
    : { id: `b-${name}`, role_access: name, fleet_wide: true, pages, granted_at: at };
}

function server(partial: Partial<UserAccessResponse>): UserAccessResponse {
  return { ...EMPTY_ACCESS, version: "v1", ...partial };
}

const lines = (s: UserAccessResponse, d = toDraft(s)) => diff(s, d).map(changeText);

describe("toDraft", () => {
  it("shows one role per scope, the highest", () => {
    const s = server({ roles: [role("viewer", "n1"), role("operator", "n1")] });
    expect(toDraft(s).roles).toEqual({ n1: "operator" });
  });

  it("drops node rows the fleet-wide row covers", () => {
    const s = server({
      roles: [role("operator"), role("viewer", "n1"), role("admin", "n2")],
      pages: [page("ports"), page("ports", "n1"), page("disks", "n1")],
    });
    const d = toDraft(s);
    expect(d.roles).toEqual({ [FLEET]: "operator", n2: "admin" });
    expect(d.pages).toEqual({ [FLEET]: ["ports"], n1: ["disks"] });
  });

  it("an unchanged draft of a clean state has no changes", () => {
    const s = server({ roles: [role("viewer", "n1")], pages: [page("overview"), page("ports", "n1")] });
    expect(diff(s, toDraft(s))).toEqual([]);
  });
});

describe("normalize", () => {
  it("keeps a node role above the fleet-wide role", () => {
    expect(normalize({ roles: { [FLEET]: "viewer", n1: "admin", n2: "viewer" }, pages: {} }).roles).toEqual({
      [FLEET]: "viewer",
      n1: "admin",
    });
  });
});

describe("diff", () => {
  it("labels rows the editor does not model as removed", () => {
    const s = server({
      roles: [role("viewer", "n1"), role("operator", "n1"), role("admin"), role("operator", "n2")],
      pages: [page("ports"), page("ports", "n3")],
    });
    expect(lines(s)).toEqual([
      "Removed: page ports (node n3) — covered by the fleet-wide tick",
      "Removed: role operator (node n1) — covered by the fleet-wide admin role",
      "Removed: role viewer (node n1) — a higher role is already set on this scope",
      "Removed: role operator (node n2) — covered by the fleet-wide admin role",
    ]);
  });

  it("a role change is a revoke plus a grant", () => {
    const s = server({ roles: [role("viewer", "n1")] });
    const d = applyEdit(s, toDraft(s), { kind: "role", row: "n1", role: "operator" });
    expect(lines(s, d)).toEqual(["Revoke role viewer (node n1)", "Grant role operator (node n1)"]);
  });

  it("raising the fleet-wide role removes node roles it now covers, labelled", () => {
    const s = server({ roles: [role("viewer", "n1")] });
    const d = applyEdit(s, toDraft(s), { kind: "role", row: FLEET, role: "operator" });
    expect(lines(s, d)).toEqual([
      "Removed: role viewer (node n1) — covered by the fleet-wide operator role",
      "Grant role operator (fleet-wide)",
    ]);
  });
});

describe("applyEdit", () => {
  const s = server({ roles: [role("viewer", "n1")], pages: [page("ports", "n1"), page("disks", "n1")] });

  it("setting a node role to none clears that row's pages", () => {
    const d = applyEdit(s, toDraft(s), { kind: "role", row: "n1", role: "none" });
    expect(d).toEqual({ roles: {}, pages: {} });
    expect(lines(s, d)).toEqual([
      "Revoke page disks (node n1)",
      "Revoke page ports (node n1)",
      "Revoke role viewer (node n1)",
    ]);
  });

  it("ignores page edits on a node row with no role", () => {
    const d = toDraft(s);
    expect(applyEdit(s, d, { kind: "page", row: "n2", page: "ports", on: true })).toBe(d);
  });

  it("a fleet-wide role enables every node row's pages", () => {
    const d = applyEdit(EMPTY_ACCESS, toDraft(EMPTY_ACCESS), { kind: "role", row: FLEET, role: "viewer" });
    expect(applyEdit(EMPTY_ACCESS, d, { kind: "page", row: "n2", page: "ports", on: true }).pages).toEqual({ n2: ["ports"] });
  });

  it("select all ticks every unlocked page on the row", () => {
    const withBundle = server({ roles: [role("viewer", "n1")], bundles: [bundle("c", ["containers"])] });
    const d = applyEdit(withBundle, toDraft(withBundle), { kind: "selectAll", row: "n1" });
    expect(d.pages.n1).toEqual(["processes", "services", "cron", "ports", "disks"]);
    const f = applyEdit(withBundle, d, { kind: "selectAll", row: FLEET });
    expect(f.pages[FLEET]).toEqual(["overview", "nodes", "users", "processes", "services", "cron", "ports", "disks"]);
    expect(f.pages.n1).toBeUndefined();
  });
});

describe("pageLock", () => {
  it("bundle pages are ticked and locked; a fleet-wide tick locks node rows", () => {
    const s = server({ bundles: [bundle("full", ["containers"]), bundle("node", ["disks"], "n1")] });
    const d = { roles: {}, pages: { [FLEET]: ["ports" as Page] } };
    expect(pageLock(s, d, "n2", "containers")).toEqual({ ticked: true, reason: "from bundle full" });
    expect(pageLock(s, d, "n1", "disks")).toEqual({ ticked: true, reason: "from bundle node" });
    expect(pageLock(s, d, FLEET, "disks")).toEqual({ ticked: false, reason: "bundle node grants this on node n1" });
    expect(pageLock(s, d, "n2", "ports")).toEqual({ ticked: true, reason: "via fleet-wide" });
    expect(pageLock(s, d, "n2", "disks")).toBeNull();
  });
});

describe("problems", () => {
  it("mirrors the server's page-needs-a-role rule", () => {
    expect(problems({ roles: {}, pages: { [FLEET]: ["overview", "users"] } })).toEqual([
      "overview (fleet-wide) needs a role somewhere",
    ]);
    expect(problems({ roles: { n1: "viewer" }, pages: { [FLEET]: ["overview"], n2: ["ports"] } })).toEqual([
      "ports on node n2 needs a role on that node or fleet-wide",
    ]);
    expect(problems({ roles: { [FLEET]: "viewer" }, pages: { n2: ["ports"] } })).toEqual([]);
  });
});

describe("toBody", () => {
  it("sends explicit scopes", () => {
    expect(toBody({ roles: { [FLEET]: "viewer", n1: "admin" }, pages: { n1: ["ports"] } })).toEqual({
      roles: [
        { role: "viewer", fleet_wide: true },
        { role: "admin", node_id: "n1", fleet_wide: false },
      ],
      pages: [{ page: "ports", node_id: "n1", fleet_wide: false }],
    });
  });
});

describe("editorReducer", () => {
  it("discard returns to the loaded state; load replaces it", () => {
    const s = server({ roles: [role("viewer", "n1")] });
    let st = initialEditorState(s);
    st = editorReducer(st, { type: "edit", edit: { kind: "role", row: "n1", role: "admin" } });
    st = editorReducer(st, { type: "edit", edit: { kind: "page", row: "n1", page: "ports", on: true } });
    expect(diff(st.server, st.draft)).toHaveLength(3);
    st = editorReducer(st, { type: "discard" });
    expect(diff(st.server, st.draft)).toEqual([]);
    const reloaded = server({ version: "v2", roles: [] });
    st = editorReducer(st, { type: "load", server: reloaded });
    expect(st.server.version).toBe("v2");
    expect(st.draft).toEqual({ roles: {}, pages: {} });
  });
});

describe("nodeRows", () => {
  it("keeps nodes the user holds rows on even when no longer reported", () => {
    const s = server({ roles: [role("viewer", "gone")] });
    expect(nodeRows(s, [{ node_id: "n2" }, { node_id: "n1" }])).toEqual(["gone", "n1", "n2"]);
  });
});

describe("saveErrorKind", () => {
  it("only the stale-version 409 gets the Reload action", () => {
    expect(saveErrorKind(409, "access changed since you opened it; reload")).toBe("stale");
    expect(saveErrorKind(409, "page \"containers\" on fleet-wide is already granted by a role bundle")).toBe("server");
    expect(saveErrorKind(412, "this is the last user with fleet-wide admin access")).toBe("server");
  });
});
