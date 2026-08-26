import { useMemo, useRef, useState } from "react";
import {
  Ban,
  Check,
  Copy,
  History,
  KeyRound,
  LogOut,
  MoreHorizontal,
  PlayCircle,
  ShieldMinus,
  ShieldPlus,
  UserPlus,
  type LucideIcon,
} from "lucide-react";
import { ApiError } from "../api/client";
import { emptyArray } from "../api/empty";
import {
  useCreateUser,
  useDisableUser,
  useEnableUser,
  useForceLogout,
  useGrantRole,
  useNodes,
  useResetPassword,
  useRevokeRole,
  useUserAudit,
  useUsers,
} from "../api/queries";
import type { AuditEntry, Grant, Role, UserAccount } from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { Badge } from "../components/Badge";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { Drawer, DrawerField } from "../components/Drawer";
import { EmptyAction, EmptyState } from "../components/EmptyState";
import { Modal, ModalActions } from "../components/Modal";
import { PageHeader } from "../components/PageHeader";
import { QueryState } from "../components/QueryState";
import { useToast } from "../components/Toast";
import { SearchInput, Toolbar } from "../components/Toolbar";
import { emptyArt, errorArt } from "../lib/assets";
import { formatTime } from "../format";

/** Every role a grant may name — see internal/core/user.KnownRoles. */
const ROLES: Role[] = ["viewer", "operator", "admin"];

const ACTION_LABEL: Record<AuditEntry["action"], string> = {
  create_user: "Account created",
  grant_role: "Role granted",
  revoke_role: "Role revoked",
  disable_user: "Account disabled",
  enable_user: "Account enabled",
  reset_password: "Password reset",
  force_logout: "Forced logout",
};

function messageFor(error: unknown): string {
  return error instanceof ApiError ? error.message : "Could not reach Atlas.";
}

function isFleetWideAdmin(g: Grant): boolean {
  return g.role === "admin" && g.fleet_wide;
}

function scopeLabel(g: Grant): string {
  return g.fleet_wide ? "fleet-wide" : (g.node_id ?? "unknown node");
}

/**
 * Manage users and their access grants.
 *
 * The one page in Atlas with real, mutating actions — see api/client.ts's
 * doc on the /users exception and Button's own "Atlas has no destructive
 * actions" comment, which this page is the first to need one for. Every
 * mutation here is gated server-side by user.manage regardless of what this
 * page shows; the `can_manage_users` check below is a display convenience,
 * not the boundary — see CurrentUser's own doc.
 */
