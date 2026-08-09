import { describe, expect, it } from "vitest";
import { computeFindings, type Inputs } from "./findings";
import type { Container, Mount, Node, Port, Process, Service } from "../../api/types";

/**
 * The findings engine decides what the Overview claims needs attention, and
 * every rule in it is a claim about somebody's infrastructure. These tests pin
 * the ones that have already been wrong once, plus the coverage behaviour that
 * keeps an all-clear honest.
 */

function inputs(over: Partial<Inputs> = {}): Inputs {
  return {
    nodes: [],
    services: { data: [], error: null },
    containers: { data: [], error: null },
    mounts: { data: [], error: null },
    ports: { data: [], error: null },
    processes: { data: [], error: null },
    cron: { data: undefined, error: null },
    collectors: undefined,
    health: undefined,
    runtime: undefined,
    ...over,
  };
}

function container(over: Partial<Container> = {}): Container {
  return {
    id: "abc123", short_id: "abc123", name: "app", image: "app:latest",
    state: "running", health: "none", restart_count: 0,
    ...over,
  };
}

function node(over: Partial<Node> = {}): Node {
  return {
    node_id: "n1", hostname: "host-1", status: "up",
    first_seen_at: "2026-01-01T00:00:00Z", last_seen_at: "2026-01-01T00:00:00Z",
    seconds_since_seen: 1,
    ...over,
  };
}

describe("container health", () => {
  // Docker freezes a container's last healthcheck result when it stops, so an
  // exited container reports `unhealthy` forever. Treating that as current
  // health raised two bogus criticals on a real host.
  it("ignores unhealthy on containers that are not running", () => {
    const { findings } = computeFindings(
      inputs({
        containers: {
          data: [
            container({ name: "stopped-db", state: "exited", health: "unhealthy" }),
          ],
          error: null,
        },
      }),
    );

    expect(findings.find((f) => f.id === "containers-unhealthy")).toBeUndefined();
  });

  it("reports unhealthy on containers that are running", () => {
    const { findings } = computeFindings(
      inputs({
        containers: {
          data: [container({ name: "live-db", state: "running", health: "unhealthy" })],
          error: null,
        },
      }),
    );

    const found = findings.find((f) => f.id === "containers-unhealthy");
    expect(found?.severity).toBe("critical");
    expect(found?.evidence).toContain("live-db");
  });

  // An absent exit_code is JSON omitempty for zero, which is a clean exit.
  it("treats an absent exit code as a clean exit", () => {
    const { findings } = computeFindings(
      inputs({
        containers: { data: [container({ state: "exited" })], error: null },
      }),
    );

    expect(findings.find((f) => f.id === "containers-crashed")).toBeUndefined();
  });

  it("reports a non-zero exit", () => {
    const { findings } = computeFindings(
      inputs({
        containers: {
          data: [container({ name: "crashed", state: "exited", exit_code: 137 })],
          error: null,
        },
      }),
    );

    const found = findings.find((f) => f.id === "containers-crashed");
    expect(found).toBeDefined();
    expect(found?.evidence[0]).toContain("137");
  });
});

describe("disk findings", () => {
  function mount(over: Partial<Mount> = {}): Mount {
    return {
      device: "/dev/sda1", mountpoint: "/", fstype: "ext4",
      total: 1000, used: 500, free: 500, used_percent: 50,
      ...over,
    };
  }

  it("separates critical from warning by threshold", () => {
    const { findings } = computeFindings(
      inputs({
        mounts: {
          data: [
            mount({ mountpoint: "/full", used_percent: 96, total: 1, free: 1 }),
            mount({ mountpoint: "/tight", used_percent: 88, total: 2, free: 2 }),
          ],
          error: null,
        },
      }),
    );

    expect(findings.find((f) => f.id === "disks-critical")?.severity).toBe("critical");
    expect(findings.find((f) => f.id === "disks-warn")?.severity).toBe("warning");
  });

  // APFS presents one container as several synthetic volumes with identical
  // totals. Counting mounts reported the same disk filling up four times.
  it("deduplicates filesystems sharing a capacity signature", () => {
    const { findings } = computeFindings(
      inputs({
        mounts: {
          data: [
            mount({ mountpoint: "/", used_percent: 96, total: 500, free: 20 }),
            mount({ mountpoint: "/System/Volumes/Data", used_percent: 96, total: 500, free: 20 }),
            mount({ mountpoint: "/System/Volumes/Update", used_percent: 96, total: 500, free: 20 }),
          ],
          error: null,
        },
      }),
    );

    const found = findings.find((f) => f.id === "disks-critical");
    expect(found?.evidence).toHaveLength(1);
  });

  // Inodes exhaust independently of bytes: a filesystem at 30% capacity with
  // no inodes refuses every write while byte-based dashboards say it is fine.
  it("reports inode exhaustion separately from capacity", () => {
    const { findings } = computeFindings(
      inputs({
        mounts: {
          data: [mount({ used_percent: 30, inodes_used_percent: 97 })],
          error: null,
        },
      }),
    );

    expect(findings.find((f) => f.id === "disks-inodes")?.severity).toBe("critical");
    expect(findings.find((f) => f.id === "disks-critical")).toBeUndefined();
  });
});

