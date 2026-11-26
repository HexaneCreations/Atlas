import type { NotificationDelivery } from "../../api/types";
import { Badge, type Tone } from "../../components/Badge";
import { TABLE, TD, TD_MUTED, TH, THEAD_TR, TR } from "../../components/table";
import { formatDateTime } from "../../format";
import { TYPE_LABEL, deliveryTitle, shortError } from "./channelModel";

const STATUS_TONE: Record<NotificationDelivery["status"], Tone> = {
  pending: "info",
  delivered: "success",
  failed: "danger",
};

const KIND_TONE: Record<NotificationDelivery["kind"], Tone> = {
  firing: "danger",
  escalated: "danger",
  recovered: "success",
  flapping: "warning",
  test: "neutral",
};

export function DeliveryList({
  deliveries,
  channelNames,
}: {
  deliveries: NotificationDelivery[];
  channelNames: ReadonlyMap<string, string>;
}) {
  return (
    <div className="scroll-thin max-h-[34rem] overflow-auto">
      <table aria-label="Recent deliveries" className={TABLE}>
        <thead className="sticky top-0 z-10">
          <tr className={THEAD_TR}>
            <th className={TH}>Time</th>
            <th className={TH}>Channel</th>
            <th className={TH}>Alert</th>
            <th className={TH}>Status</th>
            <th className={TH}>Error</th>
          </tr>
        </thead>
        <tbody>
          {deliveries.map((d) => (
            <tr key={d.id} className={TR}>
              <td className={TD_MUTED}>{formatDateTime(d.created_at)}</td>
              <td className={TD}>{channelNames.get(d.channel_id) ?? TYPE_LABEL[d.channel_type]}</td>
              <td className={TD}>
                <span className="mr-2">{deliveryTitle(d)}</span>
                <Badge tone={KIND_TONE[d.kind]}>{d.kind}</Badge>
              </td>
              <td className={TD}><Badge tone={STATUS_TONE[d.status]}>{d.status}</Badge></td>
              <td className={TD_MUTED}>{shortError(d.last_error) || "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
