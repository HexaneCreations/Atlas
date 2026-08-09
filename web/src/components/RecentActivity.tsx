import { AlertTriangle, CheckCircle2, Info, XCircle } from "lucide-react";
import { useActivity } from "../api/queries";
import type { ActivityEntry, ActivitySeverity } from "../api/types";
import { Card, CardHeader } from "./Card";
import { QueryState } from "./QueryState";
import { formatAgo } from "../format";

const SEVERITY = {
  success: { icon: CheckCircle2, cls: "bg-success/10 text-success" },
  info: { icon: Info, cls: "bg-info/10 text-info" },
  warning: { icon: AlertTriangle, cls: "bg-warning/10 text-warning" },
  danger: { icon: XCircle, cls: "bg-danger/10 text-danger" },
} as const satisfies Record<ActivitySeverity, { icon: typeof Info; cls: string }>;

/**
 * The recent-activity feed.
 *
 * The server holds this in memory, so it starts empty after a restart. The
 * empty state says so rather than implying nothing has ever happened — those
 * are different facts and an operations tool must not conflate them.
 */
export function RecentActivity() {
  const activity = useActivity(12);
  const entries = activity.data?.entries ?? [];

  return (
    <Card>
      <CardHeader
        title="Recent activity"
        action={
          activity.data && activity.data.dropped > 0 ? (
            <span
              className="text-xs text-warning"
              title="The recorder fell behind the event bus, so this feed has gaps."
            >
              {activity.data.dropped} dropped
            </span>
          ) : null
        }
      />

      <QueryState isPending={activity.isPending} error={activity.error} />

      {!activity.isPending && !activity.error ? (
        entries.length === 0 ? (
          <p className="py-8 text-center text-sm text-text-muted">
            Nothing notable since Atlas started {formatAgo(secondsSince(activity.data.since))}.
          </p>
        ) : (
          <ul className="flex flex-col">
            {entries.map((e) => (
              <ActivityRow key={e.id} entry={e} />
            ))}
          </ul>
        )
      ) : null}
    </Card>
  );
}

function ActivityRow({ entry }: { entry: ActivityEntry }) {
  const { icon: Icon, cls } = SEVERITY[entry.severity];

  return (
    <li className="flex items-start gap-3 border-b border-border py-3 last:border-0">
      <span className={`mt-0.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-full ${cls}`}>
        <Icon size={14} />
      </span>
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium text-text">{entry.title}</p>
        {entry.detail ? <p className="truncate text-xs text-text-muted">{entry.detail}</p> : null}
      </div>
      <span className="shrink-0 text-xs text-text-muted" title={new Date(entry.time).toLocaleString()}>
        {formatAgo(secondsSince(entry.time))}
      </span>
    </li>
  );
}

function secondsSince(iso: string): number {
  return Math.max((Date.now() - new Date(iso).getTime()) / 1000, 0);
}
