import type { Grant } from "../../api/types";
import { formatDateTime } from "../../format";
import type { RevokeGuard } from "../usersAccess";

/**
 * A user's active role grants with a per-grant Revoke. A superadmin grant
 * never gets a Revoke control: the API refuses it for every caller, so the
 * only way to replace the superadmin is the CLI's transfer-superadmin.
 */
export function RoleGrantsTable({
  grants,
  scopeLabel,
  guardFor,
  onRevoke,
}: {
  grants: readonly Grant[];
  scopeLabel: (g: Grant) => string;
  guardFor: (g: Grant) => RevokeGuard;
  onRevoke: (g: Grant) => void;
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full border-collapse text-sm">
        <thead>
          <tr>
            {["Role", "Granted by", "Granted at", ""].map((h, i) => (
              <th
                key={h || i}
                className={`pb-1 text-[11px] font-semibold tracking-wider text-text-subtle uppercase ${i === 3 ? "text-right" : "text-left"}`}
              >
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {grants.map((g) => {
            const guard = guardFor(g);
            return (
              <tr key={g.id} className="border-t border-border/60">
                <td className="py-2 pr-3">
                  <span className="font-medium text-text">{g.role}</span>
                  <span className="text-text-muted"> · {scopeLabel(g)}</span>
                </td>
                <td className="py-2 pr-3 text-xs text-text-muted">{g.granted_by ?? "—"}</td>
                <td className="py-2 pr-3 text-xs text-text-muted">{formatDateTime(g.granted_at)}</td>
                <td className="py-2 text-right">
                  {g.role === "superadmin" ? null : (
                    <button
                      type="button"
                      disabled={guard.disabled}
                      title={guard.reason ?? "Revoke"}
                      onClick={() => { onRevoke(g); }}
                      className="rounded px-2 py-1 text-xs font-medium text-danger hover:bg-danger/10 disabled:cursor-not-allowed disabled:opacity-40"
                    >
                      Revoke
                    </button>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
