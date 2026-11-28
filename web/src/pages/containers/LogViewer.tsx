import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { ArrowDown, ArrowUp, Clock, Copy, Download, Search, WrapText } from "lucide-react";
import { useContainerLogRange } from "../../api/queries";
import { useContainerLogFollow, type FollowStatus } from "../../api/logFollow";
import type { LogLine, LogRange } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { QueryState } from "../../components/QueryState";
import { Badge } from "../../components/Badge";
import { emptyArt } from "../../lib/assets";
import {
  FOLLOW_REPLAY_TAIL, compensateScrollTopForPrepend, findMatches, isNearBottom,
  logFileName, mergeLogLines, renderLogText, type LogMatch,
} from "./logViewerModel";
import {
  PRESETS, canFollow, displayMessage, filterLines, formatLogTime, pagesToLines, presetRange, rangeNotices,
  toInputValue, validateCustomRange, zoneLabel, type PresetID,
} from "./logRangeModel";

/**
 * A container's logs for a chosen time range: the newest page of the range
 * first, older pages on "Load more", and, only for a range that ends now, a
 * live follow appended after it. Search filters what is loaded.
 *
 * Log text is the container's own and untrusted: it is only ever rendered as
 * text nodes, with ANSI escapes removed and whitespace kept.
 */
