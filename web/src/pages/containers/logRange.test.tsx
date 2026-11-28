// A zone with a half-hour offset and no daylight saving, so local-time
// conversions are checked against something other than UTC.
process.env.TZ = "Asia/Kolkata";

import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { logRangeQuery } from "../../api/queries";
import type { ContainerLogsResponse, LogLine } from "../../api/types";
import { LogRow, RangeNoticeList, RangePicker } from "./LogViewer";
import {
  MAX_RANGE_MS, PRESETS, canFollow, displayMessage, filterLines, formatLogTime, fromInputValue, pagesToLines,
  presetRange, rangeNotices, toInputValue, validateCustomRange, zoneLabel,
} from "./logRangeModel";

const now = new Date("2026-03-01T12:00:00Z");

describe("presets", () => {
  it("end now, so they can be followed", () => {
    for (const p of PRESETS.filter((x) => x.id !== "custom")) {
      const r = presetRange(p.id as "15m", now);
      expect(r.until).toBeNull();
      expect(now.getTime() - new Date(r.since).getTime()).toBe(p.ms);
      expect(canFollow(r)).toBe(true);
    }
    expect(PRESETS.map((p) => p.id)).toEqual(["15m", "1h", "6h", "24h", "custom"]);
  });

  it("never offers more than the range cap", () => {
    expect(Math.max(...PRESETS.map((p) => p.ms))).toBeLessThanOrEqual(MAX_RANGE_MS);
  });
});

describe("time zones", () => {
  it("shows and reads times in local time or UTC", () => {
    expect(formatLogTime("2026-03-01T12:00:00.123456789Z", false)).toBe("2026-03-01 17:30:00.123");
    expect(formatLogTime("2026-03-01T12:00:00.123456789Z", true)).toBe("2026-03-01 12:00:00.123");
    expect(toInputValue("2026-03-01T12:00:00Z", false)).toBe("2026-03-01T17:30:00");
    expect(fromInputValue("2026-03-01T17:30", false)?.toISOString()).toBe("2026-03-01T12:00:00.000Z");
    expect(fromInputValue("2026-03-01T17:30:05", true)?.toISOString()).toBe("2026-03-01T17:30:05.000Z");
  });

  it("names the zone", () => {
    expect(zoneLabel(true)).toBe("UTC");
    expect(zoneLabel(false, now)).toMatch(/^Asia\/(Kolkata|Calcutta) \(UTC\+05:30\)$/);
  });

  it("refuses values that are not a time", () => {
    for (const v of ["", "yesterday", "2026-02-30T10:00", "2026-03-01 10:00", "2026-03-01T25:00"]) {
      expect(fromInputValue(v, false)).toBeNull();
    }
  });
});

describe("validateCustomRange", () => {
  it("accepts a past range and an open-ended one", () => {
    expect(validateCustomRange("2026-03-01T15:30", "2026-03-01T16:30", false, now)).toEqual({
      range: { since: "2026-03-01T10:00:00.000Z", until: "2026-03-01T11:00:00.000Z" },
    });
    const open = validateCustomRange("2026-03-01T15:30", "", false, now);
    expect(open).toEqual({ range: { since: "2026-03-01T10:00:00.000Z", until: null } });
    if ("range" in open) expect(canFollow(open.range)).toBe(true);
  });

  it("rejects reversed, future, too wide and malformed ranges", () => {
    const err = (from: string, to: string) => {
      const r = validateCustomRange(from, to, true, now);
      return "error" in r ? r.error : "";
    };
    expect(err("2026-03-01T11:00", "2026-03-01T10:00")).toMatch(/before the end/);
    expect(err("2026-03-01T11:00", "2026-03-01T11:00")).toMatch(/before the end/);
    expect(err("2026-03-01T13:00", "")).toMatch(/start is in the future/);
    expect(err("2026-03-01T10:00", "2026-03-01T14:00")).toMatch(/end is in the future/);
    expect(err("2026-02-27T10:00", "2026-03-01T10:00")).toMatch(/at most 24 hours/);
    expect(err("nonsense", "")).toMatch(/valid start/);
    expect(err("2026-03-01T10:00", "later")).toMatch(/valid end/);
  });

  it("treats a few seconds of skew as now, and a past end is never followed", () => {
    const r = validateCustomRange("2026-03-01T11:00", "2026-03-01T12:00:30", true, now);
    expect(r).toEqual({ range: { since: "2026-03-01T11:00:00.000Z", until: "2026-03-01T12:00:00.000Z" } });
    if ("range" in r) expect(canFollow(r.range)).toBe(false);
  });
});

