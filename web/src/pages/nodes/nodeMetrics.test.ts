import { describe, expect, it } from "vitest";
import type { LatestValue } from "../../api/types";
import { readVitals } from "./nodeMetrics";

function value(
  metric: string,
  v: number,
  labels?: Record<string, string>,
): LatestValue {
  return {
    metric,
    value: v,
    unit: "percent",
    kind: "gauge",
    collector_id: "system",
    time: "2026-01-01T00:00:00Z",
    ...(labels ? { labels } : {}),
  };
}

describe("reporting state", () => {
  // A node that stopped reporting returns nothing at all. Rendering zeroes for
  // it would show an idle machine rather than an unreachable one.
  it("reports not-reporting for an empty value set", () => {
    const vitals = readVitals([]);
    expect(vitals.reporting).toBe(false);
    expect(vitals.cpu).toBeUndefined();
    expect(vitals.filesystems).toHaveLength(0);
    expect(vitals.interfaces).toHaveLength(0);
  });

  it("reports reporting once any value exists", () => {
    expect(readVitals([value("system.cpu.usage", 12)]).reporting).toBe(true);
  });
});

describe("scalar and labelled metrics", () => {
  // A metric name appears both unlabelled (the host total) and labelled (per
  // device). Reading the wrong one reports one disk's usage as the host's.
  it("reads the unlabelled value, not a labelled one sharing the name", () => {
    const vitals = readVitals([
      value("system.cpu.usage", 42),
      value("system.cpu.core.usage", 99, { core: "0" }),
    ]);

    expect(vitals.cpu).toBe(42);
  });

  it("selects load averages by window label", () => {
    const vitals = readVitals([
      value("system.load.average", 1.5, { window: "1m" }),
      value("system.load.average", 2.5, { window: "5m" }),
      value("system.load.average", 3.5, { window: "15m" }),
    ]);

    expect(vitals.load1).toBe(1.5);
    expect(vitals.load5).toBe(2.5);
    expect(vitals.load15).toBe(3.5);
  });

  // Core labels are numeric strings, and lexical ordering puts "10" before
  // "2" — which draws the per-core bars in the wrong order.
  it("orders cores numerically", () => {
    const vitals = readVitals([
      value("system.cpu.core.usage", 1, { core: "10" }),
      value("system.cpu.core.usage", 2, { core: "2" }),
      value("system.cpu.core.usage", 3, { core: "1" }),
    ]);

    expect(vitals.coreUsage.map((c) => c.core)).toEqual(["1", "2", "10"]);
  });
});

describe("filesystems", () => {
  function disk(metric: string, v: number, mountpoint: string): LatestValue {
    return value(metric, v, { mountpoint, device: "/dev/disk1", fstype: "apfs" });
  }

  it("joins the separate capacity metrics into one filesystem", () => {
    const vitals = readVitals([
      disk("system.disk.usage", 90, "/"),
      disk("system.disk.total", 1000, "/"),
      disk("system.disk.free", 100, "/"),
      disk("system.disk.inodes.usage", 12, "/"),
    ]);

    expect(vitals.filesystems).toHaveLength(1);
    expect(vitals.filesystems[0]).toMatchObject({
      mountpoint: "/", usedPercent: 90, total: 1000, free: 100, inodesPercent: 12,
    });
  });

  // APFS presents one container as several synthetic volumes with identical
  // capacity. Counting mounts reported the same disk six times.
  it("deduplicates volumes sharing a capacity signature", () => {
    const vitals = readVitals([
      disk("system.disk.usage", 94, "/"),
      disk("system.disk.total", 500, "/"),
      disk("system.disk.free", 30, "/"),
      disk("system.disk.usage", 94, "/System/Volumes/Update/mnt1"),
      disk("system.disk.total", 500, "/System/Volumes/Update/mnt1"),
      disk("system.disk.free", 30, "/System/Volumes/Update/mnt1"),
    ]);

    expect(vitals.filesystems).toHaveLength(1);
    // The shortest mountpoint is the readable name for the shared pool.
    expect(vitals.filesystems[0]?.mountpoint).toBe("/");
  });

  // A node is only as healthy as its worst filesystem.
  it("surfaces the fullest filesystem as the headline disk", () => {
    const vitals = readVitals([
      disk("system.disk.usage", 20, "/data"),
      disk("system.disk.total", 100, "/data"),
      disk("system.disk.usage", 95, "/"),
      disk("system.disk.total", 200, "/"),
    ]);

    expect(vitals.disk?.mountpoint).toBe("/");
    expect(vitals.disk?.usedPercent).toBe(95);
  });
});

describe("network interfaces", () => {
  it("joins rx, tx and error counters per interface", () => {
    const vitals = readVitals([
      value("system.network.rx.bytes", 100, { interface: "en0" }),
      value("system.network.tx.bytes", 200, { interface: "en0" }),
      value("system.network.rx.errors", 1, { interface: "en0" }),
    ]);

    expect(vitals.interfaces).toHaveLength(1);
    expect(vitals.interfaces[0]).toMatchObject({ name: "en0", rx: 100, tx: 200, rxErrors: 1 });
  });

  // An idle loopback should not head the list.
  it("orders interfaces by throughput", () => {
    const vitals = readVitals([
      value("system.network.rx.bytes", 0, { interface: "lo0" }),
      value("system.network.rx.bytes", 5000, { interface: "en0" }),
    ]);

    expect(vitals.interfaces[0]?.name).toBe("en0");
  });

  it("sums throughput across every interface", () => {
    const vitals = readVitals([
      value("system.network.rx.bytes", 100, { interface: "en0" }),
      value("system.network.rx.bytes", 50, { interface: "utun0" }),
    ]);

    expect(vitals.netRx).toBe(150);
  });
});

describe("workload counts", () => {
  it("reads container, port and process totals from node-scoped metrics", () => {
    const vitals = readVitals([
      value("docker.containers.total", 15),
      value("docker.containers.count", 3, { state: "running" }),
      value("port.listening.total", 28),
      value("process.total", 502),
      value("process.zombies", 0),
    ]);

    expect(vitals.containersTotal).toBe(15);
    expect(vitals.containersRunning).toBe(3);
    expect(vitals.portsListening).toBe(28);
    expect(vitals.processesTotal).toBe(502);
    expect(vitals.zombies).toBe(0);
  });

  // A host without Docker reports no container metrics at all, which must
  // stay undefined rather than becoming zero — "no Docker" and "no containers"
  // are different facts.
  it("leaves container counts undefined when Docker is absent", () => {
    const vitals = readVitals([value("system.cpu.usage", 10)]);
    expect(vitals.containersTotal).toBeUndefined();
    expect(vitals.containersRunning).toBeUndefined();
  });
});