export function UsersPage() {
  const { user: me } = useAuth();
  const usersQuery = useUsers();
  const [search, setSearch] = useState("");
  const [modal, setModal] = useState<ModalState | null>(null);
  const [activityUserID, setActivityUserID] = useState<string | null>(null);

  const createUser = useCreateUser();
  const grantRole = useGrantRole();
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

  // How many *other* enabled users hold a fleet-wide admin grant — the same
  // condition the backend's last-admin guard checks (see
  // internal/storage/user.otherEnabledFleetWideAdminExists), computed here
  // only to disable the action ahead of a request that would fail anyway.
  // The backend re-checks this itself regardless of what this says.
  const enabledFleetAdmins = useMemo(
    () => new Set(all.filter((u) => !u.disabled && u.grants?.some(isFleetWideAdmin)).map((u) => u.id)),
    [all],
  );

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
                    isOnlyFleetAdmin={enabledFleetAdmins.has(u.id) && enabledFleetAdmins.size <= 1}
                    onGrant={() => { setModal({ kind: "grant", target: u }); }}
                    onRevokePick={() => { setModal({ kind: "revokePick", target: u }); }}
                    onDisable={() => { setModal({ kind: "disable", target: u }); }}
                    onEnable={() => {
                      enableUser.mutate(u.id, {
                        onSuccess: () => { push({ tone: "success", title: `${u.username} enabled` }); },
                        onError: (e) => { push({ tone: "danger", title: "Could not enable user", description: messageFor(e) }); },
                      });
                    }}
                    onResetPassword={() => { setModal({ kind: "resetConfirm", target: u }); }}
                    onForceLogout={() => { setModal({ kind: "forceLogout", target: u }); }}
                    onViewActivity={() => { setActivityUserID(u.id); }}
                  />
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </Card>

      {modal?.kind === "create" ? (
        <CreateUserModal
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
          onGrantNow={() => { setModal({ kind: "grant", target: modal.user }); }}
        />
      ) : null}
      {modal?.kind === "grant" ? (
        <GrantRoleModal target={modal.target} grantRole={grantRole} onClose={() => { setModal(null); }} />
      ) : null}
      {modal?.kind === "revokePick" ? (
        <RevokePickModal
          target={modal.target}
          isBlocked={(g) => isFleetWideAdmin(g) && enabledFleetAdmins.size <= 1}
          onClose={() => { setModal(null); }}
          onPick={(grant) => { setModal({ kind: "revokeConfirm", target: modal.target, grant }); }}
        />
      ) : null}
      {modal?.kind === "revokeConfirm" ? (
        <RevokeConfirmModal
          target={modal.target}
          grant={modal.grant}
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

      {activityUserID ? (
        <UserActivityDrawer
          userID={activityUserID}
          username={all.find((u) => u.id === activityUserID)?.username ?? ""}
          onClose={() => { setActivityUserID(null); }}
        />
      ) : null}
    </>
  );
}

type ModalState =
  | { kind: "create" }
  | { kind: "created"; user: UserAccount; password: string }
  | { kind: "grant"; target: UserAccount }
  | { kind: "revokePick"; target: UserAccount }
  | { kind: "revokeConfirm"; target: UserAccount; grant: Grant }
  | { kind: "disable"; target: UserAccount }
  | { kind: "resetConfirm"; target: UserAccount }
  | { kind: "resetReveal"; target: UserAccount; password: string }
  | { kind: "forceLogout"; target: UserAccount };

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

function UserRow({
  user: u,
  isSelf,
  isOnlyFleetAdmin,
  onGrant,
  onRevokePick,
  onDisable,
  onEnable,
  onResetPassword,
  onForceLogout,
  onViewActivity,
}: {
  user: UserAccount;
  isSelf: boolean;
  isOnlyFleetAdmin: boolean;
  onGrant: () => void;
  onRevokePick: () => void;
  onDisable: () => void;
  onEnable: () => void;
  onResetPassword: () => void;
  onForceLogout: () => void;
  onViewActivity: () => void;
}) {
  const grants = u.grants ?? [];

  const items: RowMenuItem[] = [
    { label: "Grant role", icon: ShieldPlus, onClick: onGrant },
    {
      label: "Revoke role",
      icon: ShieldMinus,
      onClick: onRevokePick,
      disabled: grants.length === 0,
      disabledReason: grants.length === 0 ? "No active grants to revoke" : undefined,
    },
    u.disabled
      ? { label: "Enable user", icon: PlayCircle, onClick: onEnable }
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
        },
    { label: "Reset password", icon: KeyRound, onClick: onResetPassword },
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

  return (
    <tr className="border-b border-border/60 last:border-0 hover:bg-surface-hover">
      <td className="px-3 py-2.5">
        <span className="block font-medium text-text">
          {u.username}
          {isSelf ? <span className="ml-1.5 text-xs text-text-muted">(you)</span> : null}
        </span>
        {u.email ? <span className="block truncate text-xs text-text-subtle">{u.email}</span> : null}
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
                title={`Granted ${formatTime(g.granted_at)}${g.granted_by ? ` by ${g.granted_by}` : ""}`}
              >
                {g.role} · {scopeLabel(g)}
              </span>
            ))}
          </div>
        )}
      </td>
      <td className="px-3 py-2.5">
        <Badge tone={u.disabled ? "danger" : "success"}>{u.disabled ? "Disabled" : "Active"}</Badge>
      </td>
      <td className="px-3 py-2.5 text-xs text-text-muted">{formatTime(u.created_at)}</td>
      <td className="px-3 py-2.5 text-right">
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

function RowMenu({ items }: { items: RowMenuItem[] }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  return (
    <div
      ref={ref}
      className="relative inline-block text-left"
      onBlur={(e) => {
        if (!ref.current?.contains(e.relatedTarget)) setOpen(false);
      }}
      onKeyDown={(e) => {
        if (e.key === "Escape") setOpen(false);
      }}
    >
      <button
        type="button"
        onClick={() => { setOpen((o) => !o); }}
        aria-label="Actions"
        aria-haspopup="menu"
        aria-expanded={open}
        className="rounded-lg p-1.5 text-text-muted hover:bg-surface-hover hover:text-text"
      >
        <MoreHorizontal size={16} />
      </button>
      {open ? (
        <div role="menu" className="absolute right-0 z-20 mt-1 w-56 rounded-lg border border-border bg-surface py-1 shadow-lg">
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
        </div>
      ) : null}
    </div>
  );
}