describe("loaded lines", () => {
  const line = (time: string, message: string): LogLine => ({ time, stream: "stdout", message });

  it("filters case-insensitively by the text as shown", () => {
    const lines = [line("t1", "GET /health 200"), line("t2", "\u001b[31mERROR\u001b[0m db down"), line("t3", "ok")];
    expect(filterLines(lines, "error db").map((l) => l.time)).toEqual(["t2"]);
    expect(filterLines(lines, "  HEALTH ").map((l) => l.time)).toEqual(["t1"]);
    expect(filterLines(lines, "")).toHaveLength(3);
    expect(filterLines(lines, "31m")).toHaveLength(0);
  });

  it("orders pages oldest first and reports what the reader must know", () => {
    const page = (over: Partial<ContainerLogsResponse>): ContainerLogsResponse => ({ container_id: "c", lines: [], total: 0, ...over });
    const newest = page({ lines: [line("3", "c")], more: true, next_before: "3", fallback: true });
    const older = page({ lines: [line("1", "a"), line("2", "b")], more: false, available_since: "2026-03-01T06:00:00Z" });
    expect(pagesToLines([newest, older]).map((l) => l.message).join("")).toBe("abc");
    expect(rangeNotices([newest, older])).toEqual({
      availableSince: "2026-03-01T06:00:00Z", scanLimited: false, truncated: false, fallback: true, more: false,
    });
    expect(rangeNotices([newest]).more).toBe(true);
    expect(rangeNotices([page({ scan_limited: true, truncated: true })])).toMatchObject({ scanLimited: true, truncated: true });
  });

  it("builds the page request", () => {
    const q = new URLSearchParams(logRangeQuery({ since: "2026-03-01T10:00:00Z", until: null }, "n1", 500, "2026-03-01T11:00:00.5Z"));
    expect(Object.fromEntries(q)).toEqual({ since: "2026-03-01T10:00:00Z", limit: "500", node: "n1", before: "2026-03-01T11:00:00.5Z" });
    expect(new URLSearchParams(logRangeQuery({ since: "a", until: "b" }, undefined, 10)).get("until")).toBe("b");
  });
});

describe("hostile log lines", () => {
  const render = (message: string, truncated = false) =>
    renderToStaticMarkup(
      <pre>
        <LogRow line={{ time: "2026-03-01T12:00:00Z", stream: "stdout", message, truncated }} lineIndex={0} utc matches={[]} activeMatch={undefined} />
      </pre>,
    );

  it("renders markup as text", () => {
    const html = render(`<img src=x onerror=alert(1)><script>alert(2)</script>`);
    expect(html).not.toContain("<img");
    expect(html).not.toContain("<script>");
    expect(html).toContain("&lt;script&gt;");
  });

  it("removes ANSI escapes, shows other control characters, keeps whitespace", () => {
    expect(displayMessage("\u001b[1;31mred\u001b[0m \u001b]0;title\u0007done")).toBe("red done");
    expect(displayMessage("a\u0000b\u0008c")).toBe("a�b�c");
    expect(displayMessage("col1\tcol2   spaced")).toBe("col1\tcol2   spaced");
    expect(render("\u001b[32mgreen\u001b[0m")).not.toContain("\u001b");
  });

  it("marks a line the server cut and keeps a very long one whole", () => {
    const long = "x".repeat(20_000);
    expect(render(long, true)).toContain("[line cut]");
    expect(render(long)).toContain(long);
  });

  it("shows each line's timestamp", () => {
    expect(render("hello")).toContain(`<time dateTime="2026-03-01T12:00:00Z"`);
    expect(render("hello")).toContain("2026-03-01 12:00:00.000");
  });
});

describe("range notices and picker", () => {
  it("explains missing history, scan limits and the fallback", () => {
    const html = renderToStaticMarkup(
      <RangeNoticeList utc notices={{ availableSince: "2026-03-01T06:00:00Z", scanLimited: true, truncated: true, fallback: true, more: false }} />,
    );
    for (const text of ["Logs available only since 2026-03-01 06:00:00.000", "Scan limit reached", "agent is older; large ranges may be slow", "did not fit"]) {
      expect(html).toContain(text);
    }
    expect(renderToStaticMarkup(<RangeNoticeList utc={false} notices={{ availableSince: null, scanLimited: false, truncated: false, fallback: false, more: true }} />)).toBe("");
  });

  it("shows the presets, the zone and its toggle", () => {
    const html = renderToStaticMarkup(
      <RangePicker preset="custom" range={{ since: "2026-03-01T10:00:00Z", until: null }} utc={false}
        onUTC={() => undefined} onChoose={() => undefined} now={() => now} />,
    );
    for (const text of ["Last 15 minutes", "Last hour", "Last 6 hours", "Last 24 hours", "Custom", "Show UTC", "(UTC+05:30)", "2026-03-01T15:30:00", "empty for now"]) {
      expect(html).toContain(text);
    }
  });
});
