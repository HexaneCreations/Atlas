import type { Container, ContainerState, LatestValue } from "../../api/types";

/**
 * The container domain model: lifecycle, health, projects, images, resources.
 *
 * Pure functions over the API payload, kept apart from rendering so the rules
 * that matter can be read and argued with in one place. Three of them are
 * correctness rules rather than presentation choices:
 *
 * 1. **Health belongs to running containers only.** Docker freezes a
 *    container's last healthcheck result when it stops, so an exited container
 *    reports `unhealthy` forever. Treating that as current health raises
 *    alarms about machines that are simply switched off — a false alarm that
 *    already cost this project a bogus pair of criticals on the Overview.
 *
 * 2. **Runtime state and health state are separate axes.** A container can be
 *    running and unhealthy, or stopped and last-seen-healthy. Collapsing them
 *    into one "status" loses the only signal `docker ps` cannot give you.
 *
 * 3. **Resource usage comes from live runtime metrics only.** A stopped
 *    container has no CPU or memory figure, and rendering zero for it makes an
 *    idle-looking row out of a dead one.
 */

// ------------------------------------------------------------------ health ---

/**
 * Health as it can honestly be reported, given the container's state.
 *
 * `unknown` is not a failure: it is the correct answer for a container that is
 * not running, whatever its last check said.
 */
export type EffectiveHealth = "healthy" | "unhealthy" | "starting" | "none" | "unknown";

export function effectiveHealth(c: Container): EffectiveHealth {
  // Only a running container has a current healthcheck result. Docker keeps
  // reporting the last one after exit, which is history, not health.
  if (c.state !== "running") return "unknown";
  return c.health;
}

/** Whether the container's healthcheck is currently failing. The one condition
 *  that should page somebody. */
export function isFailingHealth(c: Container): boolean {
  return effectiveHealth(c) === "unhealthy";
}

// --------------------------------------------------------------- lifecycle ---

/** Docker's states, grouped by what an operator does about them. */
export const LIFECYCLE_ORDER: ContainerState[] = [
  "running", "restarting", "paused", "created", "exited", "dead", "removing",
];

export function countByState(containers: Container[]): Record<ContainerState, number> {
  const out: Record<ContainerState, number> = {
    created: 0, running: 0, paused: 0, restarting: 0, removing: 0, exited: 0, dead: 0,
  };
  for (const c of containers) out[c.state]++;
  return out;
}

/**
 * How a container's exit should be read.
 *
 * Docker reports 0 for a clean exit and omits the field entirely when it is
 * zero, so an absent code means a clean exit rather than an unknown one.
 * 128+n encodes termination by signal n, which is where the interesting cases
 * live: 137 is SIGKILL — usually the OOM killer or a `docker stop` that timed
 * out — and 143 is a normal SIGTERM shutdown.
 */
export type ExitKind = "clean" | "signalled" | "error" | "none";

export interface ExitReading {
  kind: ExitKind;
  code: number;
  /** A short, plain explanation. Empty for a clean exit. */
  reason: string;
  /** Whether this deserves an operator's attention. */
  abnormal: boolean;
}

const SIGNALS: Record<number, string> = {
  1: "SIGHUP", 2: "SIGINT", 3: "SIGQUIT", 6: "SIGABRT", 9: "SIGKILL",
  11: "SIGSEGV", 13: "SIGPIPE", 15: "SIGTERM",
};

export function readExit(c: Container): ExitReading {
  if (c.state !== "exited" && c.state !== "dead") {
    return { kind: "none", code: 0, reason: "", abnormal: false };
  }

  const code = c.exit_code ?? 0;

  if (code === 0) {
    return { kind: "clean", code: 0, reason: "exited cleanly", abnormal: false };
  }

  // Signals occupy 129–192 (128 + 1..64, SIGRTMAX being the ceiling on Linux).
  // Codes above that are not signals: 255 in particular is the conventional
  // "generic failure" an image returns, and reading it as 128+127 produced the
  // meaningless "terminated by signal 127".
  if (code > 128 && code <= 192) {
    const signal = code - 128;
    const name = SIGNALS[signal] ?? `signal ${String(signal)}`;
    // SIGTERM is how a container is asked to stop, so a 143 is an orderly
    // shutdown rather than a fault. SIGKILL is not: it means the process
    // ignored SIGTERM and was killed, or the OOM killer took it.
    const orderly = signal === 15 || signal === 2;
    return {
      kind: "signalled",
      code,
      reason: orderly
        ? `stopped by ${name}`
        : signal === 9
          ? `killed (${name}) — out of memory, or it ignored a stop request`
          : `terminated by ${name}`,
      abnormal: !orderly,
    };
  }

  if (code === 255) {
    return {
      kind: "error",
      code,
      reason: "exited with 255 — the conventional catch-all failure, so the cause is in the container's own logs",
      abnormal: true,
    };
  }

  return { kind: "error", code, reason: `exited with code ${String(code)}`, abnormal: true };
}

// ----------------------------------------------------------------- projects ---

/** Containers with no compose project. A real grouping, not a gap: standalone
 *  containers are how most of a host's incidental workload runs. */
export const STANDALONE = "standalone";

export interface ProjectSummary {
  name: string;
  standalone: boolean;
  containers: Container[];
  total: number;
  running: number;
  stopped: number;
  created: number;
  restarting: number;
  paused: number;
  /** Failing healthchecks among *running* containers only. */
  unhealthy: number;
  /** Containers whose last exit was abnormal. */
  abnormalExits: number;
  restarts: number;
  images: string[];
  /** Live resource use, summed across running members that report metrics. */
  cpu?: number | undefined;
  memory?: number | undefined;
  /** How many members contributed to those figures — the denominator, which
   *  the UI states rather than implying the sum covers everything. */
  reporting: number;
}

