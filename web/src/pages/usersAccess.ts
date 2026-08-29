import type { ListUserPageAccessResponse, Page } from "../api/types";

/**
 * Shown on a Revoke control that is disabled because the user detail panel is
 * open on the signed-in user's own account.
 *
 * This is a client-side guard against an accidental self-revoke, not the
 * boundary. The server-side last-admin guard still enforces the real rule,
 * but it only blocks removing the *last* fleet-wide admin — a well-formed
 * self-revoke while other admins remain still succeeds server-side. The
 * incident this exists for: two admins, one accidentally revokes their own
 * grant, nothing stops it.
 */
export const SELF_REVOKE_MESSAGE = "You cannot revoke your own access.";

/** Shown on a role Revoke blocked because it is the last fleet-wide admin
 *  grant in the system. */
export const LAST_ADMIN_REVOKE_MESSAGE = "Can't revoke — this is the last user with admin access";

export interface RevokeGuard {
  disabled: boolean;
  reason?: string;
}

/**
 * Whether a Revoke control in the user detail panel must be disabled, and the
 * reason to surface on hover.
 *
 * `lastAdmin` applies to role grants only; `isSelf` applies to every access
 * axis — role grants, direct page grants, and bundle assignments alike. The
 * last-admin reason takes precedence when both hold.
 */
export function revokeGuard(opts: { isSelf: boolean; lastAdmin?: boolean }): RevokeGuard {
  if (opts.lastAdmin) return { disabled: true, reason: LAST_ADMIN_REVOKE_MESSAGE };
  if (opts.isSelf) return { disabled: true, reason: SELF_REVOKE_MESSAGE };
  return { disabled: false };
}

/**
 * The distinct set of pages a user can actually reach: every direct page
 * grant, unioned with every page carried by a bundle they are assigned.
 *
 * The users list badge counts this, not just direct grants. A user holding a
 * fleet-wide full-access bundle and no direct grants still has full access
 * and must never read as "No pages". A bundle assignment whose definition is
 * unknown (deleted since) contributes nothing.
 */
export function effectivePages(
  access: Pick<ListUserPageAccessResponse, "pages" | "role_access">,
  bundlePages: ReadonlyMap<string, readonly Page[]>,
): Set<Page> {
  const pages = new Set<Page>();
  for (const grant of access.pages) {
    if (grant.page) pages.add(grant.page);
  }
  for (const assignment of access.role_access) {
    for (const page of bundlePages.get(assignment.role_access ?? "") ?? []) {
      pages.add(page);
    }
  }
  return pages;
}
