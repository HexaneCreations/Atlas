import { Clock, ShieldAlert } from "lucide-react";
import { useCronJobs, usePrimaryNodeID } from "../api/queries";
import { ApiError, inventoryGapKind } from "../api/client";
import { AgentSubjectGap } from "../components/AgentSubjectGap";
import type { CronJob } from "../api/types";
import { Card } from "../components/Card";
import { EmptyState } from "../components/EmptyState";
import { emptyArt, errorArt } from "../lib/assets";
import { StatCard } from "../components/StatCard";
import { Badge } from "../components/Badge";
import { PageHeader } from "../components/PageHeader";
import { QueryState } from "../components/QueryState";
import { TABLE, TABLE_WRAP, TD, TD_MUTED, TH, THEAD_TR, TR } from "../components/table";

export function CronPage() {
  const nodeID = usePrimaryNodeID();
  const cron = useCronJobs(nodeID);

  if (inventoryGapKind(cron.error) === "agent") {
    return <AgentSubjectGap subject="scheduled jobs" />;
  }

  if (cron.error instanceof ApiError && cron.error.code === "not_implemented") {
    return (
      <>
        <PageHeader title="Scheduled jobs" subtitle="System, user, and packaged cron jobs." />
        <Card>
          <EmptyState
            kind="unavailable"
            art={errorArt.forbidden}
            title="No readable crontab on this host"
            description="Atlas could not read any cron source. Where crontabs are root-only and Atlas runs unprivileged, this means “cannot see” rather than “nothing scheduled”."
            hint="Atlas reads /etc/crontab, /etc/cron.d and per-user tables. It parses schedules; it never installs, edits or triggers a job."
          />
        </Card>
      </>
    );
  }

  const jobs = cron.data?.jobs ?? [];
  const total = cron.data?.total ?? 0;
  const root = cron.data?.root ?? 0;

  return (
    <>
      <PageHeader
        stats={[
          { label: "Jobs", value: String(total), hint: "readable by Atlas" },
          {
            label: "Running as root",
            value: String(root),
            hint: root > 0 ? "full privileges" : "none",
            tone: root > 0 ? "warning" : "success",
          },
          {
            label: "Sources",
            value: String(new Set(jobs.map((j) => j.source)).size),
            hint: "crontab locations",
          },
          {
            label: "On reboot",
            value: String(jobs.filter((j) => j.schedule.startsWith("@reboot")).length),
            hint: "run at boot",
          },
        ]}
      />

      <div className="mb-6 grid grid-cols-2 gap-4 lg:grid-cols-4">
        <StatCard label="Total jobs" icon={Clock} value={String(total)} iconTone="primary" />
        <StatCard
          label="Running as root"
          icon={ShieldAlert}
          value={String(root)}
          tone={root > 0 ? "warning" : "neutral"}
          iconTone={root > 0 ? "warning" : "primary"}
        />
      </div>

      <Card>
        <QueryState
          isPending={cron.isPending}
          error={cron.error}
          isEmpty={jobs.length === 0}
          onRetry={() => void cron.refetch()}
          rows={4}
          empty={{
            art: emptyArt.data,
            title: "Nothing is scheduled on this host",
            description:
              "Every cron source Atlas can read parsed cleanly and contained no jobs. On a host that delegates its scheduling elsewhere, that is the expected answer.",
            hint: "systemd timers are not cron jobs and are not listed here — they appear under Services as their own units.",
          }}
        />

        {!cron.isPending && !cron.error && jobs.length > 0 ? (
          <div className={TABLE_WRAP}>
            <table className={TABLE}>
              <thead>
                <tr className={THEAD_TR}>
                  <th className={TH}>Schedule</th>
                  <th className={TH}>Command</th>
                  <th className={TH}>User</th>
                  <th className={TH}>Source</th>
                </tr>
              </thead>
              <tbody>
                {jobs.map((job, i) => (
                  <CronRow key={`${job.file ?? ""}:${job.line ?? i}`} job={job} />
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </Card>
    </>
  );
}

function CronRow({ job }: { job: CronJob }) {
  return (
    <tr className={TR}>
      <td className={`${TD} whitespace-nowrap font-mono text-xs`}>{job.schedule}</td>
      <td className={`${TD_MUTED} max-w-[36rem] truncate font-mono text-xs`} title={job.command}>
        {job.command}
      </td>
      <td className={TD}>
        {job.root ? (
          <Badge tone="warning">root</Badge>
        ) : (
          <span className="text-text-muted">{job.user ?? "—"}</span>
        )}
      </td>
      <td className={TD_MUTED}>{job.source}</td>
    </tr>
  );
}
