import { useEffect, useState } from "react";
import { Copy, Check, Plus, Server } from "lucide-react";
import { ApiError } from "../../api/client";
import { emptyArray } from "../../api/empty";
import { useCreateInstallCommand, useInstallCommands, useRevokeInstallCommand } from "../../api/queries";
import type { InstallCommand, InstallCodeStatus } from "../../api/types";
import { Badge, type Tone } from "../../components/Badge";
import { Button } from "../../components/Button";
import { Card, CardHeader } from "../../components/Card";
import { Modal, ModalActions } from "../../components/Modal";
import { QueryState } from "../../components/QueryState";
import { useToast } from "../../components/Toast";
import { emptyArt } from "../../lib/assets";
import { formatDateTime } from "../../format";

const STATUS_TONE: Record<InstallCodeStatus, Tone> = {
  pending: "info",
  redeemed: "success",
  expired: "neutral",
  revoked: "danger",
};

function messageFor(error: unknown): string {
  return error instanceof ApiError ? error.message : "Could not reach Atlas.";
}

/**
 * Add server: the one-time install-code flow for enrolling a new machine.
 *
 * Gated by `fleet.enroll` — see auth/me's can_enroll_fleet — because an
 * install code admits a new machine to the fleet with no further operator
 * action, the same reason user creation is restricted. The server never
 * returns a code's plaintext more than once (CreateInstallCommandResponse
 * is the only place it appears), so the "reveal" modal below is this
 * feature's entire lifetime for that value: once closed, it is gone from
 * this page for good, same as a freshly created user's password.
 */