describe("certificates", () => {
  function port(over: Partial<Port> = {}): Port {
    return { protocol: "tcp", address: "0.0.0.0", port: 443, tls_probed: true, ...over };
  }

  it("separates expired from expiring", () => {
    const { findings } = computeFindings(
      inputs({
        ports: {
          data: [
            port({
              port: 443, process: "nginx",
              tls: { days_until_expiry: -3, expired: true, self_signed: false },
            }),
            port({
              port: 8443, process: "api",
              tls: { days_until_expiry: 10, expired: false, self_signed: false },
            }),
          ],
          error: null,
        },
      }),
    );

    expect(findings.find((f) => f.id === "certs-expired")?.severity).toBe("critical");
    expect(findings.find((f) => f.id === "certs-expiring")?.severity).toBe("warning");
  });

  it("does not flag a certificate with plenty of life left", () => {
    const { findings } = computeFindings(
      inputs({
        ports: {
          data: [port({ tls: { days_until_expiry: 200, expired: false, self_signed: false } })],
          error: null,
        },
      }),
    );

    expect(findings.filter((f) => f.id.startsWith("certs-"))).toHaveLength(0);
  });
});

describe("coverage", () => {
  // An all-clear that cannot distinguish "nothing is wrong" from "I could not
  // look" is the most dangerous screen a monitoring tool can render.
  it("marks a source unreadable when its query failed", () => {
    const { coverage } = computeFindings(
      inputs({ containers: { data: [], error: new Error("no docker") } }),
    );

    const containers = coverage.find((c) => c.source === "Containers");
    expect(containers?.ok).toBe(false);
    expect(containers?.reason).toBeTruthy();
  });

  it("marks a source checked when its query succeeded", () => {
    const { coverage } = computeFindings(inputs());
    expect(coverage.find((c) => c.source === "Containers")?.ok).toBe(true);
  });

  // A failed source must not produce findings from its empty data — that is
  // how "no containers are unhealthy" gets claimed about a host Atlas cannot
  // see.
  it("produces no findings from a source it could not read", () => {
    const { findings } = computeFindings(
      inputs({ mounts: { data: [], error: new Error("unreadable") } }),
    );

    expect(findings.filter((f) => f.id.startsWith("disks-"))).toHaveLength(0);
  });
});

describe("ordering and severity", () => {
  it("sorts critical findings before warnings", () => {
    const { findings } = computeFindings(
      inputs({
        nodes: [node({ status: "stale" }), node({ node_id: "n2", status: "down" })],
      }),
    );

    expect(findings.length).toBeGreaterThan(1);
    expect(findings[0]?.severity).toBe("critical");
    expect(findings.at(-1)?.severity).toBe("warning");
  });

  it("returns nothing when everything is healthy", () => {
    const { findings } = computeFindings(inputs({ nodes: [node()] }));
    expect(findings).toHaveLength(0);
  });

  it("caps evidence so one finding cannot become a wall", () => {
    const many: Service[] = Array.from({ length: 20 }, (_, i) => ({
      name: `unit-${String(i)}.service`,
      active_state: "failed", sub_state: "failed", enabled: true,
      failed: true, running: false, restart_count: 0,
    }));

    const { findings } = computeFindings(inputs({ services: { data: many, error: null } }));
    const found = findings.find((f) => f.id === "services-failed");

    expect(found?.title).toContain("20");
    expect(found?.evidence.length).toBeLessThanOrEqual(6);
  });
});

describe("process findings", () => {
  it("reports zombies", () => {
    const zombie: Process = {
      pid: 42, ppid: 1, name: "defunct", state: "zombie",
      cpu_percent: 0, memory_rss: 0, memory_percent: 0, threads: 1,
    };

    const { findings } = computeFindings(
      inputs({ processes: { data: [zombie], error: null } }),
    );

    const found = findings.find((f) => f.id === "processes-zombies");
    expect(found?.severity).toBe("warning");
    expect(found?.evidence[0]).toContain("42");
  });
});
