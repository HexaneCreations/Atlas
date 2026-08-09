import type {
  CollectorsResponse,
  Container,
  ListCronJobsResponse,
  Mount,
  Node,
  Port,
  Process,
  Service,
  SystemHealth,
  SystemRuntime,
} from "../../api/types";

/**
 * The findings engine: everything Atlas knows that an operator should act on.
 *
 * This is the one piece of Atlas that reads across every source at once. Each
 * page answers "what is true of containers / disks / ports"; nobody was
 * answering "what is true of this machine that I need to know about", which is
 * the only question someone opening a dashboard at 3am actually has.
 *
 * Kept as a pure function so the rules can be read, argued with and tested
 * without rendering anything. Adding a rule should be a five-line change here
 * and nothing anywhere else.
 *
 * Two severities only. A third invites an "info" tier, and an attention panel
 * that lists things not needing attention is a panel people stop reading.
 */

export type Severity = "critical" | "warning";

export interface Finding {
  id: string;
  severity: Severity;
  /** The count-led headline: "3 containers are unhealthy". */
  title: string;
  /** Why it matters, in one sentence. */
  detail: string;
  /** Named instances, so the finding is checkable rather than a claim. */
  evidence: string[];
  /** Where to go to investigate. */
  to: string;
  /** Which page owns this signal. */
  source: string;
}

/**
 * What Atlas could and could not read.
 *
 * An all-clear that cannot distinguish "nothing is wrong" from "I could not
 * look" is worse than no all-clear at all, so coverage is reported alongside
 * the findings and rendered next to them.
 */
export interface Coverage {
  source: string;
  ok: boolean;
  /** Why it could not be read. Present only when `ok` is false. */
  reason?: string;
}

export interface Inputs {
  nodes: Node[];
  services: { data: Service[]; error: Error | null | undefined };
  containers: { data: Container[]; error: Error | null | undefined };
  mounts: { data: Mount[]; error: Error | null | undefined };
  ports: { data: Port[]; error: Error | null | undefined };
  processes: { data: Process[]; error: Error | null | undefined };
  cron: { data: ListCronJobsResponse | undefined; error: Error | null | undefined };
  collectors: CollectorsResponse | undefined;
  health: SystemHealth | undefined;
  runtime: SystemRuntime | undefined;
}

/** Disk thresholds. 85% is where a filesystem stops being comfortable; 95%
 *  is where writes start failing for real workloads. */
const DISK_WARN = 85;
const DISK_CRITICAL = 95;

/** A certificate inside this window needs a renewal already in motion. */
const CERT_WARN_DAYS = 30;

/** Restarts that indicate a crash loop rather than a deployment. */
const RESTART_LOOP = 5;

/** Evidence lists are capped: a finding naming forty units is a wall, and the
 *  count in the title already carries the scale. */
const EVIDENCE_CAP = 6;

