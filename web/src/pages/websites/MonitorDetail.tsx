import { useState } from "react";
import { Pause, Pencil, Play, Trash2, WifiOff } from "lucide-react";
import { useDeleteMonitor, useMonitor, useMonitorEvents, useMonitorHeartbeats, useSetMonitorActive } from "../../api/queries";
import { emptyArray } from "../../api/empty";
import type { MonitorDetail as Detail, MonitorEvent, MonitorGroup, MonitorHeartbeat, MonitorProbeState } from "../../api/types";
import { Badge } from "../../components/Badge";
import { Button } from "../../components/Button";
import { Card, CardHeader } from "../../components/Card";
import { TimeSeriesChart } from "../../components/Chart";
import { QueryState } from "../../components/QueryState";
import { useToast } from "../../components/Toast";
import { formatDateTime, formatDuration } from "../../format";
import { messageFor } from "../../lib/apiMessage";
import { DeleteMonitorConfirm } from "./MonitorForm";
import {
  FAILURE_LABEL,
  certDaysLeft,
  displayStatus,
  formatUptime,
  responsePoints,
  uptimeTone,
} from "./monitorModel";

/** The right of the Websites page: one monitor in full. */
export function MonitorDetail({
  monitorID,
  probe,
  groups,
  canManage,
  onEdit,
  onDeleted,
}: {
  monitorID: string;
  probe: MonitorProbeState | undefined;
  groups: readonly MonitorGroup[];
  canManage: boolean;
  onEdit: (m: Detail) => void;
  onDeleted: () => void;
}) {
  const query = useMonitor(monitorID);
  const m = query.data;
  if (!m) {
    return (
      <Card level="raised">
        <QueryState isPending={query.isPending} error={query.error} onRetry={() => void query.refetch()} rows={6} />
      </Card>
    );
  }
  return (
    <div className="flex flex-col gap-4">
      {probe?.offline ? <ProbeOfflineBanner probe={probe} /> : null}
      <StatusHeader monitor={m} groups={groups} canManage={canManage} onEdit={onEdit} onDeleted={onDeleted} />
      <UptimeStats monitor={m} />
      <ResponseTimeChart monitorID={m.id} />
      <EventsList monitorID={m.id} />
      {/* 13b: alert rules, notification channels and mute for this monitor go here. */}
    </div>
  );
}

export function ProbeOfflineBanner({ probe }: { probe: MonitorProbeState }) {
  return (
    <div role="status" className="flex items-start gap-3 rounded-xl border border-warning/40 bg-warning/10 p-4 text-sm text-warning">
      <WifiOff size={18} aria-hidden="true" className="mt-0.5 shrink-0" />
      <p>
        <span className="font-semibold">The probe is offline.</span>{" "}
        Atlas itself has lost connectivity{probe.since ? ` since ${formatDateTime(probe.since)}` : ""}, so failed checks
        say nothing about the sites. Statuses are held until it is back.
      </p>
    </div>
  );
}

function Stat({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs text-text-muted">{label}</dt>
      <dd className="mt-0.5 truncate text-sm font-medium text-text tabular-nums">{children}</dd>
    </div>
  );
}

export function StatusHeader({
  monitor: m,
  groups,
  canManage,
  onEdit,
  onDeleted,
}: {
  monitor: Detail;
  groups: readonly MonitorGroup[];
  canManage: boolean;
  onEdit: (m: Detail) => void;
  onDeleted: () => void;
}) {
  const status = displayStatus(m);
  const certDays = certDaysLeft(m.live.cert_expires_at);
  const group = groups.find((g) => g.id === m.group_id);
  return (
    <Card level="floating">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 className="truncate text-lg font-semibold text-text">{m.name}</h2>
          <p className="truncate text-sm text-text-muted">
            {m.method} {m.url}
          </p>
          {group || m.tags.length > 0 ? (
            <p className="mt-1 flex flex-wrap gap-1.5 text-xs text-text-subtle">
              {group ? <span>Group: {group.name}</span> : null}
              {m.tags.map((t) => <span key={t} className="rounded bg-surface-hover px-1.5 py-0.5">{t}</span>)}
            </p>
          ) : null}
        </div>
        <Badge tone={status.tone}>{status.label}</Badge>
      </div>
      {m.description ? <p className="mt-3 text-sm text-text-muted">{m.description}</p> : null}

      <dl className="mt-4 grid grid-cols-2 gap-4 sm:grid-cols-4">
        <Stat label="Last check">{m.live.last_check_at ? formatDateTime(m.live.last_check_at) : "Not yet"}</Stat>
        <Stat label="Status code">{m.live.last_status_code ?? "—"}</Stat>
        <Stat label="Response time">{m.live.last_response_ms !== null ? `${m.live.last_response_ms} ms` : "—"}</Stat>
        <Stat label="Certificate">
          {certDays === null ? "—" : certDays < 0 ? "Expired" : `Expires in ${certDays} days`}
        </Stat>
      </dl>
      {m.live.last_failure !== "" && m.active ? (
        <p className="mt-3 text-sm text-danger">Last failure: {FAILURE_LABEL[m.live.last_failure]}</p>
      ) : null}
      {m.redacted ? (
        <p className="mt-3 text-xs text-text-subtle">The query string, headers and authentication are hidden from your role.</p>
      ) : null}
      {canManage ? <ManageActions monitor={m} onEdit={onEdit} onDeleted={onDeleted} /> : null}
    </Card>
  );
}

