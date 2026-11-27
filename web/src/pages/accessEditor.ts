/**
 * The access editor's model, as pure functions: the server's state turned
 * into an editable draft, the edits a user can make, and the readable diff a
 * save will perform.
 *
 * Mirrors internal/core/access on the server (normalize, the "a page needs a
 * role" rule, and the key-by-key diff), so what this shows before Save is what
 * the PUT does. Keep the two in sync. The server stays the boundary: anything
 * this gets wrong comes back as a 400/409 rather than being applied.
 */
import type {
  AccessBody,
  AccessBundle,
  EditableRole,
  Page,
  PutUserAccessRequest,
  UserAccessResponse,
} from "../api/types";

/** Row key: "*" is the Fleet-wide row; anything else is a node id. */
export const FLEET = "*";
export type RowKey = string;

export type RoleChoice = EditableRole | "none";

export const EDITABLE_ROLES: readonly EditableRole[] = ["viewer", "operator", "admin"];

/** Every page, in display order — mirrors pageauthz.KnownPages. */
export const ALL_PAGES: readonly Page[] = [
  "overview",
  "nodes",
  "users",
  "containers",
  "processes",
  "services",
  "cron",
  "ports",
  "disks",
  "websites",
];

/** Pages with no per-node concept — mirrors pageauthz.FleetOnlyPages. */
export const FLEET_ONLY_PAGES: ReadonlySet<Page> = new Set<Page>(["overview", "nodes", "users", "websites"]);

export const NODE_PAGES: readonly Page[] = ALL_PAGES.filter((p) => !FLEET_ONLY_PAGES.has(p));

/** Strict permission nesting viewer ⊂ operator ⊂ admin — mirrors
 *  access.roleRank. */
const RANK: Record<RoleChoice, number> = { none: 0, viewer: 1, operator: 2, admin: 3 };

/** The editable state: at most one role per row, and the direct pages ticked
 *  on each row. Rows absent from `roles` hold no role. */
export interface Draft {
  roles: Readonly<Record<RowKey, EditableRole>>;
  pages: Readonly<Record<RowKey, readonly Page[]>>;
}

export const EMPTY_ACCESS: UserAccessResponse = { version: "", roles: [], pages: [], bundles: [] };

function rowOf(a: { fleet_wide: boolean; node_id?: string }): RowKey {
  return a.fleet_wide ? FLEET : (a.node_id ?? "");
}

function sortPages(pages: Iterable<Page>): Page[] {
  const set = new Set(pages);
  return ALL_PAGES.filter((p) => set.has(p));
}

/**
 * The draft the editor opens with. A scope holding several roles shows the
 * highest one; rows another row already covers are dropped by [normalize] —
 * the save will remove them, and [diff] says so.
 */
export function toDraft(server: UserAccessResponse): Draft {
  const roles: Record<RowKey, EditableRole> = {};
  for (const g of server.roles) {
    const row = rowOf(g);
    const cur = roles[row];
    if (!cur || RANK[g.role] > RANK[cur]) roles[row] = g.role;
  }
  const pages: Record<RowKey, Page[]> = {};
  for (const g of server.pages) {
    const row = rowOf(g);
    pages[row] = sortPages([...(pages[row] ?? []), g.page]);
  }
  return normalize({ roles, pages });
}

/**
 * Drops rows another row covers — mirrors access.normalize: a node role at or
 * below the fleet-wide role, and a node page the same fleet-wide page covers.
 */
export function normalize(d: Draft): Draft {
  const fleetRank = RANK[d.roles[FLEET] ?? "none"];
  const roles: Record<RowKey, EditableRole> = {};
  for (const [row, role] of Object.entries(d.roles)) {
    if (row !== FLEET && RANK[role] <= fleetRank) continue;
    roles[row] = role;
  }
  const fleetPages = new Set(d.pages[FLEET] ?? []);
  const pages: Record<RowKey, Page[]> = {};
  for (const [row, list] of Object.entries(d.pages)) {
    const kept = row === FLEET ? sortPages(list) : sortPages(list.filter((p) => !fleetPages.has(p)));
    if (kept.length > 0) pages[row] = kept;
  }
  return { roles, pages };
}

/** Whether a row holds any role, directly or through the fleet-wide row. */
export function rowHasRole(d: Draft, row: RowKey): boolean {
  return Boolean(d.roles[row]) || (row !== FLEET && Boolean(d.roles[FLEET]));
}

/** The pages a row offers: every page on Fleet-wide, node pages elsewhere. */
export function pagesForRow(row: RowKey): readonly Page[] {
  return row === FLEET ? ALL_PAGES : NODE_PAGES;
}

export interface PageLock {
  /** Whether the box shows ticked. */
  ticked: boolean;
  reason: string;
}

/**
 * Why a page box cannot be changed, if it cannot: a bundle already grants the
 * page on an overlapping scope (a direct grant would be refused with 409), or
 * the Fleet-wide row's tick already covers it.
 */