export function LogViewer({
  containerID,
  nodeID,
  containerRef,
}: {
  containerID: string;
  nodeID: string | undefined;
  /** Name or short id, used only for the downloaded file's name. */
  containerRef: string;
}) {
  const [preset, setPreset] = useState<PresetID>("15m");
  const [range, setRange] = useState<LogRange>(() => presetRange("15m", new Date()));
  const [utc, setUTC] = useState(false);
  const [live, setLive] = useState(true);
  const [wrap, setWrap] = useState(false);
  const [search, setSearch] = useState("");
  const [matchIndex, setMatchIndex] = useState(0);

  const pages = useContainerLogRange(containerID, nodeID, range);
  const followable = canFollow(range);
  const follow = useContainerLogFollow(containerID, nodeID, FOLLOW_REPLAY_TAIL, followable && live);

  const historicalLines = useMemo(() => pagesToLines(pages.data?.pages ?? []), [pages.data]);
  const notices = useMemo(() => rangeNotices(pages.data?.pages ?? []), [pages.data]);
  // `base` is what a fresh follow connection's replay burst is deduped
  // against: the loaded range, or, after a pause, everything on screen.
  const [base, setBase] = useState<LogLine[]>([]);
  useEffect(() => { setBase(historicalLines); }, [historicalLines]);
  const lines = useMemo(
    () => (followable ? mergeLogLines(base, follow.lines) : historicalLines),
    [followable, base, follow.lines, historicalLines],
  );
  const shown = useMemo(() => filterLines(lines, search), [lines, search]);

  const { bodyRef, stuckToBottom, newSinceAway, jumpToBottom, beginPrepend } = useLogScroll(shown);
  const loadMore = () => {
    beginPrepend();
    void pages.fetchNextPage();
  };

  const choose = (next: LogRange, id: PresetID) => {
    setPreset(id);
    setRange(next);
    setLive(true);
  };

  const togglePause = () => {
    if (live) setBase(lines);
    setLive((l) => !l);
  };

  const matches = useMemo(() => findMatches(shown.map(safeLine), search.trim()), [shown, search]);
  useEffect(() => { setMatchIndex(0); }, [search]);
  const activeMatch: LogMatch | undefined = matches[matchIndex];
  const jumpToMatch = (delta: number) => {
    if (matches.length === 0) return;
    setMatchIndex((i) => (i + delta + matches.length) % matches.length);
  };

  const copyLoaded = () => {
    void navigator.clipboard.writeText(renderLogText(shown));
  };
  const downloadLoaded = () => {
    const blob = new Blob([renderLogText(shown)], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = logFileName(containerRef, shown.length);
    a.click();
    URL.revokeObjectURL(url);
  };

  const showInitialLoading = pages.isPending && lines.length === 0;
  const showInitialError = pages.error && lines.length === 0;

  return (
    <div className="flex flex-col gap-3">
      <RangePicker preset={preset} range={range} utc={utc} onUTC={setUTC} onChoose={choose} />

      <div className="flex flex-wrap items-center gap-2">
        {followable ? (
          <>
            <button
              type="button"
              onClick={togglePause}
              className={`rounded-lg px-3 py-1.5 text-xs font-medium transition-colors ${
                live ? "bg-primary text-white" : "border border-border text-text-muted hover:bg-surface-hover"
              }`}
            >
              {live ? "Pause" : "Follow"}
            </button>
            {live ? <FollowStatusBadge status={follow.status} reason={follow.reason} /> : null}
          </>
        ) : (
          <Badge tone="neutral">Historical range, not live</Badge>
        )}

        <div className="ml-auto flex items-center gap-1.5">
          <SearchBox
            value={search}
            onChange={setSearch}
            matchCount={matches.length}
            matchIndex={matchIndex}
            onNext={() => { jumpToMatch(1); }}
            onPrev={() => { jumpToMatch(-1); }}
          />
          <IconToggle active={wrap} onClick={() => { setWrap((w) => !w); }} label="Wrap lines" icon={WrapText} />
          <IconButton onClick={copyLoaded} label="Copy shown logs" icon={Copy} />
          <IconButton onClick={downloadLoaded} label="Download shown logs" icon={Download} />
        </div>
      </div>

      <RangeNoticeList notices={notices} utc={utc} />

      {showInitialLoading || showInitialError ? (
        <QueryState isPending={showInitialLoading} error={pages.error} onRetry={() => void pages.refetch()} rows={6} />
      ) : shown.length === 0 ? (
        <EmptyState
          art={emptyArt.logs}
          title={lines.length > 0 ? "No loaded line matches the search" : "No logs in this range"}
          description={
            lines.length > 0
              ? "Search looks through the lines loaded for this range. Load more, or widen the range."
              : followable && live
                ? "Nothing was written in this range. New lines appear here as the container writes them."
                : "Nothing was written to stdout or stderr in this range."
          }
          hint="Atlas reads the container's stdout and stderr through the Docker daemon, which keeps only what is still in its log file."
          compact
        />
      ) : (
        <div className="relative">
          {notices.more ? (
            <div className="mb-1.5 flex justify-center">
              <button
                type="button"
                onClick={loadMore}
                disabled={pages.isFetchingNextPage}
                className="rounded-md px-2.5 py-1 text-[11px] font-medium text-text-muted hover:bg-surface-hover hover:text-text disabled:opacity-50"
              >
                <ArrowUp size={11} className="mr-1 inline" />
                {pages.isFetchingNextPage ? "Loading…" : "Load more"}
              </button>
            </div>
          ) : (
            <p className="mb-1.5 text-center text-[11px] text-text-subtle">Start of the range reached.</p>
          )}

          <pre
            ref={bodyRef}
            className={`scroll-thin m-0 max-h-[70vh] min-h-[24rem] overflow-auto rounded-lg border border-border bg-bg p-3 font-mono text-xs leading-relaxed ${
              wrap ? "whitespace-pre-wrap break-all" : "whitespace-pre"
            }`}
          >
            {shown.map((line, i) => (
              <LogRow key={i} line={line} lineIndex={i} utc={utc} matches={matches} activeMatch={activeMatch} />
            ))}
          </pre>

          {!stuckToBottom && newSinceAway > 0 ? (
            <button
              type="button"
              onClick={jumpToBottom}
              className="absolute bottom-3 left-1/2 -translate-x-1/2 rounded-full bg-primary px-3 py-1.5 text-xs font-medium text-white shadow-lg transition-colors hover:bg-primary/90"
            >
              <ArrowDown size={11} className="mr-1 inline" />
              {newSinceAway} new log{newSinceAway === 1 ? "" : "s"}
            </button>
          ) : null}

          <p className="mt-1.5 text-[11px] text-text-subtle">
            Showing {shown.length} of {lines.length} loaded line{lines.length === 1 ? "" : "s"}. Times in {zoneLabel(utc)}.
          </p>
        </div>
      )}
    </div>
  );
}

/** The line as search sees it: the displayed text, not the raw escapes. */
function safeLine(line: LogLine): LogLine {
  return { ...line, message: displayMessage(line.message) };
}

/** Presets, a custom from/to in the shown zone, and the zone toggle. */
export function RangePicker({
  preset,
  range,
  utc,
  onUTC,
  onChoose,
  now = () => new Date(),
}: {
  preset: PresetID;
  range: LogRange;
  utc: boolean;
  onUTC: (utc: boolean) => void;
  onChoose: (range: LogRange, preset: PresetID) => void;
  now?: () => Date;
}) {
  const [from, setFrom] = useState(() => toInputValue(range.since, utc));
  const [to, setTo] = useState(() => (range.until ? toInputValue(range.until, utc) : ""));
  const [error, setError] = useState<string | null>(null);
  // Typed values follow the zone they are shown in.
  useEffect(() => {
    setFrom(toInputValue(range.since, utc));
    setTo(range.until ? toInputValue(range.until, utc) : "");
  }, [range, utc]);

  const apply = () => {
    const result = validateCustomRange(from, to, utc, now());
    if ("error" in result) {
      setError(result.error);
      return;
    }
    setError(null);
    onChoose(result.range, "custom");
  };

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-1.5" role="group" aria-label="Time range">
        <Clock size={13} aria-hidden="true" className="text-text-muted" />
        {PRESETS.map((p) => (
          <button
            key={p.id}
            type="button"
            aria-pressed={preset === p.id}
            onClick={() => {
              if (p.id === "custom") {
                onChoose(range, "custom");
                return;
              }
              setError(null);
              onChoose(presetRange(p.id, now()), p.id);
            }}
            className={`rounded-lg border px-2.5 py-1 text-xs transition-colors ${
              preset === p.id ? "border-primary bg-primary/10 text-primary" : "border-border text-text-muted hover:bg-surface-hover"
            }`}
          >
            {p.label}
          </button>
        ))}
        <label className="ml-auto flex items-center gap-1.5 text-xs text-text-muted">
          <input type="checkbox" checked={utc} onChange={(e) => { onUTC(e.target.checked); }} />
          Show UTC
        </label>
        <span className="text-xs text-text-subtle" aria-live="polite">Times in {zoneLabel(utc)}</span>
      </div>
      {preset === "custom" ? (
        <form
          className="flex flex-wrap items-end gap-2"
          noValidate
          onSubmit={(e) => { e.preventDefault(); apply(); }}
        >
          <label className="flex flex-col text-xs text-text-muted">
            From ({utc ? "UTC" : "local"})
            <input type="datetime-local" step={1} value={from} onChange={(e) => { setFrom(e.target.value); }}
              className="rounded-lg border border-border bg-bg px-2 py-1 text-xs text-text" />
          </label>
          <label className="flex flex-col text-xs text-text-muted">
            To ({utc ? "UTC" : "local"}; empty for now)
            <input type="datetime-local" step={1} value={to} onChange={(e) => { setTo(e.target.value); }}
              className="rounded-lg border border-border bg-bg px-2 py-1 text-xs text-text" />
          </label>
          <button type="submit" className="rounded-lg bg-primary px-3 py-1.5 text-xs font-medium text-white">Apply</button>
          <p aria-live="polite" className="w-full text-xs text-danger">{error}</p>
        </form>
      ) : null}
    </div>
  );
}