function ManageActions({ monitor: m, onEdit, onDeleted }: { monitor: Detail; onEdit: (m: Detail) => void; onDeleted: () => void }) {
  const active = useSetMonitorActive();
  const remove = useDeleteMonitor();
  const { push } = useToast();
  const [confirm, setConfirm] = useState(false);
  const [error, setError] = useState<string | null>(null);
  return (
    <div className="mt-4 flex flex-wrap gap-2 border-t border-border pt-4">
      <Button
        size="sm"
        icon={m.active ? Pause : Play}
        disabled={active.isPending}
        onClick={() => {
          active.mutate(
            { monitorID: m.id, active: !m.active },
            {
              onSuccess: () => { push({ tone: "success", title: m.active ? `${m.name} paused` : `${m.name} resumed` }); },
              onError: (e) => { push({ tone: "danger", title: messageFor(e) }); },
            },
          );
        }}
      >
        {m.active ? "Pause" : "Resume"}
      </Button>
      <Button size="sm" icon={Pencil} onClick={() => { onEdit(m); }}>Edit</Button>
      <Button size="sm" variant="danger" icon={Trash2} onClick={() => { setError(null); setConfirm(true); }}>Delete</Button>
      {confirm ? (
        <DeleteMonitorConfirm
          name={m.name}
          pending={remove.isPending}
          error={error}
          onClose={() => { setConfirm(false); }}
          onConfirm={() => {
            remove.mutate(m.id, {
              onSuccess: () => {
                push({ tone: "success", title: `${m.name} deleted` });
                setConfirm(false);
                onDeleted();
              },
              onError: (e) => { setError(messageFor(e)); },
            });
          }}
        />
      ) : null}
    </div>
  );
}

export function UptimeStats({ monitor: m }: { monitor: Pick<Detail, "uptime"> }) {
  const rows = [
    { label: "24 hours", value: m.uptime.day },
    { label: "7 days", value: m.uptime.week },
    { label: "30 days", value: m.uptime.month },
  ];
  return (
    <Card level="raised">
      <CardHeader title="Uptime" />
      <dl className="grid grid-cols-3 gap-4">
        {rows.map((r) => (
          <div key={r.label}>
            <dt className="text-xs text-text-muted">{r.label}</dt>
            <dd className="mt-1"><Badge tone={uptimeTone(r.value)}>{formatUptime(r.value)}</Badge></dd>
          </div>
        ))}
      </dl>
    </Card>
  );
}

function ResponseTimeChart({ monitorID }: { monitorID: string }) {
  const query = useMonitorHeartbeats(monitorID);
  const points = responsePoints(query.data?.heartbeats ?? emptyArray<MonitorHeartbeat>());
  return (
    <Card level="raised">
      <CardHeader title="Response time, last 24 hours (ms)" />
      {query.data ? (
        <TimeSeriesChart series={[{ label: "Response time (ms)", slot: 1, points }]} unit="count" area />
      ) : (
        <QueryState isPending={query.isPending} error={query.error} onRetry={() => void query.refetch()} />
      )}
    </Card>
  );
}

const EVENT_TEXT: Record<MonitorEvent["kind"], string> = { down: "Went down", up: "Came back up" };

export function EventsList({ monitorID }: { monitorID: string }) {
  const query = useMonitorEvents(monitorID);
  const events = query.data?.events ?? emptyArray<MonitorEvent>();
  return (
    <Card level="raised" className="p-0">
      <div className="px-5 pt-5"><CardHeader title="Events" /></div>
      <QueryState
        isPending={query.isPending}
        error={query.error}
        isEmpty={events.length === 0}
        onRetry={() => void query.refetch()}
        empty={{ title: "No down or up events yet" }}
      />
      {events.length > 0 ? <EventRows events={events} /> : null}
    </Card>
  );
}

export function EventRows({ events }: { events: readonly MonitorEvent[] }) {
  return (
    <ul className="divide-y divide-border">
      {events.map((e) => (
        <li key={e.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-5 py-3 text-sm">
          <Badge tone={e.kind === "down" ? "danger" : "success"}>{EVENT_TEXT[e.kind]}</Badge>
          <span className="text-text-muted">{formatDateTime(e.time)}</span>
          {e.failure !== "" ? <span className="text-text">{FAILURE_LABEL[e.failure]}</span> : null}
          {e.status_code !== null ? <span className="text-text-muted">status {e.status_code}</span> : null}
          {e.kind === "up" && e.duration_seconds !== null ? (
            <span className="text-text-muted">after {formatDuration(e.duration_seconds)} down</span>
          ) : null}
        </li>
      ))}
    </ul>
  );
}