export function InstallCodesPanel() {
  const commands = useInstallCommands();
  const create = useCreateInstallCommand();
  const revoke = useRevokeInstallCommand();
  const { push } = useToast();

  const [modal, setModal] = useState<ModalState | null>(null);
  const all = commands.data?.install_commands ?? emptyArray<InstallCommand>();

  return (
    <>
      <Card level="floating" className="mb-4">
        <CardHeader
          title="Install codes"
          action={
            <Button variant="primary" size="sm" icon={Plus} onClick={() => { setModal({ kind: "add" }); }}>
              Add server
            </Button>
          }
        />
        <QueryState
          isPending={commands.isPending}
          error={commands.error}
          isEmpty={all.length === 0}
          onRetry={() => void commands.refetch()}
          rows={2}
          empty={{
            art: emptyArt.data,
            title: "No install codes yet",
            description: "Generate one to enroll a new server with no manual token or peer authorization.",
          }}
        />
        {all.length > 0 ? (
          <div className="overflow-x-auto">
            <table aria-label="Install codes" className="w-full border-collapse text-sm">
              <thead>
                <tr>
                  {["Node", "Environment", "Status", "Created by", "Expires", ""].map((h, i) => (
                    <th
                      key={h || i}
                      className={`px-3 py-2 text-[11px] font-semibold tracking-wider text-text-muted uppercase ${
                        i === 5 ? "text-right" : "text-left"
                      }`}
                    >
                      {h}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {all.map((c) => (
                  <tr key={c.id} className="border-t border-border/60">
                    <td className="px-3 py-2 font-mono text-xs text-text">{c.node_id}</td>
                    <td className="px-3 py-2 text-xs text-text-muted">{c.environment}</td>
                    <td className="px-3 py-2">
                      <Badge tone={STATUS_TONE[c.status]}>{c.status}</Badge>
                    </td>
                    <td className="px-3 py-2 text-xs text-text-muted">{c.created_by}</td>
                    <td className="px-3 py-2 text-xs text-text-muted">{formatDateTime(c.expires_at)}</td>
                    <td className="px-3 py-2 text-right">
                      {c.status === "pending" ? (
                        <button
                          type="button"
                          onClick={() => { setModal({ kind: "revoke", command: c }); }}
                          className="rounded px-2 py-1 text-xs font-medium text-danger hover:bg-danger/10"
                        >
                          Revoke
                        </button>
                      ) : null}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </Card>

      {modal?.kind === "add" ? (
        <AddServerModal
          create={create}
          onClose={() => { setModal(null); }}
          onCreated={(command, expiresAt) => { setModal({ kind: "reveal", command, expiresAt }); }}
        />
      ) : null}
      {modal?.kind === "reveal" ? (
        <RevealCommandModal command={modal.command} expiresAt={modal.expiresAt} onClose={() => { setModal(null); }} />
      ) : null}
      {modal?.kind === "revoke" ? (
        <RevokeConfirmModal
          command={modal.command}
          revoke={revoke}
          onClose={() => { setModal(null); }}
          onRevoked={() => { push({ tone: "success", title: "Install code revoked" }); }}
        />
      ) : null}
    </>
  );
}

type ModalState =
  | { kind: "add" }
  | { kind: "reveal"; command: string; expiresAt: string }
  | { kind: "revoke"; command: InstallCommand };

function ErrorLine({ error }: { error: string | null }) {
  if (!error) return null;
  return <p className="mt-3 text-sm text-danger">{error}</p>;
}

const DEFAULT_TTL_MINUTES = 30;
const MAX_TTL_MINUTES = 60;
const ENVIRONMENT_RE = /^[A-Za-z0-9._-]{1,64}$/;

function AddServerModal({
  create,
  onClose,
  onCreated,
}: {
  create: ReturnType<typeof useCreateInstallCommand>;
  onClose: () => void;
  onCreated: (command: string, expiresAt: string) => void;
}) {
  const [environment, setEnvironment] = useState("");
  const [ttlMinutes, setTtlMinutes] = useState(DEFAULT_TTL_MINUTES);
  const [error, setError] = useState<string | null>(null);

  const envValid = ENVIRONMENT_RE.test(environment.trim());
  const ttlValid = ttlMinutes >= 1 && ttlMinutes <= MAX_TTL_MINUTES;

  function submit(e: { preventDefault: () => void }) {
    e.preventDefault();
    setError(null);
    create.mutate(
      { environment: environment.trim(), ttl_minutes: ttlMinutes },
      {
        onSuccess: (res) => { onCreated(res.command, res.expires_at); },
        onError: (err) => { setError(messageFor(err)); },
      },
    );
  }

  return (
    <Modal title="Add server" onClose={onClose}>
      <form onSubmit={submit} className="flex flex-col gap-3">
        <p className="text-sm text-text-muted">
          Generates a one-time install code. Paste the resulting command on a fresh Ubuntu host — it downloads,
          verifies, installs and enrolls the agent with no manual token or peer authorization.
        </p>
        <label className="flex flex-col gap-1.5 text-sm text-text">
          Environment
          <input
            autoFocus
            value={environment}
            onChange={(e) => { setEnvironment(e.target.value); }}
            placeholder="production"
            required
            className="rounded-lg border border-border bg-bg px-3 py-2 text-sm text-text outline-none focus-visible:ring-2 focus-visible:ring-primary"
          />
        </label>
        <label className="flex flex-col gap-1.5 text-sm text-text">
          Expires in (minutes)
          <input
            type="number"
            min={1}
            max={MAX_TTL_MINUTES}
            value={ttlMinutes}
            onChange={(e) => { setTtlMinutes(Number(e.target.value)); }}
            className="rounded-lg border border-border bg-bg px-3 py-2 text-sm text-text outline-none focus-visible:ring-2 focus-visible:ring-primary"
          />
          <span className="text-xs text-text-muted">Default {DEFAULT_TTL_MINUTES}, maximum {MAX_TTL_MINUTES}.</span>
        </label>
        <ErrorLine error={error} />
        <ModalActions>
          <Button type="button" variant="secondary" onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={create.isPending || !envValid || !ttlValid}>
            {create.isPending ? "Generating…" : "Generate command"}
          </Button>
        </ModalActions>
      </form>
    </Modal>
  );
}

/** mm:ss remaining until `expiresAt`, ticking every second; "expired" at zero. */
function useCountdown(expiresAt: string): string {
  const [remaining, setRemaining] = useState(() => Date.parse(expiresAt) - Date.now());
  useEffect(() => {
    const id = window.setInterval(() => { setRemaining(Date.parse(expiresAt) - Date.now()); }, 1000);
    return () => { window.clearInterval(id); };
  }, [expiresAt]);
  if (remaining <= 0) return "expired";
  const totalSeconds = Math.floor(remaining / 1000);
  const m = Math.floor(totalSeconds / 60);
  const s = totalSeconds % 60;
  return `${m}:${String(s).padStart(2, "0")}`;
}

function RevealCommandModal({
  command,
  expiresAt,
  onClose,
}: {
  command: string;
  expiresAt: string;
  onClose: () => void;
}) {
  const [copied, setCopied] = useState(false);
  const countdown = useCountdown(expiresAt);

  return (
    <Modal title="Install command" onClose={onClose} width="lg">
      <p className="text-sm text-text-muted">
        Run this on the new host as root. It will not be shown again — generate a new one if you lose it.
      </p>
      <div className="mt-3 flex items-start gap-2 rounded-lg border border-border bg-bg p-3">
        <code className="scroll-thin flex-1 overflow-x-auto whitespace-pre-wrap break-all font-mono text-xs text-text">
          {command}
        </code>
        <button
          type="button"
          onClick={() => {
            void navigator.clipboard.writeText(command).then(() => {
              setCopied(true);
              window.setTimeout(() => { setCopied(false); }, 2000);
            });
          }}
          aria-label="Copy install command"
          className="shrink-0 rounded p-1 text-text-muted hover:bg-surface-hover hover:text-text"
        >
          {copied ? <Check size={14} className="text-success" /> : <Copy size={14} />}
        </button>
      </div>
      <p className="mt-2 flex items-center gap-1.5 text-xs text-warning">
        <Server size={13} />
        This command cannot be shown again. The code expires in {countdown}.
      </p>
      <ModalActions>
        <Button variant="primary" onClick={onClose}>Done</Button>
      </ModalActions>
    </Modal>
  );
}

function RevokeConfirmModal({
  command,
  revoke,
  onClose,
  onRevoked,
}: {
  command: InstallCommand;
  revoke: ReturnType<typeof useRevokeInstallCommand>;
  onClose: () => void;
  onRevoked: () => void;
}) {
  const [error, setError] = useState<string | null>(null);

  return (
    <Modal title="Revoke install code" onClose={onClose}>
      <p className="text-sm text-text-muted">
        Revoke the pending install code for node <span className="font-mono text-text">{command.node_id}</span>{" "}
        ({command.environment})? It can no longer be redeemed.
      </p>
      <ErrorLine error={error} />
      <ModalActions>
        <Button variant="secondary" onClick={onClose}>Cancel</Button>
        <Button
          variant="danger"
          disabled={revoke.isPending}
          onClick={() => {
            setError(null);
            revoke.mutate(command.id, {
              onSuccess: () => { onRevoked(); onClose(); },
              onError: (err) => { setError(messageFor(err)); },
            });
          }}
        >
          {revoke.isPending ? "Revoking…" : "Revoke"}
        </Button>
      </ModalActions>
    </Modal>
  );
}
