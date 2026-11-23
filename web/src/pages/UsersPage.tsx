import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";
import { createPortal } from "react-dom";
import {
  Ban,
  Check,
  Copy,
  FileKey,
  History,
  KeyRound,
  LogOut,
  MoreHorizontal,
  PlayCircle,
  ShieldPlus,
  UserPlus,
  X,
  type LucideIcon,
} from "lucide-react";
import { ApiError } from "../api/client";
import { emptyArray } from "../api/empty";
import {
  useCreateUser,
  useDisableUser,
  useEnableUser,
  useForceLogout,
  useNodes,
  usePageAccessByUser,
  useResetPassword,
  useRevokePageAccess,
  useRevokeRole,
  useRevokeRoleAccess,
  useRoleAccessDefinitions,
  useUserAudit,
  useUserPageAccess,
  useUsers,
} from "../api/queries";
import type {
  AuditEntry,
  Grant,
  ListUserPageAccessResponse,
  Page,
  PageAccessAssignment,
  UserAccount,
} from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { Badge } from "../components/Badge";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { EmptyAction, EmptyState } from "../components/EmptyState";
import { Modal, ModalActions } from "../components/Modal";
import { PageHeader } from "../components/PageHeader";
import { QueryState } from "../components/QueryState";
import { useToast } from "../components/Toast";
import { SearchInput, Toolbar } from "../components/Toolbar";
import { NAV_PAGES } from "../shell/pages";
import { emptyArt, errorArt } from "../lib/assets";
import { formatDateTime } from "../format";
import {
  ADMIN_ONLY_SUPERADMIN_MESSAGE,
  PROTECTED_SUPERADMIN_MESSAGE,
  adminTargetLocked,
  effectivePages,
  holdsSuperadmin,
  revokeGuard,
  roleRevokeGuard,
} from "./usersAccess";
import { EMPTY_ACCESS, diff, editorReducer, initialEditorState, problems, toBody } from "./accessEditor";
import { AccessGrid, AccessTab, ChangeList } from "./users/AccessEditor";
import { RoleGrantsTable } from "./users/RoleGrants";

/** Every gateable page — see internal/core/pageauthz.KnownPages. */
const PAGES: Page[] = [
  "overview",
  "nodes",
  "containers",
  "processes",
  "services",
  "cron",
  "ports",
  "disks",
  "users",
];

/** page key → route path, to borrow the label and icon NAV_PAGES already
 *  defines (web/src/shell/pages.ts, the same source pageauthz.Page mirrors). */
const PAGE_ROUTE: Record<Page, string> = {
  overview: "/",
  nodes: "/nodes",
  containers: "/containers",
  processes: "/processes",
  services: "/services",
  cron: "/cron",
  ports: "/ports",
  disks: "/disks",
  users: "/users",
};

const PAGE_META: Record<Page, { label: string; icon: LucideIcon }> = Object.fromEntries(
  PAGES.map((p) => {
    const nav = NAV_PAGES.find((n) => n.to === PAGE_ROUTE[p]);
    return [p, { label: nav?.label ?? p, icon: nav?.icon ?? FileKey }];
  }),
) as Record<Page, { label: string; icon: LucideIcon }>;

const ACTION_LABEL: Record<AuditEntry["action"], string> = {
  create_user: "Account created",
  grant_role: "Role granted",
  revoke_role: "Role revoked",
  disable_user: "Account disabled",
  enable_user: "Account enabled",
  reset_password: "Password reset",
  force_logout: "Forced logout",
  create_role_access: "Bundle created",
  add_page_to_role_access: "Page added to bundle",
  remove_page_from_role_access: "Page removed from bundle",
  assign_role_access: "Bundle assigned",
  revoke_role_access: "Bundle revoked",
  grant_page_access: "Page access granted",
  revoke_page_access: "Page access revoked",
};

function messageFor(error: unknown): string {
  return error instanceof ApiError ? error.message : "Could not reach Atlas.";
}

function isFleetWideAdmin(g: Grant): boolean {
  return g.role === "admin" && g.fleet_wide;
}

/**
 * The display scope for a grant or page-access assignment — "fleet-wide", or
 * the node's stable id (the primary label Nodes/NodeSwitcher show) with the
 * hostname in parentheses as secondary context when it is known and differs.
 */
function scopeLabel(a: { fleet_wide: boolean; node_id?: string }, nodeNames: Map<string, string>): string {
  if (a.fleet_wide) return "fleet-wide";
  if (!a.node_id) return "unknown node";
  const hostname = nodeNames.get(a.node_id);
  return hostname && hostname !== a.node_id ? `${a.node_id} (${hostname})` : a.node_id;
}

// ------------------------------------------------------------ Avatar ----

const AVATAR_FALLBACK = "bg-primary/15 text-primary";
const AVATAR_COLORS = [
  AVATAR_FALLBACK,
  "bg-info/15 text-info",
  "bg-success/15 text-success",
  "bg-warning/15 text-warning",
  "bg-danger/15 text-danger",
];

function initials(name: string): string {
  const parts = name.split(/[^a-zA-Z0-9]+/).filter(Boolean);
  const [first, second] = parts;
  if (first && second) return (first.charAt(0) + second.charAt(0)).toUpperCase();
  return (first ?? name).slice(0, 2).toUpperCase();
}

function avatarColor(name: string): string {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) | 0;
  return AVATAR_COLORS[Math.abs(h) % AVATAR_COLORS.length] ?? AVATAR_FALLBACK;
}

function Avatar({ name, size = "sm" }: { name: string; size?: "sm" | "md" }) {
  const cls = size === "md" ? "h-10 w-10 text-sm" : "h-7 w-7 text-[11px]";
  return (
    <span
      aria-hidden="true"
      className={`inline-flex shrink-0 items-center justify-center rounded-full font-semibold ${cls} ${avatarColor(name)}`}
    >
      {initials(name)}
    </span>
  );
}

/**
 * Manage users, their role grants, and their page access.
 *
 * The one page in Atlas with real, mutating actions — see api/client.ts's
 * doc on the /users exception and Button's own "Atlas has no destructive
 * actions" comment. Every mutation here is gated server-side by user.manage
 * regardless of what this page shows; the `can_manage_users` check below is a
 * display convenience, not the boundary.
 */