// ------------------------------------------------------------- Modals ----

function ErrorLine({ error }: { error: string | null }) {
  if (!error) return null;
  return <p className="mt-3 text-sm text-danger">{error}</p>;
}

function CreateUserModal({
  createUser,
  onClose,
  onCreated,
}: {
  createUser: ReturnType<typeof useCreateUser>;
  onClose: () => void;
  onCreated: (user: UserAccount, password: string) => void;
}) {
  const [username, setUsername] = useState("");
  const [email, setEmail] = useState("");
  const [error, setError] = useState<string | null>(null);

  function submit(e: { preventDefault: () => void }) {
    e.preventDefault();
    setError(null);
    createUser.mutate(
      { username: username.trim(), ...(email.trim() ? { email: email.trim() } : {}) },
      {
        onSuccess: (res) => { onCreated(res.user, res.password); },
        onError: (err) => { setError(messageFor(err)); },
      },
    );
  }

  return (
    <Modal title="Create user" onClose={onClose}>
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
        <ErrorLine error={error} />
        <ModalActions>
          <Button type="button" variant="secondary" onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={createUser.isPending}>
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
  onGrantNow,
}: {
  user: UserAccount;
  password: string;
  onClose: () => void;
  onGrantNow: () => void;
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
        <Button variant="primary" onClick={onGrantNow}>Grant role now</Button>
      </ModalActions>
    </Modal>
  );
}

function GrantRoleModal({
  target,
  grantRole,
  onClose,
}: {
  target: UserAccount;
  grantRole: ReturnType<typeof useGrantRole>;
  onClose: () => void;
}) {
  const nodes = useNodes();
  const { push } = useToast();
  // No default between fleet-wide and a specific node — an admin must pick
  // one explicitly, the same rule GrantRoleRequest enforces server-side.
  const [role, setRole] = useState<Role | "">("");
  const [scope, setScope] = useState<"fleet-wide" | "node" | null>(null);
  const [nodeID, setNodeID] = useState("");
  const [error, setError] = useState<string | null>(null);

  function submit(e: { preventDefault: () => void }) {
    e.preventDefault();
    setError(null);
    if (!role) {
      setError("Choose a role.");
      return;
    }
    if (scope === null) {
      setError("Choose fleet-wide or a specific node — there is no default.");
      return;
    }
    if (scope === "node" && !nodeID) {
      setError("Choose a node.");
      return;
    }

    grantRole.mutate(
      { userID: target.id, body: { role, ...(scope === "fleet-wide" ? { fleet_wide: true } : { node_id: nodeID }) } },
      {
        onSuccess: () => {
          push({ tone: "success", title: `Granted ${role} to ${target.username}` });
          onClose();
        },
        onError: (err) => { setError(messageFor(err)); },
      },
    );
  }

  return (
    <Modal title="Grant role" onClose={onClose}>
      <form onSubmit={submit} className="flex flex-col gap-4">
        <p className="text-sm text-text-muted">
          Grant a role to <span className="font-medium text-text">{target.username}</span>.
        </p>

        <label className="flex flex-col gap-1.5 text-sm text-text">
          Role
          <select
            value={role}
            onChange={(e) => { setRole(e.target.value as Role); }}
            className="rounded-lg border border-border bg-bg px-3 py-2 text-sm text-text outline-none focus-visible:ring-2 focus-visible:ring-primary"
          >
            <option value="" disabled>Select role</option>
            {ROLES.map((r) => (
              <option key={r} value={r}>{r}</option>
            ))}
          </select>
        </label>

        <fieldset className="flex flex-col gap-2">
          <legend className="mb-1 text-sm text-text">Scope</legend>
          <label className="flex items-center gap-2 text-sm text-text">
            <input
              type="radio"
              name="scope"
              checked={scope === "fleet-wide"}
              onChange={() => { setScope("fleet-wide"); }}
            />
            Fleet-wide
          </label>
          <label className="flex items-center gap-2 text-sm text-text">
            <input
              type="radio"
              name="scope"
              checked={scope === "node"}
              onChange={() => { setScope("node"); }}
            />
            Specific node
          </label>
          {scope === "node" ? (
            <select
              value={nodeID}
              onChange={(e) => { setNodeID(e.target.value); }}
              aria-label="Node"
              className="ml-6 rounded-lg border border-border bg-bg px-3 py-2 text-sm text-text outline-none focus-visible:ring-2 focus-visible:ring-primary"
            >
              <option value="" disabled>Select node</option>
              {(nodes.data?.nodes ?? []).map((n) => (
                <option key={n.node_id} value={n.node_id}>{n.hostname}</option>
              ))}
            </select>
          ) : null}
        </fieldset>

        <ErrorLine error={error} />
        <ModalActions>
          <Button type="button" variant="secondary" onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={grantRole.isPending}>
            {grantRole.isPending ? "Granting…" : "Grant"}
          </Button>
        </ModalActions>
      </form>
    </Modal>
  );
}

