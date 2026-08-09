import { useEffect, useRef, useState } from "react";
import { X } from "lucide-react";
import { useContainerLogs } from "../../api/queries";
import { useContainerLogFollow, type FollowStatus } from "../../api/logFollow";
import type { LogLine } from "../../api/types";
import { Card } from "../../components/Card";
import { EmptyState } from "../../components/EmptyState";
import { QueryState } from "../../components/QueryState";
import { Badge } from "../../components/Badge";
import { emptyArt } from "../../lib/assets";

/**
 * A container's log tail, with optional live follow over a WebSocket.
 *
 * Extracted from the page unchanged when the container operations centre was
 * built: it is the one piece of this feature with real transport machinery
 * behind it — a socket, a follow state machine, an auto-scroll that must not
 * fight the reader — and it does not belong inside a file about layout.
 */
export function LogViewer({ containerID, onClose }: { containerID: string; onClose: () => void }) {
  const [following, setFollowing] = useState(false);
  const logs = useContainerLogs(following ? null : containerID, 200);
  const follow = useContainerLogFollow(containerID, 200, following);

  const lines = following ? follow.lines : (logs.data?.lines ?? []);
  const bodyRef = useAutoScroll(lines);

  return (
    <Card className="mt-6">
      <div className="mb-4 flex items-center justify-between">
        <h3 className="text-card-title font-semibold text-text">Logs · {containerID}</h3>
        <div className="flex items-center gap-3">
          {following ? <FollowStatusBadge status={follow.status} reason={follow.reason} /> : null}
          <button
            type="button"
            onClick={() => { setFollowing((f) => !f); }}
            className={`rounded-lg px-3 py-1.5 text-xs font-medium transition-colors ${
              following ? "bg-primary text-white" : "border border-border text-text-muted hover:bg-surface-hover"
            }`}
          >
            {following ? "pause" : "follow"}
          </button>
          <button type="button" onClick={onClose} className="text-text-muted hover:text-text">
            <X size={16} />
          </button>
        </div>
      </div>

      {!following && (logs.isPending || logs.error) ? (
        <QueryState
          isPending={logs.isPending}
          error={logs.error}
          onRetry={() => void logs.refetch()}
          rows={4}
        />
      ) : lines.length > 0 ? (
        <pre
          ref={bodyRef}
          className="scroll-thin m-0 max-h-96 overflow-auto rounded-lg border border-border bg-bg p-3 font-mono text-xs leading-relaxed"
        >
          {lines.map((line, i) => (
            <div key={i} className={line.stream === "stderr" ? "text-danger" : "text-text"}>
              <span className="mr-2 inline-block min-w-[8ch] tabular-nums text-text-muted">
                {new Date(line.time).toLocaleTimeString()}
              </span>
              {line.message}
            </div>
          ))}
        </pre>
      ) : following && follow.status === "connecting" ? (
        <EmptyState
          art={emptyArt.logs}
          title="Waiting for output"
          description="The follow stream is connected and idle. Lines appear here the moment the container writes them."
          compact
        />
      ) : (
        <EmptyState
          art={emptyArt.logs}
          title="This container has produced no output"
          description="Nothing has been written to stdout or stderr. A container that logs to a file inside itself, or to an external collector, will look like this."
          hint="Atlas reads the container's stdout and stderr through the Docker daemon. It cannot read log files inside the filesystem."
          compact
        />
      )}
    </Card>
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
        <span title={reason}>
          <Badge tone="danger">disconnected</Badge>
        </span>
      );
  }
}

function useAutoScroll(lines: LogLine[]) {
  const ref = useRef<HTMLPreElement>(null);
  const stuckToBottom = useRef(true);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const onScroll = () => {
      stuckToBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48;
    };
    el.addEventListener("scroll", onScroll);
    return () => { el.removeEventListener("scroll", onScroll); };
  }, []);

  useEffect(() => {
    const el = ref.current;
    if (el && stuckToBottom.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [lines]);

  return ref;
}
