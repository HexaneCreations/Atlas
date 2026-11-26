import { isValidElement, type ReactElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import type { NodeAlertRule } from "../../api/types";
import { AlertRulesView } from "./AlertRules";

type El = ReactElement<Record<string, unknown>>;

function rule(overrides: Partial<NodeAlertRule> = {}): NodeAlertRule {
  return {
    key: "disk_pct", area: "resources", title: "Disk usage", description: "A filesystem is filling up.",
    default_severity: "warning", default_enabled: true, global_enabled: true, override: null,
    enabled: true, source: "global", ...overrides,
  };
}

/** Expands function components so their output can be inspected. */
function elements(node: ReactNode): El[] {
  if (Array.isArray(node)) return node.flatMap(elements);
  if (!isValidElement<Record<string, unknown>>(node)) return [];
  if (typeof node.type === "function") {
    return elements((node.type as (p: Record<string, unknown>) => ReactNode)(node.props));
  }
  return [node, ...elements(node.props.children as ReactNode)];
}

const RULES: NodeAlertRule[] = [
  rule({ key: "node_down", area: "availability", title: "Node unreachable" }),
  rule({ key: "disk_pct", override: false, enabled: false, source: "node" }),
  rule({ key: "ram_pct", title: "Memory usage", global_enabled: false, enabled: false }),
];

function view(rules = RULES, pendingKey: string | null = null) {
  const onChange = vi.fn();
  const tree = AlertRulesView({ rules, pendingKey, error: null, onChange });
  const all = elements(tree);
  const switches = all.filter((e) => e.props.role === "switch");
  const resets = all.filter((e) => typeof e.props["aria-label"] === "string" && e.props["aria-label"].startsWith("Reset "));
  return { tree, onChange, switches, resets };
}

describe("AlertRulesView", () => {
  it("groups rules by area with a labelled switch, a badge and a description each", () => {
    const html = renderToStaticMarkup(view().tree);
    for (const text of ["Availability", "Resources", "Node unreachable", "Disk usage", "A filesystem is filling up.", "customised", "inherited"]) {
      expect(html).toContain(text);
    }
    const { switches } = view();
    expect(switches.map((s) => s.props.checked)).toEqual([true, false, false]);
    for (const s of switches) expect(s.props["aria-describedby"]).toMatch(/^alert-rule-.+-description$/);
  });

  it("toggles to an override and resets only a customised rule", () => {
    const { switches, resets, onChange } = view();
    (switches[0]?.props.onChange as (e: { target: { checked: boolean } }) => void)({ target: { checked: false } });
    expect(onChange).toHaveBeenLastCalledWith("node_down", false);
    expect(resets.map((r) => r.props.disabled)).toEqual([true, false, true]);
    (resets[1]?.props.onClick as () => void)();
    expect(onChange).toHaveBeenLastCalledWith("disk_pct", null);
  });

  it("lets a rule that is off globally be turned on for this node only", () => {
    const { switches, onChange, tree } = view();
    expect(renderToStaticMarkup(tree)).toContain("Off globally. Turn it on to enable it for this node only.");
    (switches[2]?.props.onChange as (e: { target: { checked: boolean } }) => void)({ target: { checked: true } });
    expect(onChange).toHaveBeenLastCalledWith("ram_pct", true);

    const enabledHere = rule({ key: "ram_pct", title: "Memory usage", global_enabled: false, override: true, enabled: true, source: "node" });
    const after = view([enabledHere]);
    expect(after.switches[0]?.props.checked).toBe(true);
    expect(after.resets[0]?.props.disabled).toBe(false);
    expect(renderToStaticMarkup(after.tree)).toContain("Off globally, on for this node only.");
  });

  it("disables the row being saved", () => {
    const { switches } = view(RULES, "disk_pct");
    expect(switches.map((s) => s.props.disabled)).toEqual([false, true, false]);
  });
});
