import { Mail, MessageSquare, Webhook, type LucideIcon } from "lucide-react";
import type { NotificationChannel, NotificationChannelType } from "../../api/types";
import { Badge } from "../../components/Badge";
import { Button } from "../../components/Button";
import { TABLE, TD, TD_MUTED, TH, THEAD_TR, TR } from "../../components/table";
import { TYPE_LABEL } from "./channelModel";

const TYPE_ICON: Record<NotificationChannelType, LucideIcon> = { teams: MessageSquare, webhook: Webhook, email: Mail };

export function ChannelList({
  channels,
  onEdit,
}: {
  channels: NotificationChannel[];
  onEdit: (channel: NotificationChannel) => void;
}) {
  return (
    <div className="scroll-thin overflow-x-auto">
      <table aria-label="Notification channels" className={TABLE}>
        <thead>
          <tr className={THEAD_TR}>
            <th className={TH}>Name</th>
            <th className={TH}>Status</th>
            <th className={TH}>Minimum severity</th>
            <th className={TH}>Destination</th>
            <th className={TH}><span className="sr-only">Actions</span></th>
          </tr>
        </thead>
        <tbody>
          {channels.map((c) => {
            const Icon = TYPE_ICON[c.type];
            return (
              <tr key={c.id} className={TR}>
                <td className={TD}>
                  <span className="flex items-center gap-2.5">
                    <Icon size={16} aria-hidden="true" className="shrink-0 text-text-muted" />
                    <span className="min-w-0">
                      <span className="block font-medium text-text">{c.name}</span>
                      <span className="block text-xs text-text-subtle">{TYPE_LABEL[c.type]}</span>
                    </span>
                  </span>
                </td>
                <td className={TD}>
                  <Badge tone={c.enabled ? "success" : "neutral"}>{c.enabled ? "Enabled" : "Disabled"}</Badge>
                </td>
                <td className={TD}>
                  <Badge tone={c.min_severity === "critical" ? "danger" : "warning"}>
                    {c.min_severity === "critical" ? "Critical" : "Warning"}
                  </Badge>
                </td>
                <td className={TD_MUTED}>
                  <span className="block">{c.destination_host || "—"}</span>
                  <span className="block text-xs text-text-subtle">
                    {c.secret_configured ? "Secret configured" : "No secret configured"}
                  </span>
                </td>
                <td className={`${TD} text-right`}>
                  <Button size="sm" aria-label={`Edit ${c.name}`} onClick={() => { onEdit(c); }}>Edit</Button>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
