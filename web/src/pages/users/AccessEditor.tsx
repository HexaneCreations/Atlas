import { useEffect, useMemo, useReducer, useState } from "react";
import { ApiError } from "../../api/client";
import { useNodes, usePutUserAccess, useUserAccess } from "../../api/queries";
import type { Page, UserAccessResponse } from "../../api/types";
import { Button } from "../../components/Button";
import { useToast } from "../../components/Toast";
import {
  EDITABLE_ROLES,
  FLEET,
  FLEET_ONLY_PAGES,
  NODE_PAGES,
  changeText,
  diff,
  editorReducer,
  initialEditorState,
  nodeRows,
  pageLock,
  pagesForRow,
  problems,
  rowHasRole,
  saveErrorKind,
  toRequest,
  type Change,
  type Draft,
  type Edit,
  type RoleChoice,
  type RowKey,
} from "../accessEditor";

const PAGE_LABEL: Record<Page, string> = {
  overview: "Overview",
  nodes: "Nodes",
  users: "Users",
  containers: "Containers",
  processes: "Processes",
  services: "Services",
  cron: "Scheduled jobs",
  ports: "Ports",
  disks: "Disks",
};

export interface NodeInfo {
  node_id: string;
  hostname: string;
}

/**
 * The editable grid: a Fleet-wide row, then one row per node. Controlled —
 * the caller owns the draft and applies each [Edit].
 */
