import { useMemo, useState } from "react";
import { BellPlus } from "lucide-react";
import { notificationsDisabled, useNotificationChannels, useNotificationDeliveries } from "../api/queries";
import { emptyArray } from "../api/empty";
import type { NotificationChannel, NotificationDelivery } from "../api/types";
import { Button } from "../components/Button";
import { Card, CardHeader } from "../components/Card";
import { EmptyAction, EmptyState } from "../components/EmptyState";
import { PageHeader } from "../components/PageHeader";
import { QueryState } from "../components/QueryState";
import { emptyArt, errorArt } from "../lib/assets";
import { ChannelFormModal } from "./notifications/ChannelForm";
import { ChannelList } from "./notifications/ChannelList";
import { DeliveryList } from "./notifications/DeliveryList";

const DELIVERIES_STEP = 25;
/** The most the API returns at once. */
const DELIVERIES_MAX = 500;

/**
 * Where alerts are sent, and whether they arrived.
 *
 * Reached only with can_manage_notifications (see App and shell/pageAccess);
 * every request behind it is authorized by the server regardless.
 */
export function NotificationsPage() {
  const channels = useNotificationChannels();
  const [form, setForm] = useState<{ channel: NotificationChannel | null } | null>(null);
  const all = channels.data?.channels ?? emptyArray<NotificationChannel>();
  const names = useMemo(() => new Map(all.map((c) => [c.id, c.name])), [all]);

  if (notificationsDisabled(channels.error)) {
    return (
      <>
        <PageHeader />
        <NotificationsDisabled />
      </>
    );
  }

  const add = () => { setForm({ channel: null }); };
  return (
    <>
      <PageHeader
        action={<Button variant="primary" icon={BellPlus} onClick={add}>Add notification</Button>}
      />

      <Card level="floating" className="p-0">
        <QueryState
          isPending={channels.isPending}
          error={channels.error}
          isEmpty={all.length === 0}
          onRetry={() => void channels.refetch()}
          empty={{
            art: emptyArt.data,
            title: "No notifications yet",
            description: "Add a Teams or webhook notification to be told when something goes wrong.",
            action: <EmptyAction onClick={add}>Add notification</EmptyAction>,
          }}
        />
        {all.length > 0 ? <ChannelList channels={all} onEdit={(channel) => { setForm({ channel }); }} /> : null}
      </Card>

      <RecentDeliveries channelNames={names} />

      {form ? <ChannelFormModal channel={form.channel} onClose={() => { setForm(null); }} /> : null}
    </>
  );
}

export function NotificationsDisabled() {
  return (
    <Card level="floating" className="p-0">
      <EmptyState
        kind="unavailable"
        art={errorArt.offline}
        title="Notifications are disabled"
        description="This server has no notifications encryption key, so it cannot store where to send alerts."
        hint="An operator must set ATLAS_NOTIFICATIONS_ENCRYPTION_KEY on the server and restart it."
        compact
      />
    </Card>
  );
}

function RecentDeliveries({ channelNames }: { channelNames: ReadonlyMap<string, string> }) {
  const [limit, setLimit] = useState(DELIVERIES_STEP);
  const query = useNotificationDeliveries(limit);
  const deliveries = query.data?.deliveries ?? emptyArray<NotificationDelivery>();

  return (
    <Card level="raised" className="mt-6 p-0">
      <div className="px-5 pt-5"><CardHeader title="Recent deliveries" /></div>
      <QueryState
        isPending={query.isPending}
        error={query.error}
        isEmpty={deliveries.length === 0}
        onRetry={() => void query.refetch()}
        empty={{ art: emptyArt.data, title: "Nothing has been sent yet" }}
      />
      {deliveries.length > 0 ? <DeliveryList deliveries={deliveries} channelNames={channelNames} /> : null}
      {deliveries.length >= limit && limit < DELIVERIES_MAX ? (
        <div className="border-t border-border p-3 text-center">
          <Button size="sm" disabled={query.isFetching} onClick={() => { setLimit((l) => Math.min(l * 2, DELIVERIES_MAX)); }}>
            Show more
          </Button>
        </div>
      ) : null}
    </Card>
  );
}
