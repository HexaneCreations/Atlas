import { describe, expect, it } from "vitest";
import type { NodeAlertRule } from "../api/types";
import { applyOverride, globalNote, groupByArea, isCustomised } from "./alertRules";

function rule(overrides: Partial<NodeAlertRule> = {}): NodeAlertRule {
  return {
    key: "disk_pct", area: "resources", title: "Disk usage", description: "A filesystem is filling up.",
    default_severity: "warning", default_enabled: true, global_enabled: true, override: null,
    enabled: true, source: "global", ...overrides,
  };
}

describe("applyOverride", () => {
  it("resolves node, then global, then catalog, like the server", () => {
    expect(applyOverride(rule(), false)).toMatchObject({ override: false, enabled: false, source: "node" });
    const customised = rule({ override: false, enabled: false, source: "node" });
    expect(applyOverride(customised, null)).toMatchObject({ override: null, enabled: true, source: "global" });
    const offGlobally = rule({ global_enabled: false, enabled: false });
    expect(applyOverride(offGlobally, null)).toMatchObject({ enabled: false, source: "global" });
    const noGlobal = rule({ global_enabled: null, default_enabled: false });
    expect(applyOverride(noGlobal, null)).toMatchObject({ enabled: false, source: "catalog" });
  });

  it("enables a rule for one node that is off globally", () => {
    expect(applyOverride(rule({ global_enabled: false, enabled: false }), true)).toMatchObject({
      enabled: true, source: "node", global_enabled: false,
    });
  });
});

describe("groupByArea", () => {
  it("keeps the server's order and labels each area", () => {
    const groups = groupByArea([
      rule({ key: "node_down", area: "availability" }),
      rule({ key: "disk_pct" }),
      rule({ key: "ram_pct" }),
      rule({ key: "container_exit", area: "workloads" }),
    ]);
    expect(groups.map((g) => [g.label, g.rules.map((r) => r.key)])).toEqual([
      ["Availability", ["node_down"]],
      ["Resources", ["disk_pct", "ram_pct"]],
      ["Workloads", ["container_exit"]],
    ]);
  });
});

describe("badges and notes", () => {
  it("is customised only when the node sets its own value", () => {
    expect(isCustomised(rule())).toBe(false);
    expect(isCustomised(rule({ source: "catalog" }))).toBe(false);
    expect(isCustomised(rule({ source: "node", override: true }))).toBe(true);
  });

  it("notes a rule that is off globally", () => {
    expect(globalNote(rule())).toBeNull();
    expect(globalNote(rule({ global_enabled: false, enabled: false }))).toContain("enable it for this node only");
    expect(globalNote(rule({ global_enabled: false, enabled: true, override: true, source: "node" }))).toBe(
      "Off globally, on for this node only.",
    );
  });
});