export function pageLock(server: UserAccessResponse, d: Draft, row: RowKey, page: Page): PageLock | null {
  const bundle = coveringBundle(server.bundles, row, page);
  if (bundle) {
    const exact = bundle.fleet_wide || row !== FLEET;
    return exact
      ? { ticked: true, reason: `from bundle ${bundle.role_access}` }
      : { ticked: false, reason: `bundle ${bundle.role_access} grants this on node ${bundle.node_id ?? ""}` };
  }
  if (row !== FLEET && (d.pages[FLEET] ?? []).includes(page)) {
    return { ticked: true, reason: "via fleet-wide" };
  }
  return null;
}

/** A bundle whose scope overlaps row and that carries page — the same
 *  overlap rule as pageauthz.Scope.Overlaps. */
function coveringBundle(bundles: readonly AccessBundle[], row: RowKey, page: Page): AccessBundle | undefined {
  const overlapping = bundles.filter(
    (b) => b.pages.includes(page) && (b.fleet_wide || row === FLEET || b.node_id === row),
  );
  // Prefer a bundle that covers this row exactly, so the label reads "from".
  return overlapping.find((b) => b.fleet_wide || b.node_id === row) ?? overlapping[0];
}

export type Edit =
  | { kind: "role"; row: RowKey; role: RoleChoice }
  | { kind: "page"; row: RowKey; page: Page; on: boolean }
  | { kind: "selectAll"; row: RowKey };

/**
 * Applies one edit and re-normalizes. Setting a node row's role to none
 * clears that row's pages when nothing else (a fleet-wide role) gives the row
 * a role; page edits on a node row with no role at all are ignored. Locked
 * boxes ([pageLock]) are never changed by Select all.
 */
export function applyEdit(server: UserAccessResponse, d: Draft, e: Edit): Draft {
  let roles: Record<RowKey, EditableRole> = { ...d.roles };
  let pages: Record<RowKey, readonly Page[]> = { ...d.pages };
  switch (e.kind) {
    case "role": {
      roles = e.role === "none" ? without(roles, e.row) : { ...roles, [e.row]: e.role };
      if (e.row !== FLEET && !rowHasRole({ roles, pages }, e.row)) pages = without(pages, e.row);
      break;
    }
    case "page": {
      if (e.row !== FLEET && !rowHasRole(d, e.row)) return d;
      if (pageLock(server, d, e.row, e.page)) return d;
      const cur = new Set(pages[e.row] ?? []);
      if (e.on) cur.add(e.page);
      else cur.delete(e.page);
      pages[e.row] = sortPages(cur);
      break;
    }
    case "selectAll": {
      if (e.row !== FLEET && !rowHasRole(d, e.row)) return d;
      const cur = new Set(pages[e.row] ?? []);
      for (const p of pagesForRow(e.row)) {
        if (!pageLock(server, d, e.row, p)) cur.add(p);
      }
      pages[e.row] = sortPages(cur);
      break;
    }
  }
  return normalize({ roles, pages });
}

function without<T>(o: Readonly<Record<RowKey, T>>, key: RowKey): Record<RowKey, T> {
  return Object.fromEntries(Object.entries(o).filter(([k]) => k !== key));
}

/**
 * Pages the server would refuse with 400 — mirrors
 * access.checkPagesHaveRoles: a page on a node needs a role on that node or
 * a fleet-wide role; a fleet-wide page other than Users needs at least one
 * role somewhere.
 */
export function problems(d: Draft): string[] {
  const anyRole = Object.keys(d.roles).length > 0;
  const out: string[] = [];
  for (const [row, list] of Object.entries(d.pages)) {
    for (const p of list) {
      if (p === "users") continue;
      if (row === FLEET ? !anyRole : !rowHasRole(d, row)) {
        out.push(row === FLEET ? `${p} (fleet-wide) needs a role somewhere` : `${p} on node ${row} needs a role on that node or fleet-wide`);
      }
    }
  }
  return out;
}

/** The request body for a draft. */
export function toBody(d: Draft): AccessBody {
  const scope = (row: RowKey) => (row === FLEET ? { fleet_wide: true } : { node_id: row, fleet_wide: false });
  return {
    roles: Object.entries(d.roles).map(([row, role]) => ({ role, ...scope(row) })),
    pages: Object.entries(d.pages).flatMap(([row, list]) => list.map((page) => ({ page, ...scope(row) }))),
  };
}

export function toRequest(server: UserAccessResponse, d: Draft): PutUserAccessRequest {
  return { version: server.version, ...toBody(d) };
}

export interface Change {
  op: "grant" | "revoke";
  kind: "role" | "page";
  row: RowKey;
  value: string;
  /** Set when the row is removed because the editor does not model it. */
  removedBecause?: string;
}

function scopeText(row: RowKey): string {
  return row === FLEET ? "fleet-wide" : `node ${row}`;
}

/** One line per change, for the save preview. */
export function changeText(c: Change): string {
  const what = `${c.kind === "role" ? "role" : "page"} ${c.value} (${scopeText(c.row)})`;
  if (c.op === "grant") return `Grant ${what}`;
  if (c.removedBecause) return `Removed: ${what} — ${c.removedBecause}`;
  return `Revoke ${what}`;
}