/** What the reader must know about the loaded range. */
export function RangeNoticeList({ notices, utc }: { notices: ReturnType<typeof rangeNotices>; utc: boolean }) {
  const items: { tone: "warning" | "info"; text: string }[] = [];
  if (notices.fallback) items.push({ tone: "info", text: "This server's agent is older; large ranges may be slow." });
  if (notices.scanLimited) items.push({ tone: "warning", text: "Scan limit reached: narrow the time range to see these logs." });
  if (notices.truncated) items.push({ tone: "info", text: "Some lines did not fit in one response; Load more shows the rest." });
  if (notices.availableSince) {
    items.push({ tone: "info", text: `Logs available only since ${formatLogTime(notices.availableSince, utc)}.` });
  }
  if (items.length === 0) return null;
  return (
    <ul className="flex flex-col gap-1">
      {items.map((n) => (
        <li key={n.text} role="status" className={`rounded-lg border px-3 py-1.5 text-xs ${
          n.tone === "warning" ? "border-warning/40 bg-warning/10 text-warning" : "border-border bg-surface-hover text-text-muted"
        }`}>
          {n.text}
        </li>
      ))}
    </ul>
  );
}

export function LogRow({
  line,
  lineIndex,
  utc,
  matches,
  activeMatch,
}: {
  line: LogLine;
  lineIndex: number;
  utc: boolean;
  matches: LogMatch[];
  activeMatch: LogMatch | undefined;
}) {
  const lineMatches = matches.filter((m) => m.lineIndex === lineIndex);
  const message = displayMessage(line.message);
  return (
    <div className={line.stream === "stderr" ? "text-danger" : "text-text"}>
      <time dateTime={line.time} className="mr-2 inline-block tabular-nums text-text-muted">
        {formatLogTime(line.time, utc)}
      </time>
      {lineMatches.length === 0 ? (
        message
      ) : (
        <HighlightedMessage message={message} matches={lineMatches} activeMatch={activeMatch} />
      )}
      {line.truncated ? <span className="ml-1 text-text-subtle">[line cut]</span> : null}
    </div>
  );
}