export function computeFindings(input: Inputs): { findings: Finding[]; coverage: Coverage[] } {
  const findings: Finding[] = [];
  const coverage: Coverage[] = [];

  const track = (source: string, error: Error | null | undefined, reason: string) => {
    coverage.push(error ? { source, ok: false, reason } : { source, ok: true });
    return !error;
  };

  // ------------------------------------------------------------------ nodes
  const down = input.nodes.filter((n) => n.status === "down");
  const stale = input.nodes.filter((n) => n.status === "stale");

  if (down.length > 0) {
    findings.push({
      id: "nodes-down",
      severity: "critical",
      title: `${plural(down.length, "node")} not reporting`,
      detail:
        "No sweep has arrived within the liveness window. Everything shown for these hosts is the last known state, not the current one.",
      evidence: down.map((n) => n.hostname).slice(0, EVIDENCE_CAP),
      to: "/nodes",
      source: "Nodes",
    });
  }
  if (stale.length > 0) {
    findings.push({
      id: "nodes-stale",
      severity: "warning",
      title: `${plural(stale.length, "node")} gone quiet`,
      detail:
        "Reporting later than the expected interval. Usually a slow or saturated host rather than a dead one.",
      evidence: stale.map((n) => n.hostname).slice(0, EVIDENCE_CAP),
      to: "/nodes",
      source: "Nodes",
    });
  }

  // --------------------------------------------------------------- services
  if (track("Services", input.services.error, "systemd not readable on this host")) {
    const failed = input.services.data.filter((s) => s.failed || s.active_state === "failed");
    if (failed.length > 0) {
      findings.push({
        id: "services-failed",
        severity: "critical",
        title: `${plural(failed.length, "unit")} failed`,
        detail:
          "systemd has given up on these units. Anything depending on them is degraded whether or not it reports so itself.",
        evidence: failed.map((s) => s.name).slice(0, EVIDENCE_CAP),
        to: "/services",
        source: "Services",
      });
    }
  }

  // ------------------------------------------------------------- containers
  if (track("Containers", input.containers.error, "Docker not available on this host")) {
    const cs = input.containers.data;

    // Running is part of the rule, not a refinement of it. Docker freezes the
    // last healthcheck result when a container stops, so a cleanly exited
    // container keeps reporting `unhealthy` forever. Matching on health alone
    // raised two criticals here for containers that had exited with code 0 —
    // a false alarm of exactly the kind that teaches people to ignore the
    // panel. The exit is the fact worth reporting about a stopped container,
    // and the rule below reports it.
    const unhealthy = cs.filter((c) => c.health === "unhealthy" && c.state === "running");
    if (unhealthy.length > 0) {
      findings.push({
        id: "containers-unhealthy",
        severity: "critical",
        title: `${plural(unhealthy.length, "container")} unhealthy`,
        detail:
          "The image's own healthcheck is failing. These are running — which is why they do not show up as stopped — and still not serving correctly.",
        evidence: unhealthy.map((c) => c.name).slice(0, EVIDENCE_CAP),
        to: "/containers",
        source: "Containers",
      });
    }

    const crashed = cs.filter((c) => c.state === "exited" && (c.exit_code ?? 0) !== 0);
    if (crashed.length > 0) {
      findings.push({
        id: "containers-crashed",
        severity: "warning",
        title: `${plural(crashed.length, "container")} exited with an error`,
        detail:
          "A non-zero exit code. For a one-shot job this may be expected; for a service it means it stopped and did not come back.",
        evidence: crashed
          .map((c) => `${c.name} · exit ${String(c.exit_code ?? 0)}`)
          .slice(0, EVIDENCE_CAP),
        to: "/containers",
        source: "Containers",
      });
    }

    const looping = cs.filter((c) => c.restart_count >= RESTART_LOOP && c.state === "running");
    if (looping.length > 0) {
      findings.push({
        id: "containers-restart-loop",
        severity: "warning",
        title: `${plural(looping.length, "container")} restarting repeatedly`,
        detail:
          "Running now, but it has been restarted many times. A container that keeps coming back is not the same as one that stayed up.",
        evidence: looping
          .map((c) => `${c.name} · ${String(c.restart_count)} restarts`)
          .slice(0, EVIDENCE_CAP),
        to: "/containers",
        source: "Containers",
      });
    }
  }

  // ------------------------------------------------------------------ disks
  if (track("Disks", input.mounts.error, "filesystem inventory not readable")) {
    const pools = dedupePools(input.mounts.data);

    const critical = pools.filter((m) => m.used_percent >= DISK_CRITICAL);
    const warn = pools.filter(
      (m) => m.used_percent >= DISK_WARN && m.used_percent < DISK_CRITICAL,
    );

    if (critical.length > 0) {
      findings.push({
        id: "disks-critical",
        severity: "critical",
        title: `${plural(critical.length, "filesystem")} nearly full`,
        detail: `Above ${String(DISK_CRITICAL)}% used. Writes fail before a disk reaches 100%, so this is an outage forming rather than a threshold crossed.`,
        evidence: critical
          .map((m) => `${m.mountpoint} · ${m.used_percent.toFixed(0)}%`)
          .slice(0, EVIDENCE_CAP),
        to: "/disks",
        source: "Disks",
      });
    }
    if (warn.length > 0) {
      findings.push({
        id: "disks-warn",
        severity: "warning",
        title: `${plural(warn.length, "filesystem")} above ${String(DISK_WARN)}%`,
        detail: "Still serving, but with little headroom for a log burst or an unplanned image pull.",
        evidence: warn
          .map((m) => `${m.mountpoint} · ${m.used_percent.toFixed(0)}%`)
          .slice(0, EVIDENCE_CAP),
        to: "/disks",
        source: "Disks",
      });
    }

    // Inodes exhaust independently of bytes. A filesystem at 30% capacity and
    // 100% inodes refuses new files while every byte-based dashboard says it
    // is fine, which is exactly why this is worth its own finding.
    const inodes = input.mounts.data.filter((m) => (m.inodes_used_percent ?? 0) >= DISK_WARN);
    if (inodes.length > 0) {
      findings.push({
        id: "disks-inodes",
        severity: "critical",
        title: `${plural(inodes.length, "filesystem")} low on inodes`,
        detail:
          "Inodes exhaust independently of capacity. A filesystem with free space and no inodes cannot create a file, and reports plenty of room while refusing every write.",
        evidence: inodes
          .map((m) => `${m.mountpoint} · ${(m.inodes_used_percent ?? 0).toFixed(0)}% inodes`)
          .slice(0, EVIDENCE_CAP),
        to: "/disks",
        source: "Disks",
      });
    }
  }

  // ------------------------------------------------------------------ certs
  if (track("Ports", input.ports.error, "listening sockets not readable")) {
    const withTLS = input.ports.data.filter((p) => p.tls);

    const expired = withTLS.filter((p) => p.tls?.expired);
    if (expired.length > 0) {
      findings.push({
        id: "certs-expired",
        severity: "critical",
        title: `${plural(expired.length, "certificate")} expired`,
        detail:
          "The service is still listening and every client validating this certificate is being turned away.",
        evidence: expired.map((p) => certLabel(p)).slice(0, EVIDENCE_CAP),
        to: "/ports",
        source: "Ports",
      });
    }

    const expiring = withTLS.filter(
      (p) => p.tls && !p.tls.expired && p.tls.days_until_expiry <= CERT_WARN_DAYS,
    );
    if (expiring.length > 0) {
      findings.push({
        id: "certs-expiring",
        severity: "warning",
        title: `${plural(expiring.length, "certificate")} expiring within ${String(CERT_WARN_DAYS)} days`,
        detail:
          "Renewal needs to be already in motion. This is the one finding on the page with a known deadline attached.",
        evidence: expiring
          .map((p) => `${certLabel(p)} · ${String(p.tls?.days_until_expiry ?? 0)}d`)
          .slice(0, EVIDENCE_CAP),
        to: "/ports",
        source: "Ports",
      });
    }
  }

  // -------------------------------------------------------------- processes
  if (track("Processes", input.processes.error, "process table not readable")) {
    const zombies = input.processes.data.filter((p) => p.state === "zombie");
    if (zombies.length > 0) {
      findings.push({
        id: "processes-zombies",
        severity: "warning",
        title: plural(zombies.length, "zombie process"),
        detail:
          "A parent is not reaping its children. Zombies hold a PID each and only accumulate, so this grows until the parent is restarted.",
        evidence: zombies.map((p) => `${p.name} · pid ${String(p.pid)}`).slice(0, EVIDENCE_CAP),
        to: "/processes",
        source: "Processes",
      });
    }
  }

  // --------------------------------------------------------------- platform
  // Atlas's own health belongs here. A collector that is failing or refusing
  // series means the rest of this page is quietly incomplete, and nothing else
  // in the product would ever say so.
  const collectors = input.collectors?.collectors ?? [];
  const failing = collectors.filter((c) => !c.healthy);
  if (failing.length > 0) {
    findings.push({
      id: "collectors-failing",
      severity: "critical",
      title: `${plural(failing.length, "collector")} failing`,
      detail:
        "Metrics from these collectors are missing or stale. Panels fed by them look calm because there is no data, not because there is no problem.",
      evidence: failing
        .map((c) => `${c.name}${c.last_error ? ` · ${c.last_error}` : ""}`)
        .slice(0, EVIDENCE_CAP),
      to: "/",
      source: "Platform",
    });
  }

  const refusing = collectors.filter((c) => c.refused_series > 0);
  if (refusing.length > 0) {
    findings.push({
      id: "collectors-refused",
      severity: "warning",
      title: "Cardinality budget reached",
      detail:
        "Series are being refused, so some labels are not being recorded at all. The budget protects storage; the cost is a silent gap in what you can query later.",
      evidence: refusing
        .map((c) => `${c.name} · ${String(c.refused_series)} refused`)
        .slice(0, EVIDENCE_CAP),
      to: "/",
      source: "Platform",
    });
  }

  const dropped = input.runtime?.event_bus.dropped ?? 0;
  if (dropped > 0) {
    findings.push({
      id: "eventbus-dropped",
      severity: "warning",
      title: "Events dropped",
      detail:
        "The event bus is lossy by design — it drops rather than blocking a collector. Sustained drops mean subscribers cannot keep up and some activity was never recorded.",
      evidence: [`${dropped.toLocaleString()} dropped`],
      to: "/",
      source: "Platform",
    });
  }

  const unhealthyChecks = (input.health?.checks ?? []).filter((c) => c.status !== "healthy");
  if (unhealthyChecks.length > 0) {
    findings.push({
      id: "health-checks",
      severity: "critical",
      title: `${plural(unhealthyChecks.length, "dependency")} unhealthy`,
      detail:
        "A dependency Atlas itself needs is failing. Storage or migration problems here affect every page, not only this one.",
      evidence: unhealthyChecks
        .map((c) => `${c.name}${c.error ? ` · ${c.error}` : ""}`)
        .slice(0, EVIDENCE_CAP),
      to: "/",
      source: "Platform",
    });
  }

  // Cron is read for coverage only. Root-owned jobs are a posture observation
  // rather than a fault, and an attention panel that lists normal system
  // configuration trains people to ignore it.
  track("Scheduled jobs", input.cron.error, "no readable crontab on this host");

  // Critical first; stable within a severity, so the panel does not reshuffle
  // itself between polls while somebody is reading it.
  const rank: Record<Severity, number> = { critical: 0, warning: 1 };
  return {
    findings: findings.sort((a, b) => rank[a.severity] - rank[b.severity]),
    coverage,
  };
}

function plural(n: number, noun: string): string {
  return `${n.toLocaleString()} ${noun}${n === 1 ? "" : "s"}`;
}

function certLabel(p: Port): string {
  return `${p.process ?? "port"} :${String(p.port)}`;
}

/**
 * Collapses synthetic devices that share one physical pool.
 *
 * APFS presents a single container as several volumes with identical totals,
 * so counting mounts would report the same disk filling up four times and
 * inflate a single finding into a wall of duplicates.
 */
function dedupePools(mounts: Mount[]): Mount[] {
  const seen = new Map<string, Mount>();
  for (const m of mounts) {
    const key = `${String(m.total)}:${String(m.free)}`;
    if (!seen.has(key)) seen.set(key, m);
  }
  return [...seen.values()];
}
