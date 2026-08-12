import type { LogLine } from "../../api/types";

/**
 * Pure logic for the container Logs page, kept apart from rendering for the
 * same reason as containerModel.ts: scrolling, merge, and search rules are
 * arguable in isolation and do not need a DOM to be tested.
 */

// ------------------------------------------------------------- scrolling ---

/** Within this many pixels of the bottom counts as "at the bottom" for
 *  auto-follow purposes — matches the tolerance the inline viewer used. */
export const BOTTOM_THRESHOLD_PX = 48;

export function isNearBottom(scrollHeight: number, scrollTop: number, clientHeight: number): boolean {
  return scrollHeight - scrollTop - clientHeight < BOTTOM_THRESHOLD_PX;
}

/**
 * The scrollTop that keeps whatever was on screen anchored after older lines
 * are prepended above it (see [useLogScroll]'s `beginPrepend`/`pendingPrepend`
 * pair): the container grew by `newScrollHeight - previousScrollHeight`
 * pixels above the old content, so the same growth has to be added back to
 * where the reader was scrolled to, or that content visually shifts down —
 * or, worse, clamps to whatever the new scrollTop max happens to be.
 */
export function compensateScrollTopForPrepend(
  previousScrollTop: number,
  previousScrollHeight: number,
  newScrollHeight: number,
): number {
  return previousScrollTop + (newScrollHeight - previousScrollHeight);
}

// ------------------------------------------------------------ live merge ---

/**
 * How many trailing lines the live-follow connection is asked to replay
 * before it starts streaming new ones (see internal/api/v1/containers.go,
 * ContainerLogsFollow: Follow+Tail always replays Tail lines first — there is
 * no `since` or `tail=0` on that endpoint to suppress it). Kept small so the
 * overlap [dedupeLiveBurst] has to reconcile is small, not zero.
 */
export const FOLLOW_REPLAY_TAIL = 5;

/**
 * Drops the prefix of `liveSoFar` that is a duplicate replay of the tail of
 * `historical` — the only way to honour "previous logs + new live logs"
 * (never "only what arrived after opening the page") given that the follow
 * endpoint always re-sends its own tail window first.
 *
 * Matches by exact (time, stream, message) rather than by count, since the
 * replay window is a server-side constant, not something this function
 * should assume the caller always honoured.
 */
export function dedupeLiveBurst(historical: LogLine[], liveSoFar: LogLine[]): LogLine[] {
  const maxOverlap = Math.min(historical.length, liveSoFar.length, FOLLOW_REPLAY_TAIL * 2);
  for (let k = maxOverlap; k > 0; k--) {
    const tail = historical.slice(historical.length - k);
    const head = liveSoFar.slice(0, k);
    if (sameLines(tail, head)) {
      return liveSoFar.slice(k);
    }
  }
  return liveSoFar;
}

function sameLines(a: LogLine[], b: LogLine[]): boolean {
  for (let i = 0; i < a.length; i++) {
    const x = a[i];
    const y = b[i];
    if (!x || x.time !== y?.time || x.stream !== y.stream || x.message !== y.message) {
      return false;
    }
  }
  return true;
}

/** The lines to render: historical, followed by whatever of the live burst
 *  is not a duplicate of historical's own tail. */
export function mergeLogLines(historical: LogLine[], liveSoFar: LogLine[]): LogLine[] {
  if (liveSoFar.length === 0) return historical;
  return [...historical, ...dedupeLiveBurst(historical, liveSoFar)];
}

// -------------------------------------------------------------- history ---

/** Server-side cap, mirrored from internal/api/v1/containers.go's
 *  maxLogTail — "Load older" must stop offering more once a request for this
 *  many lines would be capped anyway, since a same-sized response at that
 *  point means there is nothing older left to reveal, not that the request
 *  failed. */
export const MAX_LOG_TAIL = 5000;

/** The next tail size "Load older logs" should request. Docker's log API has
 *  no cursor — this is a bigger tail window, not incremental pagination —
 *  so it doubles the request rather than adding a fixed page size, keeping
 *  the number of clicks needed to reach old history roughly logarithmic. */
export function nextTail(currentTail: number): number {
  return Math.min(currentTail * 2, MAX_LOG_TAIL);
}

/** Whether "Load older logs" has anything left to offer. Once the tail is
 *  already at the server's cap, another request would return the identical
 *  window. */
export function canLoadOlder(currentTail: number): boolean {
  return currentTail < MAX_LOG_TAIL;
}

// ---------------------------------------------------------------- search ---

export interface LogMatch {
  lineIndex: number;
  start: number;
  end: number;
}

/** Every occurrence of `query` across `lines`, case-insensitive. Client-side
 *  only, over whatever is currently loaded — no query backend. */
export function findMatches(lines: LogLine[], query: string): LogMatch[] {
  if (!query) return [];
  const needle = query.toLowerCase();
  const matches: LogMatch[] = [];
  lines.forEach((line, lineIndex) => {
    const haystack = line.message.toLowerCase();
    let from = 0;
    for (;;) {
      const at = haystack.indexOf(needle, from);
      if (at === -1) break;
      matches.push({ lineIndex, start: at, end: at + needle.length });
      from = at + needle.length;
    }
  });
  return matches;
}

// ----------------------------------------------------------------- text ---

/** Plain-text rendering shared by copy and download, so what an operator
 *  copies and what they save to disk are always the same content. */
export function renderLogText(lines: LogLine[]): string {
  return lines.map((l) => `${l.time} ${l.message}`).join("\n");
}

/** A download filename that says what it is without claiming completeness —
 *  containerRef is typically the container's name or short id. */
export function logFileName(containerRef: string, lineCount: number): string {
  const safe = containerRef.replace(/[^a-zA-Z0-9._-]/g, "_");
  const stamp = new Date().toISOString().replace(/[:.]/g, "-");
  return `${safe}-logs-${String(lineCount)}lines-${stamp}.log`;
}

// ---------------------------------------------------------------- routing ---

/** The dedicated Logs page route for a container. Exported for testing, the
 *  same reason as queries.ts's containersPath: it is the one line deciding
 *  whether a link opens the right node's container. */
export function containerLogsPath(containerID: string, nodeID: string | undefined): string {
  return `/containers/${encodeURIComponent(containerID)}/logs?node=${encodeURIComponent(nodeID ?? "")}`;
}
