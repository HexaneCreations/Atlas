import { useEffect, useRef, useState } from "react";
import type { LogLine } from "./types";

/**
 * Live-follows one container's log stream over WebSocket.
 *
 * This is not a TanStack Query: a query fetches and caches an answer, and
 * following is a standing subscription with no single answer to cache — the
 * two models don't fit. The connection lifecycle instead lives in a plain
 * effect, scoped to the container being watched.
 */

export type FollowStatus = "connecting" | "open" | "ended" | "error";

export interface FollowState {
  status: FollowStatus;
  lines: LogLine[];
  /** Set when status is "ended" or "error"; the server's own explanation. */
  reason?: string;
}

/**
 * Caps how many lines are kept in the browser. A container producing a line a
 * millisecond would otherwise grow this tab's memory without bound — the same
 * reasoning behind every cardinality limit on the server side, applied here
 * because the server cannot protect a client from itself.
 */
const MAX_BUFFERED_LINES = 2000;

/**
 * Mirrors the server's close reason for its own session-length bound (see
 * maxFollowDuration in internal/api/v1/containers.go). A close for this
 * reason is not the stream ending — it is Atlas's own housekeeping — so the
 * client reconnects immediately rather than reporting an ending to the
 * operator who is still watching.
 */
const SESSION_BOUND_REASON = "closing";

/** Follows containerID's logs on nodeID while enabled is true. */
export function useContainerLogFollow(
  containerID: string | null,
  nodeID: string | undefined,
  tail: number,
  enabled: boolean,
): FollowState {
  const [state, setState] = useState<FollowState>({ status: "connecting", lines: [] });
  // Mutable alongside the state so onmessage can append without depending on
  // the previous render's closure — a WebSocket callback outlives renders.
  const linesRef = useRef<LogLine[]>([]);

  useEffect(() => {
    if (!enabled || !containerID) {
      setState({ status: "connecting", lines: [] });
      return;
    }

    let cancelled = false;
    let socket: WebSocket | undefined;

    const connect = () => {
      if (cancelled) return;

      linesRef.current = [];
      setState({ status: "connecting", lines: [] });

      const url = new URL(
        `/api/v1/containers/${encodeURIComponent(containerID)}/logs/follow`,
        window.location.href,
      );
      url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
      url.searchParams.set("tail", String(tail));
      url.searchParams.set("node", nodeID ?? "");

      socket = new WebSocket(url);

      socket.onopen = () => {
        if (!cancelled) setState((s) => ({ ...s, status: "open" }));
      };

      socket.onmessage = (event) => {
        if (cancelled || typeof event.data !== "string") return;
        let line: LogLine;
        try {
          line = JSON.parse(event.data) as LogLine;
        } catch {
          // A malformed frame is not this hook's problem to solve. Dropping
          // one line beats tearing down an otherwise-healthy connection.
          return;
        }
        const next = [...linesRef.current, line].slice(-MAX_BUFFERED_LINES);
        linesRef.current = next;
        setState({ status: "open", lines: next });
      };

      socket.onclose = (event) => {
        if (cancelled) return;
        if (event.code === 1000 && event.reason === SESSION_BOUND_REASON) {
          connect();
          return;
        }
        const status: FollowStatus = event.code === 1000 ? "ended" : "error";
        setState((s) =>
          event.reason ? { status, lines: s.lines, reason: event.reason } : { status, lines: s.lines },
        );
      };
      // onerror carries no useful detail beyond what the close event that
      // always follows it already provides.
    };

    connect();

    return () => {
      cancelled = true;
      socket?.close(1000, "view closed");
    };
  }, [containerID, nodeID, tail, enabled]);

  return state;
}
