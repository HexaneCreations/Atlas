import type { NodeAlertRule } from "../api/types";

const AREA_LABEL: Record<string, string> = {
  availability: "Availability",
  resources: "Resources",
  workloads: "Workloads",
};

/** The rule as the server will resolve it once override is stored
 *  (null clears it): the node's value, else the global one, else the
 *  catalog default. */
export function applyOverride(rule: NodeAlertRule, override: boolean | null): NodeAlertRule {
  if (override !== null) return { ...rule, override, enabled: override, source: "node" };
  if (rule.global_enabled !== null) return { ...rule, override: null, enabled: rule.global_enabled, source: "global" };
  return { ...rule, override: null, enabled: rule.default_enabled, source: "catalog" };
}

export interface RuleGroup {
  area: string;
  label: string;
  rules: NodeAlertRule[];
}

/** Rules grouped by area, in the order the server lists them. */
export function groupByArea(rules: NodeAlertRule[]): RuleGroup[] {
  const groups: RuleGroup[] = [];
  for (const rule of rules) {
    let group = groups.find((g) => g.area === rule.area);
    if (!group) {
      group = { area: rule.area, label: AREA_LABEL[rule.area] ?? rule.area, rules: [] };
      groups.push(group);
    }
    group.rules.push(rule);
  }
  return groups;
}

export function isCustomised(rule: NodeAlertRule): boolean {
  return rule.source === "node";
}

/** A note for a rule the global default turns off, or null. */
export function globalNote(rule: NodeAlertRule): string | null {
  if (rule.global_enabled !== false) return null;
  return rule.enabled ? "Off globally, on for this node only." : "Off globally. Turn it on to enable it for this node only.";
}
