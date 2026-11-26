import { isValidElement, type ReactElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import { formatDateTime } from "../../format";
import { AlertMuteView, MutedBadge } from "./AlertMute";
import { MUTED_INDEFINITELY, muteLabel } from "./muteModel";

const END = "2026-01-02T03:04:00Z";

function elements(node: ReactNode): ReactElement<Record<string, unknown>>[] {
  if (Array.isArray(node)) return node.flatMap(elements);
  if (!isValidElement<Record<string, unknown>>(node)) return [];
  return [node, ...elements(node.props.children as ReactNode)];
}

describe("muteLabel", () => {
  it("follows the API's rule", () => {
    expect(muteLabel(null)).toBeNull();
    expect(MUTED_INDEFINITELY).toBe("9999-01-01T00:00:00Z");
    expect(muteLabel("9999-01-01T00:00:00Z")).toBe("until unmuted");
    expect(muteLabel(END)).toBe(`muted until ${formatDateTime(END)}`);
    expect(muteLabel("9998-01-01T00:00:00Z")).toContain("muted until");
  });
});

describe("AlertMuteView", () => {
  const view = (mutedUntil: string | null, onSet = vi.fn(), pending = false) =>
    ({ onSet, tree: AlertMuteView({ mutedUntil, pending, error: null, onSet }) });
  const buttons = (tree: ReactNode) => elements(tree).filter((e) => typeof e.props.onClick === "function");

  it("offers every duration on a node that is not muted", () => {
    const { tree, onSet } = view(null);
    expect(buttons(tree).map((b) => b.props.children)).toEqual(["1 hour", "4 hours", "24 hours", "7 days", "Until I unmute"]);
    for (const b of buttons(tree)) (b.props.onClick as () => void)();
    expect(onSet.mock.calls.map((c) => c[0] as string)).toEqual(["1h", "4h", "24h", "7d", "until_unmuted"]);
  });

  it("offers only Unmute on a muted node, and says until when", () => {
    const { tree, onSet } = view(MUTED_INDEFINITELY);
    expect(buttons(tree).map((b) => b.props.children)).toEqual(["Unmute"]);
    (buttons(tree)[0]?.props.onClick as () => void)();
    expect(onSet).toHaveBeenCalledWith(null);
    expect(renderToStaticMarkup(tree)).toContain("until unmuted");
    expect(renderToStaticMarkup(view(END).tree)).toContain(`muted until ${formatDateTime(END)}`);
  });

  it("cannot be used twice at once", () => {
    for (const until of [null, END]) {
      expect(buttons(view(until, vi.fn(), true).tree).every((b) => b.props.disabled === true)).toBe(true);
    }
  });

  it("announces an error", () => {
    const html = renderToStaticMarkup(<AlertMuteView mutedUntil={null} pending={false} error="no permission" onSet={vi.fn()} />);
    expect(html).toMatch(/aria-live="polite"[^>]*>no permission/);
  });
});

describe("MutedBadge", () => {
  it("shows only on a muted node, and only to a caller who may mute", () => {
    const html = (mutedUntil: string | null, visible: boolean) => renderToStaticMarkup(<MutedBadge mutedUntil={mutedUntil} visible={visible} />);
    expect(html(END, true)).toContain("Muted");
    expect(html(END, true)).toContain(`title="muted until ${formatDateTime(END)}"`);
    expect(html(null, true)).toBe("");
    expect(html(END, false)).toBe("");
  });
});