function HighlightedMessage({
  message,
  matches,
  activeMatch,
}: {
  message: string;
  matches: LogMatch[];
  activeMatch: LogMatch | undefined;
}) {
  const parts: React.ReactNode[] = [];
  let cursor = 0;
  matches.forEach((m, i) => {
    if (m.start > cursor) parts.push(message.slice(cursor, m.start));
    const isActive = activeMatch?.lineIndex === m.lineIndex && activeMatch.start === m.start;
    parts.push(
      <mark key={i} className={isActive ? "bg-primary text-white" : "bg-warning/40"}>
        {message.slice(m.start, m.end)}
      </mark>,
    );
    cursor = m.end;
  });
  if (cursor < message.length) parts.push(message.slice(cursor));
  return <>{parts}</>;
}

function SearchBox({
  value,
  onChange,
  matchCount,
  matchIndex,
  onNext,
  onPrev,
}: {
  value: string;
  onChange: (v: string) => void;
  matchCount: number;
  matchIndex: number;
  onNext: () => void;
  onPrev: () => void;
}) {
  return (
    <div className="flex items-center gap-1 rounded-lg border border-border px-2 py-1">
      <Search size={12} className="text-text-muted" />
      <input
        type="text"
        value={value}
        onChange={(e) => { onChange(e.target.value); }}
        placeholder="Filter loaded lines…"
        aria-label="Filter loaded lines"
        className="w-32 bg-transparent text-xs text-text outline-none placeholder:text-text-subtle sm:w-48"
      />
      {value ? (
        <span className="flex shrink-0 items-center gap-1 text-[11px] text-text-muted">
          {matchCount > 0 ? `${String(matchIndex + 1)}/${String(matchCount)}` : "0"}
          <button
            type="button"
            onClick={onPrev}
            disabled={matchCount === 0}
            aria-label="Previous match"
            className="rounded px-0.5 hover:bg-surface-hover disabled:opacity-40"
          >
            ↑
          </button>
          <button
            type="button"
            onClick={onNext}
            disabled={matchCount === 0}
            aria-label="Next match"
            className="rounded px-0.5 hover:bg-surface-hover disabled:opacity-40"
          >
            ↓
          </button>
        </span>
      ) : null}
    </div>
  );
}

function IconButton({
  onClick,
  label,
  icon: Icon,
}: {
  onClick: () => void;
  label: string;
  icon: typeof Copy;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      title={label}
      aria-label={label}
      className="rounded-lg border border-border p-1.5 text-text-muted hover:bg-surface-hover hover:text-text"
    >
      <Icon size={13} />
    </button>
  );
}