export function AccessGrid({
  server,
  draft,
  nodes,
  readOnly,
  onEdit,
}: {
  server: UserAccessResponse;
  draft: Draft;
  nodes: readonly NodeInfo[];
  readOnly: boolean;
  onEdit: (e: Edit) => void;
}) {
  const hostnames = new Map(nodes.map((n) => [n.node_id, n.hostname]));
  const rows: RowKey[] = [FLEET, ...nodeRows(server, nodes)];
  const fleetRole = draft.roles[FLEET];

  return (
    <div className="overflow-x-auto">
      <table className="w-full border-collapse text-sm" aria-label="Access">
        <thead>
          <tr>
            <th className="pb-2 text-left text-[11px] font-semibold tracking-wider text-text-subtle uppercase">Scope</th>
            <th className="pb-2 text-left text-[11px] font-semibold tracking-wider text-text-subtle uppercase">Role</th>
            <th className="pb-2 text-left text-[11px] font-semibold tracking-wider text-text-subtle uppercase">Pages</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => {
            const isFleet = row === FLEET;
            const own = draft.roles[row];
            const hasRole = rowHasRole(draft, row);
            const pagesDisabled = readOnly || (!isFleet && !hasRole);
            const hostname = hostnames.get(row);
            const known = isFleet || hostnames.has(row);
            const ticked = new Set(draft.pages[row] ?? []);
            return (
              <tr key={row} className="border-t border-border/60 align-top">
                <td className="py-2.5 pr-3">
                  <span className="font-medium text-text">{isFleet ? "Fleet-wide" : row}</span>
                  {!isFleet && hostname && hostname !== row ? (
                    <span className="block text-xs text-text-muted">{hostname}</span>
                  ) : null}
                  {!known ? <span className="block text-xs text-warning">node no longer reporting</span> : null}
                </td>
                <td className="py-2.5 pr-3">
                  <select
                    aria-label={`Role for ${isFleet ? "fleet-wide" : row}`}
                    value={own ?? "none"}
                    disabled={readOnly}
                    onChange={(e) => { onEdit({ kind: "role", row, role: e.target.value as RoleChoice }); }}
                    className="rounded-lg border border-border bg-bg px-2 py-1.5 text-sm text-text outline-none focus-visible:ring-2 focus-visible:ring-primary disabled:opacity-60"
                  >
                    <option value="none">{!isFleet && fleetRole ? `none (fleet-wide ${fleetRole})` : "none"}</option>
                    {EDITABLE_ROLES.map((r) => (
                      <option key={r} value={r}>{r}</option>
                    ))}
                  </select>
                </td>
                <td className="py-2.5">
                  <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
                    {pagesForRow(row).map((p) => {
                      const lock = pageLock(server, draft, row, p);
                      const checked = lock ? lock.ticked : ticked.has(p);
                      return (
                        <label
                          key={p}
                          title={lock?.reason}
                          className={`flex items-center gap-1.5 text-xs ${pagesDisabled || lock ? "text-text-subtle" : "text-text"}`}
                        >
                          <input
                            type="checkbox"
                            checked={checked}
                            disabled={pagesDisabled || Boolean(lock)}
                            onChange={(e) => { onEdit({ kind: "page", row, page: p, on: e.target.checked }); }}
                          />
                          {PAGE_LABEL[p]}
                          {lock ? <span className="text-[10px] text-text-subtle">({lock.reason})</span> : null}
                          {p === "users" && !lock ? (
                            <span className="text-[10px] text-text-subtle">(needs a role with user management)</span>
                          ) : null}
                        </label>
                      );
                    })}
                    {!readOnly ? (
                      <button
                        type="button"
                        disabled={pagesDisabled}
                        onClick={() => { onEdit({ kind: "selectAll", row }); }}
                        className="rounded px-1.5 py-0.5 text-xs font-medium text-primary hover:bg-primary/10 disabled:cursor-not-allowed disabled:opacity-40"
                      >
                        Select all
                      </button>
                    ) : null}
                  </div>
                  {!isFleet && !hasRole && !readOnly ? (
                    <p className="mt-1 text-[11px] text-text-subtle">Choose a role to grant pages on this node.</p>
                  ) : null}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      <p className="mt-2 text-[11px] text-text-subtle">
        {[...FLEET_ONLY_PAGES].map((p) => PAGE_LABEL[p]).join(", ")} are fleet-wide only.{" "}
        {NODE_PAGES.length} node pages can be granted per node or fleet-wide.
      </p>
    </div>
  );
}

/** The readable preview of what Save will do. Removed rows the editor does
 *  not show are labelled as such. */
export function ChangeList({ changes }: { changes: readonly Change[] }) {
  if (changes.length === 0) return null;
  return (
    <ul aria-label="Pending changes" className="flex flex-col gap-0.5 rounded-lg border border-border bg-surface-hover/40 p-2 text-xs">
      {changes.map((c) => (
        <li
          key={`${c.op}|${c.kind}|${c.row}|${c.value}`}
          className={c.op === "grant" ? "text-success" : c.removedBecause ? "text-warning" : "text-danger"}
        >
          {c.op === "grant" ? "+ " : "− "}
          {changeText(c)}
        </li>
      ))}
    </ul>
  );
}

/** "N changes: Save / Discard". No hooks, so it renders and is tested as a
 *  plain function. */
export function SaveBar({
  count,
  saving,
  blocked,
  onSave,
  onDiscard,
}: {
  count: number;
  saving: boolean;
  blocked: boolean;
  onSave: () => void;
  onDiscard: () => void;
}) {
  if (count === 0) return null;
  return (
    <div role="status" className="flex flex-wrap items-center gap-2 rounded-lg border border-primary/30 bg-primary/5 px-3 py-2">
      <span className="text-sm font-medium text-text">
        {count} change{count === 1 ? "" : "s"}
      </span>
      <span className="ml-auto flex gap-2">
        <Button variant="secondary" size="sm" onClick={onDiscard} disabled={saving}>
          Discard
        </Button>
        <Button variant="primary" size="sm" onClick={onSave} disabled={saving || blocked}>
          {saving ? "Saving…" : "Save"}
        </Button>
      </span>
    </div>
  );
}

interface SaveError {
  kind: "stale" | "server";
  message: string;
}

/**
 * The Access tab for an existing user: loads GET /users/{id}/access, edits a
 * draft, previews the diff, and saves with the version it loaded.
 * `readOnly` covers the caller's own account and any superadmin target.
 */
export function AccessTab({
  userID,
  readOnly,
  readOnlyReason,
  onDirtyChange,
}: {
  userID: string;
  readOnly: boolean;
  readOnlyReason?: string | undefined;
  onDirtyChange: (dirty: boolean) => void;
}) {
  const access = useUserAccess(userID);
  const nodes = useNodes();
  const save = usePutUserAccess();
  const { push } = useToast();
  const [error, setError] = useState<SaveError | null>(null);
  const [state, dispatch] = useReducer(editorReducer, undefined, () =>
    initialEditorState({ version: "", roles: [], pages: [], bundles: [] }),
  );

  const changes = useMemo(() => diff(state.server, state.draft), [state]);

  // Take a newer server state only while there is no unsaved draft; with a
  // draft open, a background refetch never overwrites it — the save then
  // gets 409 and the user chooses Reload.
  const loadedVersion = access.data?.version;
  useEffect(() => {
    if (access.data && (state.server.version === "" || changes.length === 0)) {
      dispatch({ type: "load", server: access.data });
    }
  }, [loadedVersion]); // eslint-disable-line react-hooks/exhaustive-deps

  const issues = useMemo(() => problems(state.draft), [state.draft]);
  const dirty = changes.length > 0;
  useEffect(() => { onDirtyChange(dirty); }, [dirty, onDirtyChange]);
  useEffect(() => () => { onDirtyChange(false); }, [onDirtyChange]);

  if (access.isPending) {
    return <div className="h-24 animate-pulse rounded-lg bg-surface-hover" />;
  }
  if (access.error) {
    return (
      <p className="text-sm text-danger">
        Could not load access: {access.error instanceof ApiError ? access.error.message : "could not reach Atlas"}.{" "}
        <button type="button" className="underline" onClick={() => void access.refetch()}>Retry</button>
      </p>
    );
  }

  function reload() {
    setError(null);
    void access.refetch().then((r) => {
      if (r.data) dispatch({ type: "load", server: r.data });
    });
  }

  function onSave() {
    setError(null);
    save.mutate(
      { userID, body: toRequest(state.server, state.draft) },
      {
        onSuccess: (res) => {
          dispatch({ type: "load", server: res.access });
          const n = res.changes.granted.length + res.changes.revoked.length;
          push({ tone: "success", title: `Access saved (${n} change${n === 1 ? "" : "s"})` });
        },
        onError: (err) => {
          if (err instanceof ApiError) {
            setError({ kind: saveErrorKind(err.status, err.message), message: err.message });
          } else {
            setError({ kind: "server", message: "Could not reach Atlas." });
          }
        },
      },
    );
  }

  return (
    <div className="flex flex-col gap-3">
      {readOnly && readOnlyReason ? (
        <p className="rounded-lg border border-border bg-surface-hover/40 px-3 py-2 text-xs text-text-muted">{readOnlyReason}</p>
      ) : null}
      <AccessGrid
        server={state.server}
        draft={state.draft}
        nodes={nodes.data?.nodes ?? []}
        readOnly={readOnly}
        onEdit={(e) => { setError(null); dispatch({ type: "edit", edit: e }); }}
      />
      {!readOnly ? (
        <>
          <ChangeList changes={changes} />
          {issues.length > 0 ? (
            <ul className="text-xs text-danger">
              {issues.map((m) => <li key={m}>{m}</li>)}
            </ul>
          ) : null}
          {error ? (
            <p role="alert" className="text-sm text-danger">
              {error.kind === "stale" ? "Access changed since you opened it." : error.message}{" "}
              {error.kind === "stale" ? (
                <button type="button" className="font-medium underline" onClick={reload}>Reload</button>
              ) : null}
            </p>
          ) : null}
          <SaveBar
            count={changes.length}
            saving={save.isPending}
            blocked={issues.length > 0}
            onSave={onSave}
            onDiscard={() => { setError(null); dispatch({ type: "discard" }); }}
          />
        </>
      ) : null}
    </div>
  );
}