function RevokePickModal({
  target,
  isBlocked,
  onClose,
  onPick,
}: {
  target: UserAccount;
  isBlocked: (g: Grant) => boolean;
  onClose: () => void;
  onPick: (g: Grant) => void;
}) {
  const grants = target.grants ?? [];
  return (
    <Modal title="Revoke role" onClose={onClose}>
      <p className="mb-3 text-sm text-text-muted">
        Choose which grant to remove from <span className="font-medium text-text">{target.username}</span>.
      </p>
      <ul className="flex flex-col gap-1.5">
        {grants.map((g) => {
          const blocked = isBlocked(g);
          return (
            <li key={g.id}>
              <button
                type="button"
                disabled={blocked}
                title={blocked ? "Can't remove — this is the last user with admin access" : undefined}
                onClick={() => { onPick(g); }}
                className="flex w-full items-center justify-between rounded-lg border border-border px-3 py-2 text-left text-sm hover:bg-surface-hover disabled:cursor-not-allowed disabled:opacity-40"
              >
                <span>
                  <span className="font-medium text-text">{g.role}</span>{" "}
                  <span className="text-text-muted">· {scopeLabel(g)}</span>
                </span>
                <span className="text-xs text-text-subtle">{formatTime(g.granted_at)}</span>
              </button>
            </li>
          );
        })}
      </ul>
      <ModalActions>
        <Button variant="secondary" onClick={onClose}>Cancel</Button>
      </ModalActions>
    </Modal>
  );
}

function RevokeConfirmModal({
  target,
  grant,
  revokeRole,
  onClose,
}: {
  target: UserAccount;
  grant: Grant;
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
        <span className="font-medium text-text">{scopeLabel(grant)}</span>?
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

function UserActivityDrawer({
  userID,
  username,
  onClose,
}: {
  userID: string;
  username: string;
  onClose: () => void;
}) {
  const audit = useUserAudit(userID);
  const entries = audit.data?.entries ?? emptyArray<AuditEntry>();

  return (
    <Drawer title="Activity" subtitle={username} onClose={onClose}>
      <QueryState
        isPending={audit.isPending}
        error={audit.error}
        isEmpty={entries.length === 0}
        onRetry={() => void audit.refetch()}
        rows={4}
        empty={{ art: emptyArt.data, title: "No recorded activity for this user yet" }}
      />
      {entries.length > 0 ? (
        <ul className="flex flex-col gap-1">
          {entries.map((e) => (
            <li key={e.id} className="border-b border-border py-2.5 text-sm last:border-0">
              <div className="flex items-center justify-between gap-2">
                <span className="font-medium text-text">{ACTION_LABEL[e.action]}</span>
                <span className="text-xs text-text-subtle">{formatTime(e.created_at)}</span>
              </div>
              <p className="mt-0.5 text-xs text-text-muted">
                by {e.actor_username ?? e.actor_user_id}
                {typeof e.detail?.role === "string" ? ` · ${e.detail.role}` : ""}
                {e.detail?.fleet_wide === true ? " · fleet-wide" : ""}
                {typeof e.detail?.node_id === "string" ? ` · ${e.detail.node_id}` : ""}
              </p>
            </li>
          ))}
        </ul>
      ) : null}
      <DrawerField label="User id" value={<span className="font-mono text-xs">{userID}</span>} />
    </Drawer>
  );
}
