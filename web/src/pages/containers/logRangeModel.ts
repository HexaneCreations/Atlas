import type { ContainerLogsResponse, LogLine, LogRange } from "../../api/types";

/**
 * Pure logic for choosing a log time range and showing what came back: the
 * presets, the time-zone handling, validation that mirrors the server's, and
 * turning untrusted log text into something safe to display.
 */

export type PresetID = "15m" | "1h" | "6h" | "24h" | "custom";

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;

export const PRESETS: { id: PresetID; label: string; ms: number }[] = [
  { id: "15m", label: "Last 15 minutes", ms: 15 * MINUTE },
  { id: "1h", label: "Last hour", ms: HOUR },
  { id: "6h", label: "Last 6 hours", ms: 6 * HOUR },
  { id: "24h", label: "Last 24 hours", ms: 24 * HOUR },
  { id: "custom", label: "Custom", ms: 0 },
];

/** The longest range, mirroring container_logs.max_range's default. Wider
 *  ranges are refused by the server; one day keeps a historical read (which
 *  must read its whole window) bounded on a busy container. */
export const MAX_RANGE_MS = 24 * HOUR;

/** How far ahead "to" may be and still mean now (the server's clockSkew). */
const SKEW_MS = MINUTE;

/** The range a preset means at `now`. */
export function presetRange(id: Exclude<PresetID, "custom">, now: Date): LogRange {
  const ms = PRESETS.find((p) => p.id === id)?.ms ?? HOUR;
  return { since: new Date(now.getTime() - ms).toISOString(), until: null };
}

/** Only a range that ends now is ever live. */
export function canFollow(range: LogRange): boolean {
  return range.until === null;
}

const pad = (n: number, width = 2) => String(n).padStart(width, "0");

interface Parts { y: number; mo: number; d: number; h: number; mi: number; s: number; ms: number }

function parts(date: Date, utc: boolean): Parts {
  return utc
    ? { y: date.getUTCFullYear(), mo: date.getUTCMonth() + 1, d: date.getUTCDate(), h: date.getUTCHours(),
        mi: date.getUTCMinutes(), s: date.getUTCSeconds(), ms: date.getUTCMilliseconds() }
    : { y: date.getFullYear(), mo: date.getMonth() + 1, d: date.getDate(), h: date.getHours(),
        mi: date.getMinutes(), s: date.getSeconds(), ms: date.getMilliseconds() };
}

/** A log line's timestamp in the chosen zone, to the millisecond. */
export function formatLogTime(iso: string, utc: boolean): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  const p = parts(date, utc);
  return `${String(p.y)}-${pad(p.mo)}-${pad(p.d)} ${pad(p.h)}:${pad(p.mi)}:${pad(p.s)}.${pad(p.ms, 3)}`;
}

/** The value for a datetime-local input showing iso in the chosen zone. */
export function toInputValue(iso: string, utc: boolean): string {
  const p = parts(new Date(iso), utc);
  return `${String(p.y)}-${pad(p.mo)}-${pad(p.d)}T${pad(p.h)}:${pad(p.mi)}:${pad(p.s)}`;
}

/** Reads a datetime-local value as a time in the chosen zone; null when it
 *  is not one. */
export function fromInputValue(value: string, utc: boolean): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(value);
  if (!m) return null;
  const num = (i: number) => Number(m[i] ?? "0");
  const [y, mo, d, h, mi, s] = [num(1), num(2), num(3), num(4), num(5), num(6)];
  const date = utc ? new Date(Date.UTC(y, mo - 1, d, h, mi, s)) : new Date(y, mo - 1, d, h, mi, s);
  // Rejects 2026-02-30 and the like, which Date silently rolls over.
  const back = parts(date, utc);
  if (back.y !== y || back.mo !== mo || back.d !== d || back.h !== h || back.mi !== mi) return null;
  return date;
}

/** The zone times are shown in, for the indicator next to the picker. */
export function zoneLabel(utc: boolean, at: Date = new Date()): string {
  if (utc) return "UTC";
  const name = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const offset = -at.getTimezoneOffset();
  const sign = offset >= 0 ? "+" : "-";
  const abs = Math.abs(offset);
  return `${name} (UTC${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)})`;
}

/** Validates a custom range typed in the chosen zone. An empty "to" ends the
 *  range now. */
export function validateCustomRange(
  fromValue: string,
  toValue: string,
  utc: boolean,
  now: Date,
  maxMs = MAX_RANGE_MS,
): { range: LogRange } | { error: string } {
  const from = fromInputValue(fromValue, utc);
  if (!from) return { error: "Enter a valid start date and time." };
  if (from.getTime() > now.getTime()) return { error: "The start is in the future." };
  let end = now;
  let until: string | null = null;
  if (toValue !== "") {
    const to = fromInputValue(toValue, utc);
    if (!to) return { error: "Enter a valid end date and time, or leave it empty for now." };
    if (to.getTime() > now.getTime() + SKEW_MS) return { error: "The end is in the future; leave it empty to end the range now." };
    if (to.getTime() <= from.getTime()) return { error: "The start must be before the end." };
    end = to.getTime() < now.getTime() ? to : now;
    until = end.toISOString();
  }
  if (end.getTime() - from.getTime() > maxMs) {
    return { error: `A range may span at most ${String(Math.round(maxMs / HOUR))} hours.` };
  }
  return { range: { since: from.toISOString(), until } };
}

// ------------------------------------------------------------- display ---

// CSI (ESC [ … final), OSC (ESC ] … BEL or ESC \) and the remaining two-byte
// escapes. Colour codes carry no meaning once they are not a terminal's.
// eslint-disable-next-line no-control-regex
const ANSI = /\u001b\[[0-?]*[ -/]*[@-~]|\u001b\][^\u0007\u001b]*(?:\u0007|\u001b\\)?|\u001b[@-Z\\-_]/g;
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u0008\u000b-\u001f\u007f\u0080-\u009f]/g;

/** A log message as text that is safe to show: ANSI escapes removed, every
 *  other control character made visible, tabs and spaces kept. It is always
 *  rendered as a text node, never as markup. */
export function displayMessage(message: string): string {
  return message.replace(ANSI, "").replace(CONTROL, "�");
}

/** The lines whose message contains query, case-insensitive; every line when
 *  query is empty. */
export function filterLines(lines: LogLine[], query: string): LogLine[] {
  const needle = query.trim().toLowerCase();
  if (needle === "") return lines;
  return lines.filter((l) => displayMessage(l.message).toLowerCase().includes(needle));
}

/** Pages arrive newest first; lines are shown oldest first. */
export function pagesToLines(pages: readonly ContainerLogsResponse[]): LogLine[] {
  return [...pages].reverse().flatMap((p) => p.lines);
}

/** What the reader must be told about the loaded range. */
export interface RangeNotices {
  /** The container keeps nothing older than this; the range starts before. */
  availableSince: string | null;
  scanLimited: boolean;
  /** A response cap dropped lines (more can still be loaded). */
  truncated: boolean;
  /** The node's agent cannot end a read at a time; the server did. */
  fallback: boolean;
  more: boolean;
}

export function rangeNotices(pages: readonly ContainerLogsResponse[]): RangeNotices {
  const last = pages[pages.length - 1];
  return {
    availableSince: last?.available_since ?? null,
    scanLimited: pages.some((p) => p.scan_limited === true),
    truncated: pages.some((p) => p.truncated === true),
    fallback: pages.some((p) => p.fallback === true),
    more: last?.more === true && last.next_before !== undefined,
  };
}