export function summariseProjects(
  containers: Container[],
  usage: Map<string, ContainerUsage>,
): ProjectSummary[] {
  const groups = new Map<string, Container[]>();
  for (const c of containers) {
    const key = c.compose_project ?? STANDALONE;
    groups.set(key, [...(groups.get(key) ?? []), c]);
  }

  const out: ProjectSummary[] = [];
  for (const [name, members] of groups) {
    const states = countByState(members);
    let cpu: number | undefined;
    let memory: number | undefined;
    let reporting = 0;

    for (const c of members) {
      const u = usage.get(c.name);
      if (!u) continue;
      reporting++;
      if (u.cpu !== undefined) cpu = (cpu ?? 0) + u.cpu;
      if (u.memory !== undefined) memory = (memory ?? 0) + u.memory;
    }

    out.push({
      name,
      standalone: name === STANDALONE,
      containers: members,
      total: members.length,
      running: states.running,
      stopped: states.exited + states.dead,
      created: states.created,
      restarting: states.restarting,
      paused: states.paused,
      // Computed from members, never inferred from a project-level metric.
      unhealthy: members.filter(isFailingHealth).length,
      abnormalExits: members.filter((c) => readExit(c).abnormal).length,
      restarts: members.reduce((s, c) => s + c.restart_count, 0),
      images: [...new Set(members.map((c) => c.image))].sort((a, b) => a.localeCompare(b)),
      cpu,
      memory,
      reporting,
    });
  }

  // Projects first, alphabetically; standalone last, because it is a bucket
  // rather than a deployment.
  return out.sort((a, b) => {
    if (a.standalone) return 1;
    if (b.standalone) return -1;
    return a.name.localeCompare(b.name);
  });
}

/** A project's worst condition, for a single at-a-glance tone. */
export function projectTone(p: ProjectSummary): "danger" | "warning" | "success" | "neutral" {
  if (p.unhealthy > 0) return "danger";
  if (p.abnormalExits > 0 || p.restarting > 0) return "warning";
  if (p.running > 0) return "success";
  return "neutral";
}

// ------------------------------------------------------------------ images ---

export interface ImageSummary {
  /** The full reference as Docker reports it, e.g. `redis:8-alpine`. */
  reference: string;
  repository: string;
  tag: string;
  /** Distinct digests seen under this reference. More than one means two
   *  containers claim the same tag while running different builds. */
  digests: string[];
  containers: Container[];
  running: number;
  total: number;
}

export function summariseImages(containers: Container[]): ImageSummary[] {
  const groups = new Map<string, Container[]>();
  for (const c of containers) {
    groups.set(c.image, [...(groups.get(c.image) ?? []), c]);
  }

  const out: ImageSummary[] = [];
  for (const [reference, members] of groups) {
    const { repository, tag } = splitImage(reference);
    out.push({
      reference,
      repository,
      tag,
      digests: [...new Set(members.map((c) => c.image_id).filter((d): d is string => Boolean(d)))],
      containers: members,
      running: members.filter((c) => c.state === "running").length,
      total: members.length,
    });
  }

  // Most-used first: the images the host actually depends on lead.
  return out.sort((a, b) => b.total - a.total || a.reference.localeCompare(b.reference));
}

/**
 * Splits an image reference into repository and tag.
 *
 * A registry host may carry a port (`registry:5000/app:v1`), so the tag is the
 * part after the last colon *only* when that colon comes after the last slash.
 */
export function splitImage(reference: string): { repository: string; tag: string } {
  const lastColon = reference.lastIndexOf(":");
  const lastSlash = reference.lastIndexOf("/");
  if (lastColon > lastSlash) {
    return { repository: reference.slice(0, lastColon), tag: reference.slice(lastColon + 1) };
  }
  return { repository: reference, tag: "latest" };
}

// --------------------------------------------------------------- resources ---

/**
 * One container's live resource use.
 *
 * Keyed by container *name*, because that is the label the Docker stats
 * collector attaches. Only running containers appear: the collector samples
 * `docker stats`, which has nothing to say about a stopped container.
 */
export interface ContainerUsage {
  name: string;
  cpu?: number | undefined;
  memory?: number | undefined;
  memoryPercent?: number | undefined;
  memoryLimit?: number | undefined;
  netRx?: number | undefined;
  netTx?: number | undefined;
  blockRead?: number | undefined;
  blockWrite?: number | undefined;
  pids?: number | undefined;
}

const USAGE_METRICS: Record<string, keyof Omit<ContainerUsage, "name">> = {
  "docker.container.cpu.usage": "cpu",
  "docker.container.memory.usage": "memory",
  "docker.container.memory.percent": "memoryPercent",
  "docker.container.memory.limit": "memoryLimit",
  "docker.container.network.rx": "netRx",
  "docker.container.network.tx": "netTx",
  "docker.container.block.read": "blockRead",
  "docker.container.block.write": "blockWrite",
  "docker.container.pids": "pids",
};

export function readUsage(values: LatestValue[]): Map<string, ContainerUsage> {
  const out = new Map<string, ContainerUsage>();

  for (const v of values) {
    const field = USAGE_METRICS[v.metric];
    const name = v.labels?.container;
    if (!field || !name) continue;

    const entry = out.get(name) ?? { name };
    entry[field] = v.value;
    out.set(name, entry);
  }

  return out;
}