function IconToggle({
  active,
  onClick,
  label,
  icon: Icon,
}: {
  active: boolean;
  onClick: () => void;
  label: string;
  icon: typeof WrapText;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      title={label}
      aria-label={label}
      aria-pressed={active}
      className={`rounded-lg border p-1.5 transition-colors ${
        active ? "border-primary bg-primary/10 text-primary" : "border-border text-text-muted hover:bg-surface-hover"
      }`}
    >
      <Icon size={13} />
    </button>
  );
}

function FollowStatusBadge({ status, reason }: { status: FollowStatus; reason?: string | undefined }) {
  switch (status) {
    case "connecting":
      return <Badge tone="warning">connecting…</Badge>;
    case "open":
      return (
        <Badge tone="success" pulse>
          live
        </Badge>
      );
    case "ended":
      return (
        <span title={reason}>
          <Badge tone="neutral">stream ended</Badge>
        </span>
      );
    case "error":
      return (
        <span title={reason ?? "The live connection was lost. Resume to reconnect."}>
          <Badge tone="danger">disconnected</Badge>
        </span>
      );
  }
}

/**
 * All scrollTop adjustments to the log body live in one place, because two
 * independent effects racing to set the same property is how "load older"
 * and "auto-follow" used to fight each other.
 *
 * Two distinct operations share this hook:
 *
 * - Auto-follow: while the reader is at (or near) the bottom, new lines keep
 *   them there; scrolled away, new lines accumulate silently behind a
 *   "N new logs" affordance instead of yanking the viewport down.
 *   [jumpToBottom] is both that affordance's action and how the reader
 *   manually resumes following.
 *
 * - Prepend ("load older logs"): [beginPrepend] marks the *next* change to
 *   `lines` as older content added above, not new content — it is handled by
 *   exact height-delta compensation (see [compensateScrollTopForPrepend]),
 *   never by the auto-follow/indicator logic above, regardless of whether
 *   the reader happened to be at the bottom.
 *
 * The compensation runs in a layout effect so it lands before the browser
 * paints the taller container — a passive effect would let the wrong
 * position flash on screen first.
 */
function useLogScroll(lines: LogLine[]) {
  const ref = useRef<HTMLPreElement>(null);
  const [stuckToBottom, setStuckToBottom] = useState(true);
  const [newSinceAway, setNewSinceAway] = useState(0);
  const prevCount = useRef(lines.length);
  const pendingPrepend = useRef<{ scrollHeight: number; scrollTop: number } | null>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const onScroll = () => {
      const atBottom = isNearBottom(el.scrollHeight, el.scrollTop, el.clientHeight);
      setStuckToBottom(atBottom);
      if (atBottom) setNewSinceAway(0);
    };
    el.addEventListener("scroll", onScroll);
    return () => { el.removeEventListener("scroll", onScroll); };
  }, []);

  useLayoutEffect(() => {
    const el = ref.current;
    const added = lines.length - prevCount.current;
    prevCount.current = lines.length;
    if (!el) return;

    const prepend = pendingPrepend.current;
    if (prepend) {
      pendingPrepend.current = null;
      el.scrollTop = compensateScrollTopForPrepend(prepend.scrollTop, prepend.scrollHeight, el.scrollHeight);
      return;
    }

    if (added <= 0) return;
    if (stuckToBottom) {
      el.scrollTop = el.scrollHeight;
    } else {
      setNewSinceAway((n) => n + added);
    }
  }, [lines, stuckToBottom]);

  const jumpToBottom = () => {
    const el = ref.current;
    if (el) el.scrollTop = el.scrollHeight;
    setStuckToBottom(true);
    setNewSinceAway(0);
  };

  const beginPrepend = () => {
    const el = ref.current;
    if (el) pendingPrepend.current = { scrollHeight: el.scrollHeight, scrollTop: el.scrollTop };
  };

  return { bodyRef: ref, stuckToBottom, newSinceAway, jumpToBottom, beginPrepend };
}
