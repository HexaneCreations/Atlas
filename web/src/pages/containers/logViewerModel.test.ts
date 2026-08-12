import { describe, expect, it } from "vitest";
import type { LogLine } from "../../api/types";
import {
  canLoadOlder,
  compensateScrollTopForPrepend,
  containerLogsPath,
  dedupeLiveBurst,
  findMatches,
  isNearBottom,
  logFileName,
  mergeLogLines,
  nextTail,
  renderLogText,
} from "./logViewerModel";

function line(time: string, message: string, stream: "stdout" | "stderr" = "stdout"): LogLine {
  return { time, stream, message };
}

describe("isNearBottom", () => {
  it("is true within the threshold", () => {
    expect(isNearBottom(1000, 940, 40)).toBe(true); // 1000-940-40=20 < 48
  });

  it("is false past the threshold", () => {
    expect(isNearBottom(1000, 800, 40)).toBe(false); // 1000-800-40=160
  });
});

describe("dedupeLiveBurst", () => {
  it("drops a live burst that exactly replays the historical tail", () => {
    const historical = [line("1", "a"), line("2", "b"), line("3", "c")];
    const live = [line("2", "b"), line("3", "c"), line("4", "d")];
    expect(dedupeLiveBurst(historical, live)).toEqual([line("4", "d")]);
  });

  it("returns the whole burst when there is no overlap", () => {
    const historical = [line("1", "a")];
    const live = [line("2", "b")];
    expect(dedupeLiveBurst(historical, live)).toEqual(live);
  });

  it("handles an empty historical list", () => {
    const live = [line("1", "a")];
    expect(dedupeLiveBurst([], live)).toEqual(live);
  });

  it("distinguishes lines with identical messages but different timestreams", () => {
    const historical = [line("1", "heartbeat")];
    const live = [line("2", "heartbeat")]; // same text, different time: not a duplicate
    expect(dedupeLiveBurst(historical, live)).toEqual(live);
  });
});

describe("mergeLogLines", () => {
  it("is just historical when nothing live has arrived", () => {
    const historical = [line("1", "a")];
    expect(mergeLogLines(historical, [])).toEqual(historical);
  });

  it("appends only the non-duplicate live tail after historical", () => {
    const historical = [line("1", "a"), line("2", "b")];
    const live = [line("2", "b"), line("3", "c")];
    expect(mergeLogLines(historical, live)).toEqual([line("1", "a"), line("2", "b"), line("3", "c")]);
  });
});

describe("compensateScrollTopForPrepend", () => {
  // The exact scenario from the "load older logs" bug report: reader is
  // mid-scroll (not at the bottom), older content is prepended above them,
  // and the previously visible line must stay at the same pixel offset.
  it("shifts scrollTop by exactly the height the container grew by", () => {
    // 2000px of content added above a reader who was 400px down.
    expect(compensateScrollTopForPrepend(400, 3000, 5000)).toBe(2400);
  });

  it("is a no-op when nothing was actually prepended", () => {
    expect(compensateScrollTopForPrepend(400, 3000, 3000)).toBe(400);
  });

  it("keeps a reader who was at the very top pinned to the same content, not the top", () => {
    expect(compensateScrollTopForPrepend(0, 3000, 5000)).toBe(2000);
  });
});

describe("nextTail / canLoadOlder", () => {
  it("doubles up to the server cap", () => {
    expect(nextTail(200)).toBe(400);
    expect(nextTail(4000)).toBe(5000);
  });

  it("stops offering more once the cap is reached", () => {
    expect(canLoadOlder(5000)).toBe(false);
    expect(canLoadOlder(4999)).toBe(true);
  });
});

describe("findMatches", () => {
  const lines = [line("1", "starting up"), line("2", "ERROR: boom"), line("3", "error again, error")];

  it("is empty for an empty query", () => {
    expect(findMatches(lines, "")).toEqual([]);
  });

  it("matches case-insensitively", () => {
    const matches = findMatches(lines, "error");
    expect(matches).toHaveLength(3); // "ERROR" once, "error" twice on line 3
  });

  it("finds every occurrence within a single line", () => {
    const matches = findMatches(lines, "error").filter((m) => m.lineIndex === 2);
    expect(matches).toHaveLength(2);
    expect(matches[0]?.start).toBe(0);
    expect(matches[1]?.start).toBe(13);
  });
});

describe("renderLogText / logFileName", () => {
  it("renders one line per entry with its timestamp", () => {
    expect(renderLogText([line("2024-01-01T00:00:00Z", "hello")])).toBe("2024-01-01T00:00:00Z hello");
  });

  it("sanitises the container ref and includes the line count", () => {
    const name = logFileName("my app/thing", 42);
    expect(name).toMatch(/^my_app_thing-logs-42lines-.+\.log$/);
  });
});

describe("containerLogsPath", () => {
  it("encodes both the container id and the node", () => {
    expect(containerLogsPath("abc123", "linux-relay-poc-node")).toBe(
      "/containers/abc123/logs?node=linux-relay-poc-node",
    );
  });

  it("still produces a valid path with no node selected", () => {
    expect(containerLogsPath("abc123", undefined)).toBe("/containers/abc123/logs?node=");
  });
});
