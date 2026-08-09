import { describe, expect, it } from "vitest";
import type { Container, LatestValue } from "../../api/types";
import {
  countByState, effectiveHealth, readExit, readUsage, splitImage,
  summariseImages, summariseProjects,
} from "./containerModel";

function container(over: Partial<Container> = {}): Container {
  return {
    id: "abc123", short_id: "abc123", name: "app", image: "app:latest",
    state: "running", health: "none", restart_count: 0,
    ...over,
  };
}

describe("effective health", () => {
  // Docker keeps reporting the last healthcheck result after a container
  // stops. That is history, not health, and reporting it as current produced
  // two false criticals on a real host.
  it("is unknown for any container that is not running", () => {
    for (const state of ["exited", "created", "paused", "dead"] as const) {
      expect(effectiveHealth(container({ state, health: "unhealthy" }))).toBe("unknown");
    }
  });

  it("reflects the healthcheck for a running container", () => {
    expect(effectiveHealth(container({ state: "running", health: "unhealthy" }))).toBe("unhealthy");
    expect(effectiveHealth(container({ state: "running", health: "healthy" }))).toBe("healthy");
    expect(effectiveHealth(container({ state: "running", health: "none" }))).toBe("none");
  });
});

describe("exit reading", () => {
  it("treats an absent code as a clean exit", () => {
    const exit = readExit(container({ state: "exited" }));
    expect(exit.kind).toBe("clean");
    expect(exit.abnormal).toBe(false);
  });

  it("reads 137 as SIGKILL and abnormal", () => {
    const exit = readExit(container({ state: "exited", exit_code: 137 }));
    expect(exit.kind).toBe("signalled");
    expect(exit.abnormal).toBe(true);
    expect(exit.reason).toContain("SIGKILL");
  });

  // SIGTERM is how a container is asked to stop, so 143 is an orderly
  // shutdown rather than a fault.
  it("reads 143 as an orderly SIGTERM shutdown", () => {
    const exit = readExit(container({ state: "exited", exit_code: 143 }));
    expect(exit.kind).toBe("signalled");
    expect(exit.abnormal).toBe(false);
  });

  // Signals occupy 129–192. Reading 255 as 128+127 produced the meaningless
  // "terminated by signal 127" on a real container.
  it("reads 255 as a generic failure, not a signal", () => {
    const exit = readExit(container({ state: "exited", exit_code: 255 }));
    expect(exit.kind).toBe("error");
    expect(exit.reason).not.toContain("signal 127");
    expect(exit.abnormal).toBe(true);
  });

  it("reports nothing for a container that has not exited", () => {
    expect(readExit(container({ state: "running" })).kind).toBe("none");
  });
});

describe("project summaries", () => {
  it("computes counts from member containers", () => {
    const projects = summariseProjects(
      [
        container({ name: "a", compose_project: "web", state: "running" }),
        container({ name: "b", compose_project: "web", state: "exited" }),
        container({ name: "c", compose_project: "web", state: "created" }),
      ],
      new Map(),
    );

    const web = projects.find((p) => p.name === "web");
    expect(web?.total).toBe(3);
    expect(web?.running).toBe(1);
    expect(web?.stopped).toBe(1);
    expect(web?.created).toBe(1);
  });

  // A project's unhealthy count must use effective health, or a stack of
  // cleanly stopped containers reports as failing.
  it("does not count stopped containers as unhealthy", () => {
    const projects = summariseProjects(
      [container({ compose_project: "web", state: "exited", health: "unhealthy" })],
      new Map(),
    );

    expect(projects.find((p) => p.name === "web")?.unhealthy).toBe(0);
  });

  it("groups containers with no compose project as standalone, sorted last", () => {
    const projects = summariseProjects(
      [
        container({ name: "loose" }),
        container({ name: "managed", compose_project: "web" }),
      ],
      new Map(),
    );

    expect(projects.at(-1)?.standalone).toBe(true);
    expect(projects.at(-1)?.total).toBe(1);
  });

  // Resource totals must state their denominator: only running containers
  // report usage, so a three-container project may be summing one.
  it("reports how many members contributed to resource totals", () => {
    const usage = new Map([["a", { name: "a", cpu: 10, memory: 100 }]]);
    const projects = summariseProjects(
      [
        container({ name: "a", compose_project: "web", state: "running" }),
        container({ name: "b", compose_project: "web", state: "exited" }),
      ],
      usage,
    );

    const web = projects.find((p) => p.name === "web");
    expect(web?.reporting).toBe(1);
    expect(web?.total).toBe(2);
    expect(web?.cpu).toBe(10);
  });
});

describe("image parsing", () => {
  it("splits repository and tag", () => {
    expect(splitImage("redis:8-alpine")).toEqual({ repository: "redis", tag: "8-alpine" });
  });

  it("defaults a missing tag to latest", () => {
    expect(splitImage("redis")).toEqual({ repository: "redis", tag: "latest" });
  });

  // A registry host may carry a port, and that colon is not a tag separator.
  it("does not mistake a registry port for a tag", () => {
    expect(splitImage("registry:5000/app")).toEqual({
      repository: "registry:5000/app",
      tag: "latest",
    });
    expect(splitImage("registry:5000/app:v2")).toEqual({
      repository: "registry:5000/app",
      tag: "v2",
    });
  });

  // Two containers claiming the same tag while running different digests is
  // the confusion an incident needs to resolve quickly.
  it("detects a tag resolving to more than one build", () => {
    const images = summariseImages([
      container({ name: "old", image: "app:latest", image_id: "sha256:aaa" }),
      container({ name: "new", image: "app:latest", image_id: "sha256:bbb" }),
    ]);

    expect(images[0]?.digests).toHaveLength(2);
  });
});

describe("usage parsing", () => {
  function value(metric: string, containerName: string, v: number): LatestValue {
    return {
      metric, value: v, unit: "percent", kind: "gauge", collector_id: "docker.stats",
      time: "2026-01-01T00:00:00Z", labels: { container: containerName },
    };
  }

  it("keys usage by container name", () => {
    const usage = readUsage([
      value("docker.container.cpu.usage", "app", 12.5),
      value("docker.container.memory.usage", "app", 1024),
    ]);

    expect(usage.get("app")?.cpu).toBe(12.5);
    expect(usage.get("app")?.memory).toBe(1024);
  });

  // Only stats metrics count. `docker.container.up` and `restarts` are emitted
  // for stopped containers too, and treating them as usage would create
  // entries for containers that report no resources at all.
  it("ignores metrics that are not live resource usage", () => {
    const usage = readUsage([
      value("docker.container.up", "stopped", 0),
      value("docker.container.restarts", "stopped", 3),
    ]);

    expect(usage.has("stopped")).toBe(false);
  });

  it("ignores values with no container label", () => {
    const usage = readUsage([
      {
        metric: "docker.containers.total", value: 15, unit: "count", kind: "gauge",
        collector_id: "docker.containers", time: "2026-01-01T00:00:00Z",
      },
    ]);

    expect(usage.size).toBe(0);
  });
});

describe("state counting", () => {
  it("counts every docker state", () => {
    const counts = countByState([
      container({ state: "running" }),
      container({ state: "running" }),
      container({ state: "exited" }),
    ]);

    expect(counts.running).toBe(2);
    expect(counts.exited).toBe(1);
    expect(counts.paused).toBe(0);
  });
});
