import type { MonitorHeartbeat, MonitorStatus } from "../../api/types";
import { formatDateTime } from "../../format";
import { FAILURE_LABEL, STATUS_LABEL } from "./monitorModel";

const COLOR: Record<MonitorStatus, string> = {
  up: "bg-success",
  down: "bg-danger",
  pending: "bg-warning",
  unknown: "bg-text-muted/40",
};

function describe(b: MonitorHeartbeat): string {
  const parts = [formatDateTime(b.time), STATUS_LABEL[b.status]];
  if (b.failure !== "") parts.push(FAILURE_LABEL[b.failure]);
  if (b.status_code !== null) parts.push(`status ${b.status_code}`);
  if (b.response_ms !== null) parts.push(`${b.response_ms} ms`);
  return parts.join(" · ");
}

/**
 * The latest checks as a row of bars, oldest first, padded on the left so
 * a new monitor's few checks sit at the right like everyone else's.
 * The colours repeat what the summary label says in words.
 */
export function HeartbeatBar({ beats, slots = 30 }: { beats: readonly MonitorHeartbeat[]; slots?: number }) {
  const shown = beats.slice(-slots);
  const down = shown.filter((b) => b.status === "down").length;
  const label = shown.length === 0
    ? "No checks yet"
    : `Last ${shown.length} checks: ${shown.length - down} not down, ${down} down`;
  return (
    <div role="img" aria-label={label} className="flex h-5 items-stretch gap-[2px]">
      {Array.from({ length: slots - shown.length }, (_, i) => (
        <span key={`pad-${i}`} aria-hidden="true" className="w-1 flex-1 rounded-sm bg-surface-hover" />
      ))}
      {shown.map((b) => (
        <span key={b.time} aria-hidden="true" title={describe(b)} className={`w-1 flex-1 rounded-sm ${COLOR[b.status]}`} />
      ))}
    </div>
  );
}
