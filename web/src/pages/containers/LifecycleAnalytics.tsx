import { useMemo } from "react";
import type { Container, ContainerState } from "../../api/types";
import { LIFECYCLE_ORDER, countByState, readExit } from "./containerModel";
import { Card, CardHeader } from "../../components/Card";
import { formatDuration } from "../../format";

/**
 * Lifecycle: what state containers are in, why the stopped ones stopped, and
 * how much of the estate has been restarting.
 *
 * The exit analysis is the point of this panel. A stopped container is not a
 * fault — most of the ones on a developer's host exited cleanly and are simply
 * switched off — so a count of "9 stopped" says nothing. Separating clean
 * exits from abnormal ones turns that into the two containers somebody should
 * look at.
 */

const STATE_STYLE: Record<ContainerState, { bar: string; text: string }> = {
  running: { bar: "bg-success", text: "text-success" },
  restarting: { bar: "bg-warning", text: "text-warning" },
  paused: { bar: "bg-warning", text: "text-warning" },
  created: { bar: "bg-info", text: "text-info" },
  exited: { bar: "bg-text-subtle", text: "text-text-subtle" },
  dead: { bar: "bg-danger", text: "text-danger" },
  removing: { bar: "bg-text-subtle", text: "text-text-subtle" },
};

export function LifecycleAnalytics({ containers }: { containers: Container[] }) {
  const states = useMemo(() => countByState(containers), [containers]);
  const present = LIFECYCLE_ORDER.filter((s) => states[s] > 0);
  const total = containers.length;

  const exits = useMemo(() => {
    const stopped = containers
      .map((c) => ({ container: c, exit: readExit(c) }))
      .filter((e) => e.exit.kind !== "none");
    return {
      all: stopped,
      abnormal: stopped.filter((e) => e.exit.abnormal),
      clean: stopped.filter((e) => !e.exit.abnormal).length,
    };
  }, [containers]);

  const restarted = useMemo(
    () => containers.filter((c) => c.restart_count > 0).sort((a, b) => b.restart_count - a.restart_count),
    [containers],
  );

  const oldest = useMemo(
    () =>
      [...containers]
        .filter((c) => c.created_at)
        .sort((a, b) => new Date(a.created_at ?? 0).getTime() - new Date(b.created_at ?? 0).getTime())[0],
    [containers],
  );

  const longestRunning = useMemo(
    () =>
      [...containers]
        .filter((c) => c.state === "running" && c.uptime_seconds)
        .sort((a, b) => (b.uptime_seconds ?? 0) - (a.uptime_seconds ?? 0))[0],
    [containers],
  );

  return (
    <div className="mb-6 grid grid-cols-1 gap-4 lg:grid-cols-2">
      <Card level="flat">
        <CardHeader title="Lifecycle" action={<span className="text-xs text-text-muted">{total} total</span>} />

        {/* One proportional bar: the shape of the estate reads before any
            number does. */}
        <div className="mb-3 flex h-2 overflow-hidden rounded-full bg-surface-hover">
          {present.map((s) => (
            <span
              key={s}
              className={STATE_STYLE[s].bar}
              style={{ width: `${String((states[s] / total) * 100)}%` }}
              title={`${String(states[s])} ${s}`}
            />
          ))}
        </div>

        <ul className="flex flex-col gap-1.5">
          {present.map((s) => (
            <li key={s} className="flex items-center gap-2 text-xs">
              <span aria-hidden="true" className={`h-2 w-2 shrink-0 rounded-sm ${STATE_STYLE[s].bar}`} />
              <span className="capitalize text-text">{s}</span>
              <span className="ml-auto tabular-nums text-text-muted">
                {states[s]} · {((states[s] / total) * 100).toFixed(0)}%
              </span>
            </li>
          ))}
        </ul>

        <dl className="mt-4 flex flex-col gap-1.5 border-t border-border pt-3 text-xs">
          {longestRunning ? (
            <Row
              label="Longest running"
              value={`${longestRunning.name} · ${formatDuration(longestRunning.uptime_seconds ?? 0)}`}
            />
          ) : null}
          {oldest?.created_at ? (
            <Row
              label="Oldest container"
              value={`${oldest.name} · created ${new Date(oldest.created_at).toLocaleDateString()}`}
            />
          ) : null}
        </dl>
      </Card>

      <Card level="flat">
        <CardHeader
          title="Exits and restarts"
          action={
            <span className="text-xs text-text-muted">
              {exits.abnormal.length > 0
                ? `${exits.abnormal.length} abnormal`
                : exits.all.length > 0
                  ? "all clean"
                  : "none stopped"}
            </span>
          }
        />

        {exits.all.length === 0 ? (
          <p className="py-6 text-center text-sm text-text-muted">
            No container has stopped. There is nothing to explain.
          </p>
        ) : (
          <>
            <p className="mb-3 text-xs leading-relaxed text-text-muted">
              {exits.clean} of {exits.all.length} stopped container
              {exits.all.length === 1 ? "" : "s"} exited cleanly — switched off rather than failed.
              {exits.abnormal.length > 0
                ? " The rest did not:"
                : " None ended abnormally."}
            </p>

            {exits.abnormal.length > 0 ? (
              <ul className="flex flex-col gap-2">
                {exits.abnormal.map(({ container: c, exit }) => (
                  <li key={c.id} className="elev-1 rounded-lg p-2.5">
                    <div className="flex items-baseline justify-between gap-2">
                      <span className="truncate text-sm font-medium text-text" title={c.name}>
                        {c.name}
                      </span>
                      <span className="shrink-0 font-mono text-xs text-danger">exit {exit.code}</span>
                    </div>
                    <p className="mt-0.5 text-[11px] leading-relaxed text-text-muted">{exit.reason}</p>
                  </li>
                ))}
              </ul>
            ) : null}
          </>
        )}

        {restarted.length > 0 ? (
          <div className="mt-4 border-t border-border pt-3">
            <h3 className="eyebrow mb-2">Restart history</h3>
            <ul className="flex flex-col gap-1.5">
              {restarted.slice(0, 6).map((c) => (
                <li key={c.id} className="flex items-center gap-2 text-xs">
                  <span className="min-w-0 flex-1 truncate text-text" title={c.name}>
                    {c.name}
                  </span>
                  <span className="shrink-0 tabular-nums text-warning">
                    {c.restart_count} restart{c.restart_count === 1 ? "" : "s"}
                  </span>
                </li>
              ))}
            </ul>
            {/* The daemon's counter is cumulative and survives restarts of the
                container, so it is a history rather than a rate. */}
            <p className="mt-2 text-[11px] leading-relaxed text-text-subtle">
              Docker's cumulative counter since each container was created, not a recent rate.
            </p>
          </div>
        ) : (
          <p className="mt-4 border-t border-border pt-3 text-[11px] leading-relaxed text-text-subtle">
            No container has been restarted by the daemon. Restart policies are shown per container
            in the inspector.
          </p>
        )}
      </Card>
    </div>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="shrink-0 text-text-muted">{label}</dt>
      <dd className="truncate text-right text-text" title={value}>
        {value}
      </dd>
    </div>
  );
}