/**
 * Every grant and revoke saving d would perform — mirrors access.Plan's
 * key-by-key diff, so the count matches the audit rows the server writes.
 * Server rows the editor cannot show (a second role on one scope, a node
 * role or page the fleet-wide row covers) are revoked by any save and are
 * labelled with `removedBecause`.
 */
export function diff(server: UserAccessResponse, d: Draft): Change[] {
  const out: Change[] = [];
  const wantRole = new Set(Object.entries(d.roles).map(([row, role]) => `${row}|${role}`));
  const haveRole = new Set(server.roles.map((g) => `${rowOf(g)}|${g.role}`));
  const fleetRole = d.roles[FLEET];

  const topOnScope = new Map<RowKey, EditableRole>();
  for (const g of server.roles) {
    const row = rowOf(g);
    const cur = topOnScope.get(row);
    if (!cur || RANK[g.role] > RANK[cur]) topOnScope.set(row, g.role);
  }

  for (const g of server.roles) {
    const row = rowOf(g);
    if (wantRole.has(`${row}|${g.role}`)) continue;
    let removedBecause: string | undefined;
    if (topOnScope.get(row) !== g.role) {
      removedBecause = `a higher role is already set on this scope`;
    } else if (row !== FLEET && fleetRole && RANK[g.role] <= RANK[fleetRole]) {
      removedBecause = `covered by the fleet-wide ${fleetRole} role`;
    }
    out.push({ op: "revoke", kind: "role", row, value: g.role, ...(removedBecause ? { removedBecause } : {}) });
  }
  for (const [row, role] of Object.entries(d.roles)) {
    if (!haveRole.has(`${row}|${role}`)) out.push({ op: "grant", kind: "role", row, value: role });
  }

  const wantPage = new Set(Object.entries(d.pages).flatMap(([row, list]) => list.map((p) => `${row}|${p}`)));
  const havePage = new Set(server.pages.map((g) => `${rowOf(g)}|${g.page}`));
  const fleetPages = new Set(d.pages[FLEET] ?? []);
  for (const g of server.pages) {
    const row = rowOf(g);
    if (wantPage.has(`${row}|${g.page}`)) continue;
    const covered = row !== FLEET && fleetPages.has(g.page);
    out.push({
      op: "revoke",
      kind: "page",
      row,
      value: g.page,
      ...(covered ? { removedBecause: "covered by the fleet-wide tick" } : {}),
    });
  }
  for (const [row, list] of Object.entries(d.pages)) {
    for (const p of list) {
      if (!havePage.has(`${row}|${p}`)) out.push({ op: "grant", kind: "page", row, value: p });
    }
  }

  const order = (c: Change) => `${c.op === "revoke" ? 0 : 1}|${c.kind}|${c.row === FLEET ? "" : c.row}|${c.value}`;
  return out.sort((a, b) => order(a).localeCompare(order(b)));
}

/** Node rows to show: every known node, plus any node the user holds rows on
 *  that is no longer reported (so those rows stay visible and clearable). */
export function nodeRows(server: UserAccessResponse, known: readonly { node_id: string }[]): RowKey[] {
  const ids = new Set(known.map((n) => n.node_id));
  for (const g of [...server.roles, ...server.pages, ...server.bundles]) {
    if (!g.fleet_wide && g.node_id) ids.add(g.node_id);
  }
  return [...ids].sort();
}

// --------------------------------------------------- editor state ----

export interface EditorState {
  server: UserAccessResponse;
  draft: Draft;
}

export type EditorAction =
  | { type: "load"; server: UserAccessResponse }
  | { type: "edit"; edit: Edit }
  | { type: "discard" };

/** The editor's state transitions: load (or reload) resets the draft to the
 *  server state, discard returns to the last load. */
export function editorReducer(s: EditorState, a: EditorAction): EditorState {
  switch (a.type) {
    case "load":
      return { server: a.server, draft: toDraft(a.server) };
    case "edit":
      return { ...s, draft: applyEdit(s.server, s.draft, a.edit) };
    case "discard":
      return { ...s, draft: toDraft(s.server) };
  }
}

export function initialEditorState(server: UserAccessResponse): EditorState {
  return { server, draft: toDraft(server) };
}

/** The message a failed save shows. 409 "access changed" is the one case
 *  with its own action (Reload); everything else shows the server's text. */
export function saveErrorKind(status: number, message: string): "stale" | "server" {
  return status === 409 && message.includes("access changed since you opened it") ? "stale" : "server";
}

/**
 * How a row's role select behaves for the signed-in user. A non-superadmin
 * cannot grant or remove the admin role: the admin option is disabled, and a
 * row that already holds admin cannot be changed at all. Mirrors the
 * backend's ErrAdminOnlySuperadmin on PUT /users/{id}/access.
 */
export function roleSelectState(own: EditableRole | undefined, canChangeAdmin: boolean): {
  selectDisabled: boolean;
  adminOptionDisabled: boolean;
} {
  if (canChangeAdmin) return { selectDisabled: false, adminOptionDisabled: false };
  return { selectDisabled: own === "admin", adminOptionDisabled: true };
}