export function UsersPage() {
  const { user: me } = useAuth();
  const usersQuery = useUsers();
  const [search, setSearch] = useState("");
  const [modal, setModal] = useState<ModalState | null>(null);
  const [selectedUserID, setSelectedUserID] = useState<string | null>(null);
  const [panelTab, setPanelTab] = useState<PanelTab>("overview");

  const createUser = useCreateUser();
  const revokeRole = useRevokeRole();
  const disableUser = useDisableUser();
  const enableUser = useEnableUser();
  const resetPassword = useResetPassword();
  const forceLogout = useForceLogout();
  const { push } = useToast();

  const all = usersQuery.data?.users ?? emptyArray<UserAccount>();
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return all;
    return all.filter((u) => u.username.toLowerCase().includes(q));
  }, [all, search]);

  // Grants carry a node_id, which is the primary label everywhere now
  // (see lib/nodeIdentity). This lookup adds the node's hostname as
  // parenthetical context in scopeLabel; it shares its cache with the grant
  // modals' own useNodes() calls.
  const nodesQuery = useNodes();
  const nodeNames = useMemo(() => {
    const map = new Map<string, string>();
    for (const n of nodesQuery.data?.nodes ?? []) map.set(n.node_id, n.hostname);
    return map;
  }, [nodesQuery.data]);

  // Per-user page access, for the list column's badge. Fanned out over every
  // known user (not just the filtered view) so the count is stable across
  // searches and the panel that opens next reads a warm cache.
  const pageAccess = usePageAccessByUser(useMemo(() => all.map((u) => u.id), [all]));

  // Whether the Role-Access Bundles section shows at all: only when at least
  // one bundle is defined system-wide.
  const bundleDefs = useRoleAccessDefinitions();
  const hasBundles = (bundleDefs.data?.total ?? 0) > 0;

  // bundle name → the pages it carries, so the list badge can count a user's
  // *effective* page access (direct grants ∪ pages via assigned bundles), not
  // just their direct grants.
  const bundlePages = useMemo(() => {
    const map = new Map<string, Page[]>();
    for (const d of bundleDefs.data?.role_access ?? []) map.set(d.name, d.pages);
    return map;
  }, [bundleDefs.data]);

  // How many *other* enabled users hold a fleet-wide admin grant — the same
  // condition the backend's last-admin guard checks, computed here only to
  // disable the action ahead of a request that would fail anyway.
  const enabledFleetAdmins = useMemo(
    () => new Set(all.filter((u) => !u.disabled && u.grants?.some(isFleetWideAdmin)).map((u) => u.id)),
    [all],
  );
  const isRevokeBlocked = (g: Grant) => isFleetWideAdmin(g) && enabledFleetAdmins.size <= 1;

  // The superadmin account is off-limits to every per-user action for anyone
  // who is not themselves a superadmin — the client-side mirror of the
  // backend's guardSuperadminTarget 403, so an admin sees a disabled control
  // instead of a working-looking one that then fails.
  const iAmSuperadmin = me?.is_superadmin ?? false;
  const isProtectedTarget = (u: UserAccount) => holdsSuperadmin(u.grants) && !iAmSuperadmin;

  const selectedUser = all.find((u) => u.id === selectedUserID) ?? null;
  // A user removed from the list (never happens today — no delete) should not
  // leave a panel open against nothing.
  useEffect(() => {
    if (selectedUserID && !selectedUser) setSelectedUserID(null);
  }, [selectedUserID, selectedUser]);

  // The Access tab reports whether it holds an unsaved draft; leaving it
  // (another user, another tab, closing the panel) asks first.
  const accessDirty = useRef(false);
  const onAccessDirty = useCallback((dirty: boolean) => { accessDirty.current = dirty; }, []);
  function confirmLeave(): boolean {
    if (!accessDirty.current) return true;
    const ok = window.confirm("You have unsaved access changes. Discard them?");
    if (ok) accessDirty.current = false;
    return ok;
  }

  // A row-body click toggles that user's panel; the "..." menu's "View
  // activity" always opens it on the Activity Log tab, "Edit access" on the
  // Access tab.
  function togglePanel(id: string) {
    if (!confirmLeave()) return;
    setPanelTab("overview");
    setSelectedUserID((cur) => (cur === id ? null : id));
  }
  function openTab(id: string, tab: PanelTab) {
    if (!confirmLeave()) return;
    setPanelTab(tab);
    setSelectedUserID(id);
  }

  if (!me?.can_manage_users) {
    return (
      <>
        <PageHeader />
        <Card level="floating" className="p-0">
          <EmptyState
            kind="unavailable"
            art={errorArt.offline}
            title="You do not have permission to manage users"
            description="This page is limited to accounts holding the admin role, fleet-wide."
            compact
          />
        </Card>
      </>
    );
  }

  return (
    <>
      <PageHeader
        subtitle="Manage users, roles, and page access across Atlas."
        action={
          <Button variant="primary" icon={UserPlus} onClick={() => { setModal({ kind: "create" }); }}>
            Create user
          </Button>
        }
      />

      <Card level="floating" className="p-0">
        <div className="border-b border-border p-4">
          <Toolbar>
            <SearchInput value={search} onChange={setSearch} placeholder="Search by username…" />
          </Toolbar>
          <p className="mt-2 text-xs text-text-muted">
            {filtered.length} of {all.length} user{all.length === 1 ? "" : "s"}
          </p>
        </div>

        <QueryState
          isPending={usersQuery.isPending}
          error={usersQuery.error}
          isEmpty={filtered.length === 0}
          onRetry={() => void usersQuery.refetch()}
          rows={4}
          empty={
            all.length === 0
              ? {
                  art: emptyArt.data,
                  title: "No users yet",
                  description: "Create the first account to get started.",
                }
              : {
                  kind: "filtered",
                  art: emptyArt.search,
                  title: "No users match this search",
                  action: <EmptyAction onClick={() => { setSearch(""); }}>Clear search</EmptyAction>,
                }
          }
        />

        {filtered.length > 0 ? (
          <div className="scroll-thin max-h-[34rem] overflow-auto">
            <table aria-label="Users" className="w-full border-collapse text-sm">
              <thead className="sticky top-0 z-10 bg-surface-hover">
                <tr>
                  <Th>Username</Th>
                  <Th>Roles</Th>
                  <Th>Page Access</Th>
                  <Th>Status</Th>
                  <Th>Created</Th>
                  <Th align="right">Actions</Th>
                </tr>
              </thead>
              <tbody>
                {filtered.map((u) => (
                  <UserRow
                    key={u.id}
                    user={u}
                    isSelf={u.id === me.user_id}
                    selected={u.id === selectedUserID}
                    nodeNames={nodeNames}
                    pageAccess={pageAccess.byUser.get(u.id)}
                    pageAccessLoading={pageAccess.isPending || bundleDefs.isPending}
                    bundlePages={bundlePages}
                    isOnlyFleetAdmin={enabledFleetAdmins.has(u.id) && enabledFleetAdmins.size <= 1}
                    protectedTarget={isProtectedTarget(u)}
                    adminLocked={adminTargetLocked({ actorIsSuperadmin: iAmSuperadmin, isSelf: u.id === me.user_id, targetGrants: u.grants })}
                    onSelect={() => { togglePanel(u.id); }}
                    onEditAccess={() => { openTab(u.id, "access"); }}
                    onDisable={() => { setModal({ kind: "disable", target: u }); }}
                    onEnable={() => {
                      enableUser.mutate(u.id, {
                        onSuccess: () => { push({ tone: "success", title: `${u.username} enabled` }); },
                        onError: (e) => { push({ tone: "danger", title: "Could not enable user", description: messageFor(e) }); },
                      });
                    }}
                    onResetPassword={() => { setModal({ kind: "resetConfirm", target: u }); }}
                    onForceLogout={() => { setModal({ kind: "forceLogout", target: u }); }}
                    onViewActivity={() => { openTab(u.id, "activity"); }}
                  />
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </Card>

      {selectedUser ? (
        <UserDetailPanel
          user={selectedUser}
          isSelf={selectedUser.id === me.user_id}
          protectedTarget={isProtectedTarget(selectedUser)}
          actorIsSuperadmin={iAmSuperadmin}
          nodeNames={nodeNames}
          showBundles={hasBundles}
          tab={panelTab}
          onTab={(t) => { if (t === panelTab || confirmLeave()) setPanelTab(t); }}
          onClose={() => { if (confirmLeave()) setSelectedUserID(null); }}
          isRevokeBlocked={isRevokeBlocked}
          onAccessDirty={onAccessDirty}
          onRevokeRole={(grant) => { setModal({ kind: "revokeConfirm", target: selectedUser, grant }); }}
          onRevokePage={(assignment) => { setModal({ kind: "revokePage", target: selectedUser, assignment }); }}
          onRevokeBundle={(assignment) => { setModal({ kind: "revokeBundle", target: selectedUser, assignment }); }}
        />
      ) : null}

      {modal?.kind === "create" ? (
        <CreateUserModal
          actorIsSuperadmin={iAmSuperadmin}
          createUser={createUser}
          onClose={() => { setModal(null); }}
          onCreated={(user, password) => { setModal({ kind: "created", user, password }); }}
        />
      ) : null}
      {modal?.kind === "created" ? (
        <CreatedUserModal
          user={modal.user}
          password={modal.password}
          onClose={() => { setModal(null); }}
          onEditAccess={() => {
            setModal(null);
            openTab(modal.user.id, "access");
          }}
        />
      ) : null}
      {modal?.kind === "revokeConfirm" ? (
        <RevokeConfirmModal
          target={modal.target}
          grant={modal.grant}
          nodeNames={nodeNames}
          revokeRole={revokeRole}
          onClose={() => { setModal(null); }}
        />
      ) : null}
      {modal?.kind === "disable" ? (
        <DisableConfirmModal target={modal.target} disableUser={disableUser} onClose={() => { setModal(null); }} />
      ) : null}
      {modal?.kind === "resetConfirm" ? (
        <ResetPasswordConfirmModal
          target={modal.target}
          resetPassword={resetPassword}
          onClose={() => { setModal(null); }}
          onReset={(password) => { setModal({ kind: "resetReveal", target: modal.target, password }); }}
        />
      ) : null}
      {modal?.kind === "resetReveal" ? (
        <PasswordRevealModal
          title="New password"
          description={`Share this new password with ${modal.target.username} securely.`}
          password={modal.password}
          onClose={() => { setModal(null); }}
        />
      ) : null}
      {modal?.kind === "forceLogout" ? (
        <ForceLogoutConfirmModal target={modal.target} forceLogout={forceLogout} onClose={() => { setModal(null); }} />
      ) : null}
      {modal?.kind === "revokePage" ? (
        <RevokePageAccessConfirmModal
          target={modal.target}
          assignment={modal.assignment}
          nodeNames={nodeNames}
          onClose={() => { setModal(null); }}
        />
      ) : null}
      {modal?.kind === "revokeBundle" ? (
        <RevokeBundleConfirmModal
          target={modal.target}
          assignment={modal.assignment}
          nodeNames={nodeNames}
          onClose={() => { setModal(null); }}
        />
      ) : null}
    </>
  );
}

type PanelTab = "overview" | "access" | "activity";

type ModalState =
  | { kind: "create" }
  | { kind: "created"; user: UserAccount; password: string }
  | { kind: "revokeConfirm"; target: UserAccount; grant: Grant }
  | { kind: "disable"; target: UserAccount }
  | { kind: "resetConfirm"; target: UserAccount }
  | { kind: "resetReveal"; target: UserAccount; password: string }
  | { kind: "forceLogout"; target: UserAccount }
  | { kind: "revokePage"; target: UserAccount; assignment: PageAccessAssignment }
  | { kind: "revokeBundle"; target: UserAccount; assignment: PageAccessAssignment };

function Th({ children, align = "left" }: { children: React.ReactNode; align?: "left" | "right" }) {
  return (
    <th
      className={`px-3 py-2.5 text-[11px] font-semibold tracking-wider text-text-muted uppercase ${
        align === "right" ? "text-right" : "text-left"
      }`}
    >
      {children}
    </th>
  );
}

// -------------------------------------------------------- users table ----

function PageAccessBadge({
  pageAccess,
  bundlePages,
  loading,
}: {
  pageAccess: ListUserPageAccessResponse | undefined;
  bundlePages: ReadonlyMap<string, readonly Page[]>;
  loading: boolean;
}) {
  if (!pageAccess) {
    return <span className="text-xs text-text-subtle">{loading ? "…" : "—"}</span>;
  }
  // Effective access, not just direct grants: a user whose only page access
  // comes from an assigned bundle still has that access.
  const count = effectivePages(pageAccess, bundlePages).size;
  // "No pages" is a warning, never a neutral empty cell: a user with zero
  // page access is a state an admin must notice, not glance past.
  if (count === 0) {
    return <Badge tone="warning">No pages</Badge>;
  }
  return (
    <span
      className="rounded bg-primary/10 px-1.5 py-0.5 text-[11px] font-medium text-primary"
      title="Pages reachable through direct grants and assigned bundles"
    >
      {count} page{count === 1 ? "" : "s"}
    </span>
  );
}

function UserRow({
  user: u,
  isSelf,
  selected,
  nodeNames,
  pageAccess,
  pageAccessLoading,
  bundlePages,
  isOnlyFleetAdmin,
  protectedTarget,
  adminLocked,
  onSelect,
  onEditAccess,
  onDisable,
  onEnable,
  onResetPassword,
  onForceLogout,
  onViewActivity,
}: {
  user: UserAccount;
  isSelf: boolean;
  selected: boolean;
  nodeNames: Map<string, string>;
  pageAccess: ListUserPageAccessResponse | undefined;
  pageAccessLoading: boolean;
  bundlePages: ReadonlyMap<string, readonly Page[]>;
  isOnlyFleetAdmin: boolean;
  protectedTarget: boolean;
  adminLocked: boolean;
  onSelect: () => void;
  onEditAccess: () => void;
  onDisable: () => void;
  onEnable: () => void;
  onResetPassword: () => void;
  onForceLogout: () => void;
  onViewActivity: () => void;
}) {
  const grants = u.grants ?? [];

  // Only a superadmin may disable, enable or reset the password of a user
  // holding admin (the server's ErrAdminOnlySuperadmin).
  const adminLock = adminLocked ? { disabled: true, disabledReason: ADMIN_ONLY_SUPERADMIN_MESSAGE } : {};
  const rawItems: RowMenuItem[] = [
    { label: "Edit access", icon: ShieldPlus, onClick: onEditAccess },
    u.disabled
      ? { label: "Enable user", icon: PlayCircle, onClick: onEnable, ...adminLock }
      : {
          label: "Disable user",
          icon: Ban,
          onClick: onDisable,
          tone: "danger",
          disabled: isSelf || isOnlyFleetAdmin,
          disabledReason: isSelf
            ? "You cannot disable your own account"
            : isOnlyFleetAdmin
              ? "Can't disable — this is the last user with admin access"
              : undefined,
          ...adminLock,
        },
    { label: "Reset password", icon: KeyRound, onClick: onResetPassword, ...adminLock },
    {
      label: "Force logout",
      icon: LogOut,
      onClick: onForceLogout,
      tone: "danger",
      disabled: isSelf,
      disabledReason: isSelf ? "You cannot force-logout your own account" : undefined,
    },
    { label: "View activity", icon: History, onClick: onViewActivity },
  ];

  // The superadmin account: every per-user action is refused server-side
  // (guardSuperadminTarget) for a non-superadmin. Disable them all here so
  // the menu never offers a control that would 403.
  const items: RowMenuItem[] = protectedTarget
    ? rawItems.map((it) =>
        it.onClick === onEditAccess ? it : { ...it, disabled: true, disabledReason: PROTECTED_SUPERADMIN_MESSAGE },
      )
    : rawItems;

  return (
    <tr
      onClick={onSelect}
      aria-expanded={selected}
      className={`cursor-pointer border-b border-border/60 last:border-0 ${
        selected ? "bg-primary/5" : "hover:bg-surface-hover"
      }`}
    >
      <td className={`px-3 py-2.5 ${selected ? "border-l-2 border-l-primary" : "border-l-2 border-l-transparent"}`}>
        <div className="flex items-center gap-2.5">
          <Avatar name={u.username} />
          <span className="min-w-0">
            <span className="block font-medium text-text">
              {u.username}
              {isSelf ? <span className="ml-1.5 text-xs text-text-muted">(you)</span> : null}
            </span>
            {u.email ? <span className="block truncate text-xs text-text-subtle">{u.email}</span> : null}
          </span>
        </div>
      </td>
      <td className="px-3 py-2.5">
        {grants.length === 0 ? (
          <span className="text-xs text-text-subtle">no roles</span>
        ) : (
          <div className="flex flex-wrap gap-1.5">
            {grants.map((g) => (
              <span
                key={g.id}
                className="rounded bg-primary/10 px-1.5 py-0.5 text-[11px] font-medium text-primary"
                title={`Granted ${formatDateTime(g.granted_at)}${g.granted_by ? ` by ${g.granted_by}` : ""}`}
              >
                {g.role} · {scopeLabel(g, nodeNames)}
              </span>
            ))}
          </div>
        )}
      </td>
      <td className="px-3 py-2.5">
        <PageAccessBadge pageAccess={pageAccess} bundlePages={bundlePages} loading={pageAccessLoading} />
      </td>
      <td className="px-3 py-2.5">
        <Badge tone={u.disabled ? "danger" : "success"}>{u.disabled ? "Disabled" : "Active"}</Badge>
      </td>
      <td className="px-3 py-2.5 text-xs text-text-muted">{formatDateTime(u.created_at)}</td>
      <td className="px-3 py-2.5 text-right" onClick={(e) => { e.stopPropagation(); }}>
        <RowMenu items={items} />
      </td>
    </tr>
  );
}

interface RowMenuItem {
  label: string;
  icon: LucideIcon;
  onClick: () => void;
  tone?: "danger" | undefined;
  disabled?: boolean | undefined;
  disabledReason?: string | undefined;
}

/** Estimated menu geometry, for flipping above the trigger when there isn't
 *  room below. w-56 below matches MENU_WIDTH_PX. */
const MENU_WIDTH_PX = 224;
const MENU_ITEM_HEIGHT_PX = 36;

/**
 * The row action menu. Rendered through a portal into document.body rather
 * than positioned relative to its trigger in place: the table's own
 * scroll-thin max-h-[34rem] overflow-auto container clips anything
 * absolutely positioned inside it. Positioning is computed in viewport
 * (fixed) coordinates from the trigger's own bounding rect instead.
 */
function RowMenu({ items }: { items: RowMenuItem[] }) {
  const [open, setOpen] = useState(false);
  const [coords, setCoords] = useState<{ top: number; left: number } | null>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);

  function openMenu() {
    const rect = buttonRef.current?.getBoundingClientRect();
    if (!rect) return;
    const menuHeight = items.length * MENU_ITEM_HEIGHT_PX + 8;
    const opensUp = rect.bottom + menuHeight > window.innerHeight;
    setCoords({
      top: opensUp ? rect.top - menuHeight - 4 : rect.bottom + 4,
      left: Math.max(8, rect.right - MENU_WIDTH_PX),
    });
    setOpen(true);
  }

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: MouseEvent) => {
      const target = e.target as Node;
      if (buttonRef.current?.contains(target) || menuRef.current?.contains(target)) return;
      setOpen(false);
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    const onScrollOrResize = () => { setOpen(false); };
    document.addEventListener("mousedown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    window.addEventListener("scroll", onScrollOrResize, true);
    window.addEventListener("resize", onScrollOrResize);
    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
      window.removeEventListener("scroll", onScrollOrResize, true);
      window.removeEventListener("resize", onScrollOrResize);
    };
  }, [open]);

  return (
    <>
      <button
        ref={buttonRef}
        type="button"
        onClick={() => { if (open) setOpen(false); else openMenu(); }}
        aria-label="Actions"
        aria-haspopup="menu"
        aria-expanded={open}
        className="rounded-lg p-1.5 text-text-muted hover:bg-surface-hover hover:text-text"
      >
        <MoreHorizontal size={16} />
      </button>
      {open && coords
        ? createPortal(
            <div
              ref={menuRef}
              role="menu"
              style={{ position: "fixed", top: coords.top, left: coords.left, width: MENU_WIDTH_PX }}
              className="z-50 rounded-lg border border-border bg-surface py-1 shadow-lg"
            >
              {items.map((it) => (
                <button
                  key={it.label}
                  type="button"
                  role="menuitem"
                  disabled={it.disabled}
                  title={it.disabledReason}
                  onClick={() => { setOpen(false); it.onClick(); }}
                  className={`flex w-full items-center gap-2 px-3 py-2 text-left text-sm disabled:cursor-not-allowed disabled:opacity-40 ${
                    it.tone === "danger" ? "text-danger hover:bg-danger/10" : "text-text hover:bg-surface-hover"
                  }`}
                >
                  <it.icon size={14} />
                  {it.label}
                </button>
              ))}
            </div>,
            document.body,
          )
        : null}
    </>
  );
}

// ------------------------------------------------------ detail panel ----

function UserDetailPanel({
  user,
  isSelf,
  protectedTarget,
  actorIsSuperadmin,
  nodeNames,
  showBundles,
  tab,
  onTab,
  onClose,
  isRevokeBlocked,
  onAccessDirty,
  onRevokeRole,
  onRevokePage,
  onRevokeBundle,
}: {
  user: UserAccount;
  isSelf: boolean;
  protectedTarget: boolean;
  actorIsSuperadmin: boolean;
  nodeNames: Map<string, string>;
  showBundles: boolean;
  tab: PanelTab;
  onTab: (t: PanelTab) => void;
  onClose: () => void;
  isRevokeBlocked: (g: Grant) => boolean;
  onAccessDirty: (dirty: boolean) => void;
  onRevokeRole: (g: Grant) => void;
  onRevokePage: (a: PageAccessAssignment) => void;
  onRevokeBundle: (a: PageAccessAssignment) => void;
}) {
  const pageAccess = useUserPageAccess(user.id);
  const audit = useUserAudit(user.id);
  const grants = user.grants ?? [];
  const entries = audit.data?.entries ?? emptyArray<AuditEntry>();
  const roleAccess = pageAccess.data?.role_access ?? emptyArray<PageAccessAssignment>();
  const directPages = pageAccess.data?.pages ?? emptyArray<PageAccessAssignment>();

  return (
    <Card level="floating" className="mt-4 p-0">
      <header className="flex flex-wrap items-center gap-3 border-b border-border p-4">
        <Avatar name={user.username} size="md" />
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span className="font-semibold text-text">
              {user.username}
              {isSelf ? <span className="ml-1.5 text-xs font-normal text-text-muted">(you)</span> : null}
            </span>
            <Badge tone={user.disabled ? "danger" : "success"}>{user.disabled ? "Disabled" : "Active"}</Badge>
          </div>
          <p className="text-xs text-text-muted">
            {user.email ? <span>{user.email} · </span> : null}
            Created {formatDateTime(user.created_at)}
          </p>
        </div>
        <Button variant="secondary" size="sm" icon={X} className="ml-auto" onClick={onClose}>
          Close
        </Button>
      </header>

      <div className="flex gap-1 border-b border-border px-4">
        <TabButton active={tab === "overview"} onClick={() => { onTab("overview"); }}>
          Overview
        </TabButton>
        <TabButton active={tab === "access"} onClick={() => { onTab("access"); }}>
          Access
        </TabButton>
        <TabButton active={tab === "activity"} onClick={() => { onTab("activity"); }}>
          Activity Log
        </TabButton>
      </div>

      {tab === "overview" ? (
        <div className="grid gap-4 p-4 lg:grid-cols-2">
          {isSelf ? (
            <p className="rounded-lg border border-border bg-surface-hover/40 px-3 py-2 text-xs text-text-muted lg:col-span-2">
              You&rsquo;re viewing your own account. Revoke actions are disabled here so you can&rsquo;t
              accidentally remove your own access — ask another admin if a change is needed.
            </p>
          ) : protectedTarget ? (
            <p className="rounded-lg border border-border bg-surface-hover/40 px-3 py-2 text-xs text-text-muted lg:col-span-2">
              This is a superadmin account. {PROTECTED_SUPERADMIN_MESSAGE} — role, page-access, bundle and
              account actions are disabled here.
            </p>
          ) : null}

          {/* 1. Roles */}
          <Section
            n={1}
            title="Roles"
            hint="Roles define the level of access in Atlas. Change them on the Access tab."
            actions={
              <Button variant="secondary" size="sm" onClick={() => { onTab("access"); }}>
                Edit access
              </Button>
            }
          >
            {grants.length === 0 ? (
              <EmptyRow>No roles granted to this user.</EmptyRow>
            ) : (
              <RoleGrantsTable
                grants={grants}
                scopeLabel={(g) => scopeLabel(g, nodeNames)}
                guardFor={(g) =>
                  roleRevokeGuard({ isSelf, lastAdmin: isRevokeBlocked(g), protectedSuperadmin: protectedTarget, actorIsSuperadmin, grant: g })
                }
                onRevoke={onRevokeRole}
              />
            )}
          </Section>

          {/* 2. Page Access */}
          <Section
            n={2}
            title="Page Access"
            hint="Pages this user can access, and for which scope. Grant pages on the Access tab."
          >
            <SectionState
              isPending={pageAccess.isPending}
              error={pageAccess.error}
              onRetry={() => void pageAccess.refetch()}
            />
            {!pageAccess.isPending && !pageAccess.error ? (
              directPages.length === 0 ? (
                <EmptyRow>No direct page access granted.</EmptyRow>
              ) : (
                <AccessTable head={["Page", "Scope", "Granted by", "Granted at", ""]}>
                  {directPages.map((a) => {
                    const meta = a.page ? PAGE_META[a.page] : undefined;
                    const Icon = meta?.icon ?? FileKey;
                    const guard = revokeGuard({ isSelf, protectedSuperadmin: protectedTarget });
                    return (
                      <tr key={a.id} className="border-t border-border/60">
                        <td className="py-2 pr-3">
                          <span className="flex items-center gap-1.5 font-medium text-text">
                            <Icon size={13} className="text-text-muted" />
                            {meta?.label ?? a.page}
                          </span>
                        </td>
                        <td className="py-2 pr-3 text-xs text-text-muted">{scopeLabel(a, nodeNames)}</td>
                        <td className="py-2 pr-3 text-xs text-text-muted">{a.granted_by ?? "—"}</td>
                        <td className="py-2 pr-3 text-xs text-text-muted">{formatDateTime(a.granted_at)}</td>
                        <td className="py-2 text-right">
                          <button
                            type="button"
                            aria-label={`Revoke ${meta?.label ?? a.page ?? "page"} access`}
                            disabled={guard.disabled}
                            title={guard.reason ?? "Revoke"}
                            onClick={() => { onRevokePage(a); }}
                            className="rounded p-1 text-text-muted hover:bg-danger/10 hover:text-danger disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:bg-transparent disabled:hover:text-text-muted"
                          >
                            <X size={14} />
                          </button>
                        </td>
                      </tr>
                    );
                  })}
                </AccessTable>
              )
            ) : null}
          </Section>

          {/* 3. Role-Access Bundles — only when any bundle exists system-wide */}
          {showBundles ? (
            <Section
              n={3}
              title="Role-Access Bundles"
              hint="Bundles are preset collections of page access grants. Their pages show locked on the Access tab."
            >
              {pageAccess.isPending ? (
                <SectionState isPending error={null} onRetry={() => void pageAccess.refetch()} />
              ) : roleAccess.length === 0 ? (
                <EmptyRow>No bundles assigned to this user.</EmptyRow>
              ) : (
                <AccessTable head={["Bundle", "Scope", "Granted by", "Granted at", ""]}>
                  {roleAccess.map((a) => {
                    const guard = revokeGuard({ isSelf, protectedSuperadmin: protectedTarget });
                    return (
                      <tr key={a.id} className="border-t border-border/60">
                        <td className="py-2 pr-3 font-medium text-text">{a.role_access}</td>
                        <td className="py-2 pr-3 text-xs text-text-muted">{scopeLabel(a, nodeNames)}</td>
                        <td className="py-2 pr-3 text-xs text-text-muted">{a.granted_by ?? "—"}</td>
                        <td className="py-2 pr-3 text-xs text-text-muted">{formatDateTime(a.granted_at)}</td>
                        <td className="py-2 text-right">
                          <button
                            type="button"
                            disabled={guard.disabled}
                            title={guard.reason ?? "Revoke"}
                            onClick={() => { onRevokeBundle(a); }}
                            className="rounded px-2 py-1 text-xs font-medium text-danger hover:bg-danger/10 disabled:cursor-not-allowed disabled:opacity-40"
                          >
                            Revoke
                          </button>
                        </td>
                      </tr>
                    );
                  })}
                </AccessTable>
              )}
            </Section>
          ) : null}

          {/* 4. Activity */}
          <Section
            n={4}
            title="Activity"
            hint="Recent access changes for this user."
            actions={
              <Button variant="ghost" size="sm" onClick={() => { onTab("activity"); }}>
                View full audit log
              </Button>
            }
          >
            {entries.length === 0 ? (
              <EmptyRow>No recorded activity yet.</EmptyRow>
            ) : (
              <ul className="flex flex-col">
                {entries.slice(0, 4).map((e) => (
                  <li key={e.id} className="flex items-center justify-between gap-3 border-t border-border/60 py-2 text-sm first:border-0">
                    <span className="text-text">{ACTION_LABEL[e.action]}</span>
                    <span className="shrink-0 text-xs text-text-subtle">
                      {e.actor_username ?? e.actor_user_id} · {formatDateTime(e.created_at)}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </Section>
        </div>
      ) : tab === "access" ? (
        <div className="p-4">
          <AccessTab
            userID={user.id}
            readOnly={isSelf || holdsSuperadmin(grants)}
            canChangeAdmin={actorIsSuperadmin}
            readOnlyReason={
              isSelf
                ? "You cannot change your own access — ask another admin."
                : holdsSuperadmin(grants)
                  ? `This is a superadmin account. ${PROTECTED_SUPERADMIN_MESSAGE}; the editor is read-only here, and the superadmin role itself is managed only by the CLI's transfer-superadmin.`
                  : undefined
            }
            onDirtyChange={onAccessDirty}
          />
        </div>
      ) : (
        <ActivityLog userID={user.id} audit={audit} />
      )}
    </Card>
  );
}

function TabButton({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`-mb-px border-b-2 px-3 py-2.5 text-sm font-medium transition-colors ${
        active ? "border-primary text-text" : "border-transparent text-text-muted hover:text-text"
      }`}
    >
      {children}
    </button>
  );
}

function Section({
  n,
  title,
  hint,
  actions,
  children,
}: {
  n: number;
  title: string;
  hint?: string;
  actions?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <section className="self-start rounded-xl border border-border p-4">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <h3 className="text-sm font-semibold text-text">
            {n}. {title}
          </h3>
          {hint ? <p className="mt-0.5 text-xs text-text-muted">{hint}</p> : null}
        </div>
        {actions ? <div className="flex flex-wrap gap-2">{actions}</div> : null}
      </div>
      <div className="mt-3">{children}</div>
    </section>
  );
}

function AccessTable({ head, children }: { head: string[]; children: React.ReactNode }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full border-collapse text-sm">
        <thead>
          <tr>
            {head.map((h, i) => (
              <th
                key={h || i}
                className={`pb-1 text-[11px] font-semibold tracking-wider text-text-subtle uppercase ${
                  i === head.length - 1 ? "text-right" : "text-left"
                }`}
              >
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>{children}</tbody>
      </table>
    </div>
  );
}

function EmptyRow({ children }: { children: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-dashed border-border px-3 py-4 text-center text-xs text-text-muted">
      {children}
    </div>
  );
}

/** Compact loading / error state for a panel section — a full [QueryState]
 *  with its artwork is too heavy at this size. Returns null once there is
 *  data to render. */
function SectionState({
  isPending,
  error,
  onRetry,
}: {
  isPending: boolean;
  error: Error | null;
  onRetry: () => void;
}) {
  if (isPending) {
    return (
      <div className="flex flex-col gap-2">
        <div className="h-4 animate-pulse rounded bg-surface-hover" />
        <div className="h-4 w-3/4 animate-pulse rounded bg-surface-hover" />
      </div>
    );
  }
  if (error) {
    return (
      <p className="text-xs text-danger">
        Could not load.{" "}
        <button type="button" onClick={onRetry} className="underline hover:no-underline">
          Retry
        </button>
      </p>
    );
  }
  return null;
}

function ActivityLog({
  userID,
  audit,
}: {
  userID: string;
  audit: ReturnType<typeof useUserAudit>;
}) {
  const entries = audit.data?.entries ?? emptyArray<AuditEntry>();
  return (
    <div className="p-4">
      <QueryState
        isPending={audit.isPending}
        error={audit.error}
        isEmpty={entries.length === 0}
        onRetry={() => void audit.refetch()}
        rows={4}
        empty={{ art: emptyArt.data, title: "No recorded activity for this user yet" }}
      />
      {entries.length > 0 ? (
        <ul className="flex flex-col">
          {entries.map((e) => (
            <li key={e.id} className="border-b border-border py-2.5 text-sm last:border-0">
              <div className="flex items-center justify-between gap-2">
                <span className="font-medium text-text">{ACTION_LABEL[e.action]}</span>
                <span className="text-xs text-text-subtle">{formatDateTime(e.created_at)}</span>
              </div>
              <p className="mt-0.5 text-xs text-text-muted">
                by {e.actor_username ?? e.actor_user_id}
                {typeof e.detail?.role === "string" ? ` · ${e.detail.role}` : ""}
                {typeof e.detail?.page === "string" ? ` · ${e.detail.page}` : ""}
                {typeof e.detail?.role_access === "string" ? ` · ${e.detail.role_access}` : ""}
                {e.detail?.fleet_wide === true ? " · fleet-wide" : ""}
                {typeof e.detail?.node_id === "string" ? ` · ${e.detail.node_id}` : ""}
              </p>
            </li>
          ))}
        </ul>
      ) : null}
      <p className="mt-3 text-xs text-text-subtle">
        User id: <span className="font-mono">{userID}</span>
      </p>
    </div>
  );
}

// ------------------------------------------------------------- Modals ----

function ErrorLine({ error }: { error: string | null }) {
  if (!error) return null;
  return <p className="mt-3 text-sm text-danger">{error}</p>;
}

function CreateUserModal({
  actorIsSuperadmin,
  createUser,
  onClose,
  onCreated,
}: {
  actorIsSuperadmin: boolean;
  createUser: ReturnType<typeof useCreateUser>;
  onClose: () => void;
  onCreated: (user: UserAccount, password: string) => void;
}) {
  const [username, setUsername] = useState("");
  const [email, setEmail] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [withAccess, setWithAccess] = useState(false);
  const [editor, dispatch] = useReducer(editorReducer, EMPTY_ACCESS, initialEditorState);
  const nodes = useNodes();
  const changes = diff(editor.server, editor.draft);
  const issues = problems(editor.draft);
  const sendAccess = withAccess && changes.length > 0;

  function submit(e: { preventDefault: () => void }) {
    e.preventDefault();
    setError(null);
    createUser.mutate(
      {
        username: username.trim(),
        ...(email.trim() ? { email: email.trim() } : {}),
        ...(sendAccess ? { access: toBody(editor.draft) } : {}),
      },
      {
        onSuccess: (res) => { onCreated(res.user, res.password); },
        onError: (err) => { setError(messageFor(err)); },
      },
    );
  }

  return (
    <Modal title="Create user" onClose={onClose} width={withAccess ? "lg" : "sm"}>
      <form onSubmit={submit} className="flex flex-col gap-3">
        <label className="flex flex-col gap-1.5 text-sm text-text">
          Username
          <input
            autoFocus
            value={username}
            onChange={(e) => { setUsername(e.target.value); }}
            required
            className="rounded-lg border border-border bg-bg px-3 py-2 text-sm text-text outline-none focus-visible:ring-2 focus-visible:ring-primary"
          />
        </label>
        <label className="flex flex-col gap-1.5 text-sm text-text">
          Email <span className="text-text-muted">(optional)</span>
          <input
            type="email"
            value={email}
            onChange={(e) => { setEmail(e.target.value); }}
            className="rounded-lg border border-border bg-bg px-3 py-2 text-sm text-text outline-none focus-visible:ring-2 focus-visible:ring-primary"
          />
        </label>
        <label className="flex items-center gap-2 text-sm text-text">
          <input type="checkbox" checked={withAccess} onChange={(e) => { setWithAccess(e.target.checked); }} />
          Set access now <span className="text-text-muted">(optional — by default the user has no access)</span>
        </label>
        {withAccess ? (
          <>
            <AccessGrid
              server={editor.server}
              draft={editor.draft}
              nodes={nodes.data?.nodes ?? []}
              readOnly={false}
              canChangeAdmin={actorIsSuperadmin}
              onEdit={(edit) => { dispatch({ type: "edit", edit }); }}
            />
            <ChangeList changes={changes} />
            {issues.length > 0 ? (
              <ul className="text-xs text-danger">
                {issues.map((m) => <li key={m}>{m}</li>)}
              </ul>
            ) : null}
          </>
        ) : null}
        <ErrorLine error={error} />
        <ModalActions>
          <Button type="button" variant="secondary" onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={createUser.isPending || (sendAccess && issues.length > 0)}>
            {createUser.isPending ? "Creating…" : "Create user"}
          </Button>
        </ModalActions>
      </form>
    </Modal>
  );
}

function PasswordBox({ password }: { password: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="mt-3 flex items-center gap-2 rounded-lg border border-border bg-bg px-3 py-2">
      <code className="flex-1 truncate font-mono text-sm text-text">{password}</code>
      <button
        type="button"
        onClick={() => {
          void navigator.clipboard.writeText(password).then(() => {
            setCopied(true);
            window.setTimeout(() => { setCopied(false); }, 2000);
          });
        }}
        aria-label="Copy password"
        className="shrink-0 rounded p-1 text-text-muted hover:bg-surface-hover hover:text-text"
      >
        {copied ? <Check size={14} className="text-success" /> : <Copy size={14} />}
      </button>
    </div>
  );
}

function CreatedUserModal({
  user,
  password,
  onClose,
  onEditAccess,
}: {
  user: UserAccount;
  password: string;
  onClose: () => void;
  onEditAccess: () => void;
}) {
  return (
    <Modal title="User created" onClose={onClose}>
      <p className="text-sm text-text-muted">
        <span className="font-medium text-text">{user.username}</span> has been created successfully.
      </p>
      <PasswordBox password={password} />
      <p className="mt-2 text-xs text-warning">This password will not be shown again.</p>
      <ModalActions>
        <Button variant="secondary" onClick={onClose}>Done</Button>
        <Button variant="primary" onClick={onEditAccess}>Edit access</Button>
      </ModalActions>
    </Modal>
  );
}

function RevokeConfirmModal({
  target,
  grant,
  nodeNames,
  revokeRole,
  onClose,
}: {
  target: UserAccount;
  grant: Grant;
  nodeNames: Map<string, string>;
  revokeRole: ReturnType<typeof useRevokeRole>;
  onClose: () => void;
}) {
  const [error, setError] = useState<string | null>(null);
  const { push } = useToast();

  return (
    <Modal title="Revoke role" onClose={onClose}>
      <p className="text-sm text-text-muted">
        Remove <span className="font-medium text-text">{grant.role}</span> access for{" "}
        <span className="font-medium text-text">{target.username}</span> on{" "}
        <span className="font-medium text-text">{scopeLabel(grant, nodeNames)}</span>?
      </p>
      <ErrorLine error={error} />
      <ModalActions>
        <Button variant="secondary" onClick={onClose}>Cancel</Button>
        <Button
          variant="danger"
          disabled={revokeRole.isPending}
          onClick={() => {
            setError(null);
            revokeRole.mutate(
              { userID: target.id, grantID: grant.id },
              {
                onSuccess: () => { push({ tone: "success", title: "Role revoked" }); onClose(); },
                onError: (err) => { setError(messageFor(err)); },
              },
            );
          }}
        >
          {revokeRole.isPending ? "Revoking…" : "Revoke"}
        </Button>
      </ModalActions>
    </Modal>
  );
}

function DisableConfirmModal({
  target,
  disableUser,
  onClose,
}: {
  target: UserAccount;
  disableUser: ReturnType<typeof useDisableUser>;
  onClose: () => void;
}) {
  const [error, setError] = useState<string | null>(null);
  const { push } = useToast();

  return (
    <Modal title="Disable user" onClose={onClose}>
      <p className="text-sm text-text-muted">
        Disable <span className="font-medium text-text">{target.username}</span>?
      </p>
      <p className="mt-2 rounded-lg border border-danger/30 bg-danger/10 p-2.5 text-xs text-danger">
        This will terminate all active sessions right away.
      </p>
      <ErrorLine error={error} />
      <ModalActions>
        <Button variant="secondary" onClick={onClose}>Cancel</Button>
        <Button
          variant="danger"
          disabled={disableUser.isPending}
          onClick={() => {
            setError(null);
            disableUser.mutate(target.id, {
              onSuccess: () => { push({ tone: "success", title: `${target.username} disabled` }); onClose(); },
              onError: (err) => { setError(messageFor(err)); },
            });
          }}
        >
          {disableUser.isPending ? "Disabling…" : "Disable"}
        </Button>
      </ModalActions>
    </Modal>
  );
}

function ResetPasswordConfirmModal({
  target,
  resetPassword,
  onClose,
  onReset,
}: {
  target: UserAccount;
  resetPassword: ReturnType<typeof useResetPassword>;
  onClose: () => void;
  onReset: (password: string) => void;
}) {
  const [error, setError] = useState<string | null>(null);

  return (
    <Modal title="Reset password" onClose={onClose}>
      <p className="text-sm text-text-muted">
        This will invalidate <span className="font-medium text-text">{target.username}</span>&rsquo;s current password
        and issue a new one.
      </p>
      <ErrorLine error={error} />
      <ModalActions>
        <Button variant="secondary" onClick={onClose}>Cancel</Button>
        <Button
          variant="primary"
          disabled={resetPassword.isPending}
          onClick={() => {
            setError(null);
            resetPassword.mutate(target.id, {
              onSuccess: (res) => { onReset(res.password); },
              onError: (err) => { setError(messageFor(err)); },
            });
          }}
        >
          {resetPassword.isPending ? "Resetting…" : "Reset password"}
        </Button>
      </ModalActions>
    </Modal>
  );
}

function PasswordRevealModal({
  title,
  description,
  password,
  onClose,
}: {
  title: string;
  description: string;
  password: string;
  onClose: () => void;
}) {
  return (
    <Modal title={title} onClose={onClose}>
      <p className="text-sm text-text-muted">{description}</p>
      <PasswordBox password={password} />
      <p className="mt-2 text-xs text-warning">This password will not be shown again.</p>
      <ModalActions>
        <Button variant="primary" onClick={onClose}>Close</Button>
      </ModalActions>
    </Modal>
  );
}

function ForceLogoutConfirmModal({
  target,
  forceLogout,
  onClose,
}: {
  target: UserAccount;
  forceLogout: ReturnType<typeof useForceLogout>;
  onClose: () => void;
}) {
  const [error, setError] = useState<string | null>(null);
  const { push } = useToast();

  return (
    <Modal title="Force logout" onClose={onClose}>
      <p className="text-sm text-text-muted">
        Force <span className="font-medium text-text">{target.username}</span> to log out of all active sessions?
      </p>
      <p className="mt-2 text-xs text-text-subtle">They&rsquo;ll need to log in again, but their access stays the same.</p>
      <ErrorLine error={error} />
      <ModalActions>
        <Button variant="secondary" onClick={onClose}>Cancel</Button>
        <Button
          variant="danger"
          disabled={forceLogout.isPending}
          onClick={() => {
            setError(null);
            forceLogout.mutate(target.id, {
              onSuccess: () => { push({ tone: "success", title: `${target.username} logged out everywhere` }); onClose(); },
              onError: (err) => { setError(messageFor(err)); },
            });
          }}
        >
          {forceLogout.isPending ? "Logging out…" : "Force logout"}
        </Button>
      </ModalActions>
    </Modal>
  );
}

function RevokePageAccessConfirmModal({
  target,
  assignment,
  nodeNames,
  onClose,
}: {
  target: UserAccount;
  assignment: PageAccessAssignment;
  nodeNames: Map<string, string>;
  onClose: () => void;
}) {
  const revoke = useRevokePageAccess();
  const { push } = useToast();
  const [error, setError] = useState<string | null>(null);
  const pageLabel = assignment.page ? PAGE_META[assignment.page].label : "this page";

  return (
    <Modal title="Revoke page access" onClose={onClose}>
      <p className="text-sm text-text-muted">
        Remove <span className="font-medium text-text">{pageLabel}</span> access for{" "}
        <span className="font-medium text-text">{target.username}</span> on{" "}
        <span className="font-medium text-text">{scopeLabel(assignment, nodeNames)}</span>?
      </p>
      <ErrorLine error={error} />
      <ModalActions>
        <Button variant="secondary" onClick={onClose}>Cancel</Button>
        <Button
          variant="danger"
          disabled={revoke.isPending}
          onClick={() => {
            setError(null);
            revoke.mutate(
              { userID: target.id, grantID: assignment.id },
              {
                onSuccess: () => { push({ tone: "success", title: "Page access revoked" }); onClose(); },
                onError: (err) => { setError(messageFor(err)); },
              },
            );
          }}
        >
          {revoke.isPending ? "Revoking…" : "Revoke"}
        </Button>
      </ModalActions>
    </Modal>
  );
}

function RevokeBundleConfirmModal({
  target,
  assignment,
  nodeNames,
  onClose,
}: {
  target: UserAccount;
  assignment: PageAccessAssignment;
  nodeNames: Map<string, string>;
  onClose: () => void;
}) {
  const revoke = useRevokeRoleAccess();
  const { push } = useToast();
  const [error, setError] = useState<string | null>(null);

  return (
    <Modal title="Revoke bundle" onClose={onClose}>
      <p className="text-sm text-text-muted">
        Remove the <span className="font-medium text-text">{assignment.role_access}</span> bundle from{" "}
        <span className="font-medium text-text">{target.username}</span> on{" "}
        <span className="font-medium text-text">{scopeLabel(assignment, nodeNames)}</span>?
      </p>
      <ErrorLine error={error} />
      <ModalActions>
        <Button variant="secondary" onClick={onClose}>Cancel</Button>
        <Button
          variant="danger"
          disabled={revoke.isPending}
          onClick={() => {
            setError(null);
            revoke.mutate(
              { userID: target.id, assignmentID: assignment.id },
              {
                onSuccess: () => { push({ tone: "success", title: "Bundle revoked" }); onClose(); },
                onError: (err) => { setError(messageFor(err)); },
              },
            );
          }}
        >
          {revoke.isPending ? "Revoking…" : "Revoke"}
        </Button>
      </ModalActions>
    </Modal>
  );
}
