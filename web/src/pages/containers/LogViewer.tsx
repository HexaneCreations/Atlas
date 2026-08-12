import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { ArrowDown, ArrowUp, Copy, Download, Search, WrapText } from "lucide-react";
import { useContainerLogs } from "../../api/queries";
import { useContainerLogFollow, type FollowStatus } from "../../api/logFollow";
import type { LogLine } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { QueryState } from "../../components/QueryState";
import { Badge } from "../../components/Badge";
import { emptyArt } from "../../lib/assets";
import {
  FOLLOW_REPLAY_TAIL, canLoadOlder, compensateScrollTopForPrepend, findMatches, isNearBottom,
  logFileName, mergeLogLines, nextTail, renderLogText, type LogMatch,
} from "./logViewerModel";

const INITIAL_TAIL = 200;

/**
 * A container's logs: history fetched once up front, a live follow appended
 * after it — never one replacing the other — plus search, wrap, copy, and
 * download over whatever is currently loaded.
 *
 * Owns the transport (the follow socket, the auto-scroll state machine) the
 * same way it always has; [ContainerLogsPage] only supplies the surrounding
 * page chrome.
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
  const [live, setLive] = useState(true);
  const [tail, setTail] = useState(INITIAL_TAIL);
  const [wrap, setWrap] = useState(false);
  const [search, setSearch] = useState("");
  const [matchIndex, setMatchIndex] = useState(0);

  const historical = useContainerLogs(containerID, nodeID, tail);
  const follow = useContainerLogFollow(containerID, nodeID, FOLLOW_REPLAY_TAIL, live);

  // `base` is what a fresh follow connection's replay burst gets deduped
  // against. It starts as the REST tail fetch and is replaced wholesale by a
  // bigger fetch on "load older" (see the effect below) — a fresh Docker tail
  // read is always a superset of what came before, so replacing rather than
  // merging is correct, not lossy.
  const historicalLines = useMemo(() => historical.data?.lines ?? [], [historical.data]);
  const [base, setBase] = useState<LogLine[]>([]);
  useEffect(() => { setBase(historicalLines); }, [historicalLines]);

  const lines = useMemo(() => mergeLogLines(base, follow.lines), [base, follow.lines]);

  const { bodyRef, stuckToBottom, newSinceAway, jumpToBottom, beginPrepend } = useLogScroll(lines);
  const loadingOlder = historical.isFetching;
  const loadOlder = () => {
    beginPrepend();
    setTail(nextTail(tail));
  };

  // Pausing freezes the merged view as the new base *before* the follow
  // connection tears down, so resuming (a fresh connection, whose own
  // internal buffer always restarts empty) dedupes its replay burst against
  // everything already on screen instead of discarding it back to just the
  // original historical fetch.
  const togglePause = () => {
    if (live) setBase(lines);
    setLive((l) => !l);
  };

  const matches = useMemo(() => findMatches(lines, search), [lines, search]);
  useEffect(() => { setMatchIndex(0); }, [search]);
  const activeMatch: LogMatch | undefined = matches[matchIndex];

  const jumpToMatch = (delta: number) => {
    if (matches.length === 0) return;
    setMatchIndex((i) => (i + delta + matches.length) % matches.length);
  };

  const copyLoaded = () => {
    void navigator.clipboard.writeText(renderLogText(lines));
  };

  const downloadLoaded = () => {
    const blob = new Blob([renderLogText(lines)], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = logFileName(containerRef, lines.length);
    a.click();
    URL.revokeObjectURL(url);
  };

  const showInitialLoading = historical.isPending && lines.length === 0;
  const showInitialError = historical.error && lines.length === 0;

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <button
          type="button"
          onClick={togglePause}
          className={`rounded-lg px-3 py-1.5 text-xs font-medium transition-colors ${
            live ? "bg-primary text-white" : "border border-border text-text-muted hover:bg-surface-hover"
          }`}
        >
          {live ? "Pause" : "Resume"}
        </button>
        {live ? <FollowStatusBadge status={follow.status} reason={follow.reason} /> : null}

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
          <IconButton onClick={copyLoaded} label="Copy loaded logs" icon={Copy} />
          <IconButton onClick={downloadLoaded} label="Download loaded logs" icon={Download} />
        </div>
      </div>

      {showInitialLoading || showInitialError ? (
        <QueryState
          isPending={showInitialLoading}
          error={historical.error}
          onRetry={() => void historical.refetch()}
          rows={6}
        />
      ) : lines.length === 0 ? (
        <EmptyState
          art={emptyArt.logs}
          title={live && follow.status === "connecting" ? "Waiting for output" : "This container has produced no output"}
          description={
            live && follow.status === "connecting"
              ? "The follow stream is connected and idle. Lines appear here the moment the container writes them."
              : "Nothing has been written to stdout or stderr."
          }
          hint="Atlas reads the container's stdout and stderr through the Docker daemon. It cannot read log files inside the filesystem."
          compact
        />
      ) : (
        <div className="relative">
          {canLoadOlder(tail) ? (
            <div className="mb-1.5 flex justify-center">
              <button
                type="button"
                onClick={loadOlder}
                disabled={loadingOlder}
                className="rounded-md px-2.5 py-1 text-[11px] font-medium text-text-muted hover:bg-surface-hover hover:text-text disabled:opacity-50"
              >
                <ArrowUp size={11} className="mr-1 inline" />
                {loadingOlder ? "Loading…" : "Load older logs"}
              </button>
            </div>
          ) : (
            <p className="mb-1.5 text-center text-[11px] text-text-subtle">
              No older logs available — Atlas serves up to {String(tail)} lines
            </p>
          )}

          <pre
            ref={bodyRef}
            className={`scroll-thin m-0 max-h-[70vh] min-h-[24rem] overflow-auto rounded-lg border border-border bg-bg p-3 font-mono text-xs leading-relaxed ${
              wrap ? "whitespace-pre-wrap break-all" : "whitespace-pre"
            }`}
          >
            {lines.map((line, i) => (
              <LogRow key={i} line={line} lineIndex={i} matches={matches} activeMatch={activeMatch} />
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
            Showing {lines.length} line{lines.length === 1 ? "" : "s"} — not necessarily the container's full
            history.
          </p>
        </div>
      )}
    </div>
  );
}

function LogRow({
  line,
  lineIndex,
  matches,
  activeMatch,
}: {
  line: LogLine;
  lineIndex: number;
  matches: LogMatch[];
  activeMatch: LogMatch | undefined;
}) {
  const lineMatches = matches.filter((m) => m.lineIndex === lineIndex);

  return (
    <div className={line.stream === "stderr" ? "text-danger" : "text-text"}>
      <span className="mr-2 inline-block min-w-[8ch] tabular-nums text-text-muted">
        {new Date(line.time).toLocaleTimeString()}
      </span>
      {lineMatches.length === 0 ? (
        line.message
      ) : (
        <HighlightedMessage message={line.message} matches={lineMatches} activeMatch={activeMatch} />
      )}
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
        placeholder="Search loaded logs…"
        aria-label="Search loaded logs"
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
