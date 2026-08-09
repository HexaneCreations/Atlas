import { describe, expect, it } from "vitest";
import type { GraphNode, Service } from "../../api/types";
import { joinServices, ownHealth } from "./useServiceTable";

function unit(over: Partial<Service> = {}): Service {
  return {
    name: "app.service", active_state: "active", sub_state: "running",
    enabled: true, failed: false, running: true, restart_count: 0,
    ...over,
  };
}

function graphNode(over: Partial<GraphNode> = {}): GraphNode {
  return {
    id: "app.service", type: "service", health: "healthy",
    known: true, dependencies: 0, dependents: 0,
    ...over,
  };
}

describe("own health", () => {
  // The fallback used when the dependency graph is unavailable. It must never
  // report `degraded`: that verdict requires knowing what a unit depends on,
  // and claiming it without the graph would be invention.
  it("never reports degraded without the graph", () => {
    const states: Service["active_state"][] = [
      "active", "inactive", "failed", "activating", "deactivating",
    ];
    for (const active_state of states) {
      expect(ownHealth(unit({ active_state }))).not.toBe("degraded");
    }
  });

  it("maps systemd states onto health", () => {
    expect(ownHealth(unit({ active_state: "failed", failed: true }))).toBe("failed");
    expect(ownHealth(unit({ active_state: "active" }))).toBe("healthy");
    expect(ownHealth(unit({ active_state: "activating" }))).toBe("healthy");
    expect(ownHealth(unit({ active_state: "inactive" }))).toBe("inactive");
  });

  // The `failed` flag and the active state can disagree across systemd
  // versions; either one failing means failed.
  it("treats the failed flag as authoritative", () => {
    expect(ownHealth(unit({ active_state: "inactive", failed: true }))).toBe("failed");
  });
});

describe("joining units with the graph", () => {
  it("prefers the graph's propagated health over the unit's own state", () => {
    const rows = joinServices(
      [unit({ name: "app.service", active_state: "active" })],
      [graphNode({ id: "app.service", health: "degraded", failed_dependencies: ["db.service"] })],
    );

    expect(rows[0]?.health).toBe("degraded");
    expect(rows[0]?.failedDependencies).toEqual(["db.service"]);
  });

  // Without the graph the page must still render, using each unit's own state
  // and claiming nothing about dependencies.
  it("falls back to own health when the graph is unavailable", () => {
    const rows = joinServices([unit({ active_state: "failed", failed: true })], undefined);

    expect(rows[0]?.health).toBe("failed");
    expect(rows[0]?.failedDependencies).toEqual([]);
    expect(rows[0]?.node).toBeUndefined();
  });

  it("falls back for a unit the graph does not contain", () => {
    const rows = joinServices(
      [unit({ name: "new.service", active_state: "active" })],
      [graphNode({ id: "other.service" })],
    );

    expect(rows[0]?.health).toBe("healthy");
    expect(rows[0]?.dependencies).toBe(0);
  });

  it("carries degree counts from the graph", () => {
    const rows = joinServices(
      [unit()],
      [graphNode({ dependencies: 7, dependents: 5 })],
    );

    expect(rows[0]?.dependencies).toBe(7);
    expect(rows[0]?.dependents).toBe(5);
  });
});
