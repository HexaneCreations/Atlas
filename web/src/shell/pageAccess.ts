import type { CurrentUser, Page } from "../api/types";

/**
 * Overview is always reachable, whatever GET /auth/me reports.
 *
 * It renders only data from endpoints that are themselves page-gated for
 * their own page, so a user with no Overview grant simply sees a sparser
 * page — never privileged data. This is the one deliberate, documented
 * exception to page-access nav gating (see internal/api/v1/auth.go's
 * currentUserResponse doc and the Priority 3 report).
 */
const ALWAYS_REACHABLE = new Set<Page>(["overview"]);

/**
 * Whether the signed-in user may reach `page` at all — the basis for both
 * hiding a nav item (shell/Sidebar, shell/CommandPalette) and redirecting a
 * direct-URL visit (App.tsx's RequirePage). Presence in `page_access`
 * (fleet-wide or for any node) is enough; this is a navigation hint, not
 * the data boundary, which every page endpoint enforces on its own.
 *
 * The Users page is intentionally NOT decided here — it stays gated on
 * `can_manage_users` exactly as before, since a freshly created admin holds
 * user.manage before any page grant.
 */
export function canReachPage(user: CurrentUser | null, page: Page): boolean {
  if (!user) return false;
  if (ALWAYS_REACHABLE.has(page)) return true;
  return user.page_access.some((entry) => entry.page === page);
}
