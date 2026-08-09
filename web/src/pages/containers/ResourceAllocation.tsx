import { useMemo } from "react";
import type { Container } from "../../api/types";
import type { ContainerUsage, ProjectSummary } from "./containerModel";
import { Card, CardHeader } from "../../components/Card";
import { formatBytes, formatValue } from "../../format";

/**
 * Where resources are actually going.
 *
 * Every figure comes from the live stats collector, which samples the daemon's
 * `docker stats`. That has nothing to say about a stopped container, so this
 * panel covers running containers only and says so — a table padded out with
 * zeroed rows for stopped containers would make a busy host look idle and bury
 * the containers that matter.
 *
 * Memory is shown against each container's own limit where one is configured.
 * An unlimited container is reported as such rather than as a percentage of
 * the host, because "42% of unlimited" is not a number.
 */
export function ResourceAllocation({
  containers,
  usage,
  projects,
}: {
  containers: Container[];
  usage: Map<string, ContainerUsage>;
  projects: ProjectSummary[];
}) {
  const rows = useMemo(() => {
    const out = containers
      .map((c) => ({ container: c, use: usage.get(c.name) }))
      .filter((r): r is { container: Container; use: ContainerUsage } => Boolean(r.use));
    // Busiest first: this panel exists to find the consumer.
    out.sort((a, b) => (b.use.cpu ?? 0) - (a.use.cpu ?? 0));
    return out;
  }, [containers, usage]);

  const notReporting = containers.length - rows.length;

  const projectTotals = useMemo(
    () => projects.filter((p) => p.reporting > 0).sort((a, b) => (b.cpu ?? 0) - (a.cpu ?? 0)),
    [projects],
  );

  // Bars scale against the busiest project. A leader at zero would divide by
  // zero, so the floor is 1 — every bar then renders at its minimum width,
  // which is the right picture for a fleet using no CPU.
  const leadCpu = Math.max(projectTotals[0]?.cpu ?? 0, 1);

  if (rows.length === 0) {
    return (
      <Card level="flat" className="mb-6">
        <CardHeader title="Resource allocation" />
        <p className="py-8 text-center text-sm text-text-muted">
          No container is running, so there is no live resource use to report.
        </p>
      </Card>
    );
  }

  return (
    <Card level="flat" className="mb-6">
      <CardHeader
        title="Resource allocation"
        action={
          <span className="text-xs text-text-muted">
            {rows.length} running
            {notReporting > 0 ? ` · ${String(notReporting)} not running, excluded` : ""}
          </span>
        }
      />

      <div className="scroll-thin overflow-x-auto">
        <table aria-label="Container resource allocation" className="w-full border-collapse text-sm">
          <thead>
            <tr className="border-b border-border">
              <Th align="left">Container</Th>
              <Th>CPU</Th>
              <Th>Memory</Th>
              <Th align="right">Network I/O</Th>
              <Th align="right">Disk I/O</Th>
              <Th align="right">PIDs</Th>
            </tr>
          </thead>
          <tbody>
            {rows.map(({ container: c, use }) => (
              <tr key={c.id} className="border-b border-border/60 last:border-0">
                <td className="max-w-[16rem] py-2.5 pr-3">
                  <span className="block truncate font-medium text-text" title={c.name}>
                    {c.name}
                  </span>
                  <span className="block truncate text-[11px] text-text-subtle">
                    {c.compose_project ?? "standalone"}
                  </span>
                </td>

                <td className="px-2 py-2.5">
                  <Bar value={use.cpu} label={use.cpu !== undefined ? formatValue(use.cpu, "percent") : "—"} />
                </td>

                <td className="px-2 py-2.5">
                  <span className="block text-xs font-medium tabular-nums text-text">
                    {use.memory !== undefined ? formatBytes(use.memory) : "—"}
                  </span>
                  {/* A limit of zero is Docker's "unlimited". Rendering the
                      percentage anyway would divide by the host's memory and
                      imply a ceiling the container does not have. */}
                  {use.memoryLimit && use.memoryPercent !== undefined ? (
                    <>
                      <span className="mt-0.5 block h-1 w-full max-w-24 overflow-hidden rounded-full bg-surface-hover">
                        <span
                          className={`block h-full rounded-full ${barColor(use.memoryPercent)}`}
                          style={{ width: `${String(Math.min(Math.max(use.memoryPercent, 1), 100))}%` }}
                        />
                      </span>
                      <span className="text-[11px] tabular-nums text-text-subtle">
                        {use.memoryPercent.toFixed(1)}% of {formatBytes(use.memoryLimit)}
                      </span>
                    </>
                  ) : (
                    <span className="text-[11px] text-text-subtle">no limit set</span>
                  )}
                </td>

                <td className="px-2 py-2.5 text-right text-xs tabular-nums text-text-muted">
                  <span className="block">↓ {formatValue(use.netRx ?? 0, "bytes_per_second")}</span>
                  <span className="block">↑ {formatValue(use.netTx ?? 0, "bytes_per_second")}</span>
                </td>

                <td className="px-2 py-2.5 text-right text-xs tabular-nums text-text-muted">
                  <span className="block">R {formatValue(use.blockRead ?? 0, "bytes_per_second")}</span>
                  <span className="block">W {formatValue(use.blockWrite ?? 0, "bytes_per_second")}</span>
                </td>

                <td className="px-2 py-2.5 text-right text-xs tabular-nums text-text-muted">
                  {use.pids ?? "—"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {projectTotals.length > 1 ? (
        <div className="mt-5 border-t border-border pt-4">
          <h3 className="eyebrow mb-2.5">By project</h3>
          <ul className="flex flex-col gap-2">
            {projectTotals.map((p) => (
              <li key={p.name} className="flex items-center gap-3 text-xs">
                <span className="w-32 shrink-0 truncate text-text" title={p.name}>
                  {p.name}
                </span>
                <span className="h-1.5 flex-1 overflow-hidden rounded-full bg-surface-hover">
                  <span
                    className="block h-full rounded-full bg-primary"
                    style={{
                      width: `${String(Math.max(((p.cpu ?? 0) / leadCpu) * 100, 3))}%`,
                    }}
                  />
                </span>
                <span className="w-16 shrink-0 text-right tabular-nums text-text-muted">
                  {p.cpu !== undefined ? formatValue(p.cpu, "percent") : "—"}
                </span>
                <span className="w-20 shrink-0 text-right tabular-nums text-text-muted">
                  {p.memory !== undefined ? formatBytes(p.memory) : "—"}
                </span>
              </li>
            ))}
          </ul>
          <p className="mt-2 text-[11px] text-text-subtle">
            Each project's total is summed from its running members only.
          </p>
        </div>
      ) : null}
    </Card>
  );
}

function Th({
  children,
  align = "left",
}: {
  children: React.ReactNode;
  align?: "left" | "right";
}) {
  return (
    <th
      className={`px-2 py-2 text-[11px] font-semibold tracking-wider text-text-muted uppercase ${
        align === "right" ? "text-right" : "text-left"
      }`}
    >
      {children}
    </th>
  );
}

function barColor(v: number): string {
  return v >= 90 ? "bg-danger" : v >= 75 ? "bg-warning" : "bg-primary";
}

/** CPU on a fixed 0–100 scale so containers are comparable by eye. A container
 *  may exceed 100% where it uses more than one core, so the bar clamps while
 *  the figure does not. */
function Bar({ value, label }: { value: number | undefined; label: string }) {
  if (value === undefined) return <span className="text-xs text-text-subtle">—</span>;
  return (
    <>
      <span className={`block text-xs font-medium tabular-nums ${value >= 90 ? "text-danger" : value >= 75 ? "text-warning" : "text-text"}`}>
        {label}
      </span>
      <span className="mt-0.5 block h-1 w-full max-w-24 overflow-hidden rounded-full bg-surface-hover">
        <span
          className={`block h-full rounded-full ${barColor(value)}`}
          style={{ width: `${String(Math.min(Math.max(value, 1), 100))}%` }}
        />
      </span>
    </>
  );
}
