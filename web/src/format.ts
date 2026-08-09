import type { MetricUnit } from "./api/types";

/**
 * Formats a metric value according to its unit.
 *
 * Units travel with every sample precisely so this function can exist: the UI
 * must be able to format a metric it has never seen, because a new plugin adds
 * metrics without the frontend being rebuilt.
 */
export function formatValue(value: number, unit: MetricUnit): string {
  switch (unit) {
    case "percent":
      return `${value.toFixed(1)}%`;
    case "bytes":
      return formatBytes(value);
    case "bytes_per_second":
      return `${formatBytes(value)}/s`;
    case "operations_per_second":
      return `${formatCompact(value)}/s`;
    case "seconds":
      return formatDuration(value);
    case "ratio":
      return value.toFixed(2);
    default:
      return formatCompact(value);
  }
}

/** Binary units, because that is what the kernel reports. */
export function formatBytes(bytes: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  const sign = bytes < 0 ? "-" : "";
  let n = Math.abs(bytes);
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i += 1;
  }
  return `${sign}${n.toFixed(n >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

export function formatCompact(value: number): string {
  const abs = Math.abs(value);
  if (abs >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (abs >= 1_000) return `${(value / 1_000).toFixed(1)}k`;
  if (abs >= 10) return value.toFixed(0);
  return value.toFixed(abs < 1 ? 2 : 1);
}

/** Coarse duration, because uptime is read at a glance, not measured. */
export function formatDuration(seconds: number): string {
  if (seconds < 60) return `${Math.floor(seconds)}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${minutes % 60}m`;
  const days = Math.floor(hours / 24);
  return `${days}d ${hours % 24}h`;
}

/** Clock time for a chart axis or tooltip. */
export function formatTime(iso: string): string {
  const d = new Date(iso);
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

/** How long ago, for staleness. */
export function formatAgo(seconds: number): string {
  if (seconds < 2) return "just now";
  return `${formatDuration(seconds)} ago`;
}
