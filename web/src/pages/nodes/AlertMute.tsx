import { Bell, BellOff } from "lucide-react";
import { useMuteNode } from "../../api/queries";
import type { MuteDuration, Node } from "../../api/types";
import { useAuth } from "../../auth/AuthContext";
import { Badge } from "../../components/Badge";
import { Button } from "../../components/Button";
import { useToast } from "../../components/Toast";
import { messageFor } from "../../lib/apiMessage";
import { nodePrimaryLabel } from "../../lib/nodeIdentity";
import { MUTE_CHOICES, muteLabel } from "./muteModel";

/** Mutes or unmutes one node's alerts. Nothing without
 *  can_manage_notifications; the server checks it on every call. */
export function AlertMute({ node }: { node: Node }) {
  const { user } = useAuth();
  const mute = useMuteNode();
  const { push } = useToast();
  if (!user?.can_manage_notifications) return null;

  const set = (duration: MuteDuration | null) => {
    mute.mutate(
      { nodeID: node.node_id, duration },
      {
        onSuccess: () => {
          push({ tone: "success", title: `Alerts ${duration ? "muted" : "unmuted"} for ${nodePrimaryLabel(node)}` });
        },
      },
    );
  };
  return (
    <AlertMuteView
      mutedUntil={node.alerts_muted_until}
      pending={mute.isPending}
      error={mute.error ? messageFor(mute.error) : null}
      onSet={set}
    />
  );
}

export function AlertMuteView({
  mutedUntil,
  pending,
  error,
  onSet,
}: {
  mutedUntil: string | null;
  pending: boolean;
  error: string | null;
  /** null unmutes. */
  onSet: (duration: MuteDuration | null) => void;
}) {
  const label = muteLabel(mutedUntil);
  return (
    <section aria-label="Alerts" className="border-b border-border px-4 py-3">
      {label ? (
        <div className="flex items-center justify-between gap-2">
          <span className="flex items-center gap-1.5 text-xs text-text-muted">
            <BellOff size={13} aria-hidden="true" />
            Alerts: {label}
          </span>
          <Button size="sm" icon={Bell} disabled={pending} onClick={() => { onSet(null); }}>Unmute</Button>
        </div>
      ) : (
        <div role="group" aria-label="Mute alerts" className="flex flex-wrap items-center gap-1.5">
          <span className="mr-1 text-xs text-text-muted">Mute alerts</span>
          {MUTE_CHOICES.map((c) => (
            <Button
              key={c.duration}
              size="sm"
              disabled={pending}
              aria-label={`Mute alerts: ${c.label}`}
              onClick={() => { onSet(c.duration); }}
            >
              {c.label}
            </Button>
          ))}
        </div>
      )}
      <p aria-live="polite" className="mt-2 text-xs text-danger">{error}</p>
    </section>
  );
}

/** The node table's mark for a muted node. */
export function MutedBadge({ mutedUntil, visible }: { mutedUntil: string | null; visible: boolean }) {
  const label = muteLabel(mutedUntil);
  if (!visible || !label) return null;
  return (
    <span title={label} className="mt-1 block">
      <Badge tone="neutral">Muted<span className="sr-only">, {label}</span></Badge>
    </span>
  );
}
