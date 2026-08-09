import type { Certificate, Port } from "../../api/types";

/**
 * The port domain model: exposure, TLS posture, and process ownership.
 *
 * The organising idea of this page is that a listening socket's risk is
 * decided almost entirely by *what can reach it*, not by what it is. A
 * database on loopback and the same database on 0.0.0.0 are the same service
 * and completely different exposures, and no other field in the payload
 * carries that distinction.
 *
 * Two honesty rules run through this module:
 *
 * 1. **"No certificate" is not "plaintext".** TLS probing is budgeted and
 *    TCP-only, so a socket with no certificate may simply never have been
 *    looked at. The API reports `tls_probed` precisely so the two can be told
 *    apart, and this module keeps them apart all the way to the screen.
 *
 * 2. **Plaintext is an observation, not a verdict.** A plaintext port is only
 *    a finding when something can reach it. Loopback services are plaintext by
 *    design almost everywhere, and flagging them would bury the one that
 *    matters under twenty that do not.
 */

// ---------------------------------------------------------------- exposure ---

/**
 * How far a socket can be reached from.
 *
 * `world` does not mean "reachable from the internet" — that depends on
 * firewalls and routing Atlas cannot see. It means the socket accepts
 * connections on every interface the host has, so nothing about the *binding*
 * limits who may connect. That is the honest claim, and it is the one that
 * matters when reviewing a host.
 */
export type Exposure = "world" | "network" | "loopback";

const WILDCARD = new Set(["0.0.0.0", "::", "::0", "*"]);
const LOOPBACK = new Set(["127.0.0.1", "::1", "localhost"]);

export function exposureOf(p: Port): Exposure {
  if (WILDCARD.has(p.address)) return "world";
  if (LOOPBACK.has(p.address) || p.address.startsWith("127.")) return "loopback";
  return "network";
}

export const EXPOSURE_ORDER: Exposure[] = ["world", "network", "loopback"];

export const EXPOSURE_LABEL: Record<Exposure, string> = {
  world: "All interfaces",
  network: "Specific interface",
  loopback: "Loopback only",
};

export const EXPOSURE_DETAIL: Record<Exposure, string> = {
  world:
    "Bound to every interface on the host. Nothing about the binding restricts who may connect — only a firewall could.",
  network:
    "Bound to one non-loopback address, so it is reachable by anything that can route to that interface.",
  loopback:
    "Bound to loopback, so only processes on this host can connect. Not reachable from the network at all.",
};

// --------------------------------------------------------------------- TLS ---

/**
 * What Atlas actually knows about a socket's transport security.
 *
 * Five states, because collapsing them loses the difference between a fact and
 * a gap. `unprobed` and `plaintext` in particular look identical in the raw
 * payload and mean opposite things to a reader.
 */
export type TLSState = "valid" | "expiring" | "expired" | "self-signed" | "plaintext" | "unprobed";

/** Inside this window a certificate needs a renewal already underway. */
export const CERT_WARN_DAYS = 30;

export function tlsStateOf(p: Port): TLSState {
  if (p.tls) {
    if (p.tls.expired) return "expired";
    if (p.tls.self_signed) return "self-signed";
    if (p.tls.days_until_expiry <= CERT_WARN_DAYS) return "expiring";
    return "valid";
  }
  // UDP is never probed, and a TCP port beyond the probe budget was not looked
  // at either. Neither is evidence of plaintext.
  return p.tls_probed ? "plaintext" : "unprobed";
}

export const TLS_LABEL: Record<TLSState, string> = {
  valid: "Valid certificate",
  expiring: "Expiring soon",
  expired: "Expired",
  "self-signed": "Self-signed",
  plaintext: "No TLS",
  unprobed: "Not probed",
};

/**
 * Whether a socket's transport security is worth acting on.
 *
 * Plaintext counts only when something off-host can reach it. On loopback it is
 * the norm and flagging it would drown the real findings.
 */
export function tlsNeedsAttention(p: Port): boolean {
  const state = tlsStateOf(p);
  if (state === "expired" || state === "expiring") return true;
  if (state === "self-signed") return exposureOf(p) !== "loopback";
  if (state === "plaintext") return exposureOf(p) === "world";
  return false;
}

// ------------------------------------------------------------------ posture ---

export interface Posture {
  total: number;
  tcp: number;
  udp: number;
  byExposure: Record<Exposure, Port[]>;
  /** TCP sockets a probe cycle actually attempted. */
  probed: number;
  probeable: number;
  certificates: Port[];
  expired: Port[];
  expiring: Port[];
  selfSigned: Port[];
  /** Plaintext on a world-reachable binding — the combination worth reviewing. */
  exposedPlaintext: Port[];
  /** Sockets whose owning process Atlas could not resolve. */
  unattributed: Port[];
}

export function readPosture(ports: Port[]): Posture {
  const byExposure: Record<Exposure, Port[]> = { world: [], network: [], loopback: [] };
  for (const p of ports) byExposure[exposureOf(p)].push(p);

  const tcp = ports.filter((p) => p.protocol === "tcp");
  const certificates = ports.filter((p) => p.tls);

  return {
    total: ports.length,
    tcp: tcp.length,
    udp: ports.length - tcp.length,
    byExposure,
    probed: tcp.filter((p) => p.tls_probed).length,
    // Only TCP can carry a TLS handshake, so it is the honest denominator for
    // coverage — reporting "1 of 33" against every socket would understate it.
    probeable: tcp.length,
    certificates,
    expired: certificates.filter((p) => p.tls?.expired),
    expiring: certificates.filter(
      (p) => p.tls && !p.tls.expired && p.tls.days_until_expiry <= CERT_WARN_DAYS,
    ),
    selfSigned: certificates.filter((p) => p.tls?.self_signed),
    exposedPlaintext: ports.filter(
      (p) => exposureOf(p) === "world" && tlsStateOf(p) === "plaintext",
    ),
    unattributed: ports.filter((p) => !p.pid),
  };
}

// ----------------------------------------------------------------- ownership ---

export interface ProcessOwnership {
  key: string;
  process: string;
  /** Every pid running under this name. Multi-process programs — browsers,
   *  editors, language servers — routinely have several. */
  pids: number[];
  ports: Port[];
  exposures: Set<Exposure>;
  /** Highest exposure this process reaches, for a single tone. */
  worst: Exposure;
}

/**
 * Sockets grouped by the program holding them.
 *
 * Keyed by *name*, not pid, and that choice is deliberate. A browser or editor
 * spawns a dozen helper processes under one name; grouping by pid produces a
 * dozen near-identical rows and answers "which pid" when the question is
 * "which program". The pids are kept and shown so nothing is lost.
 *
 * It also keeps this panel consistent with the explorer's process filter,
 * which selects by name. Grouping by pid here meant clicking one row filtered
 * the table to every pid sharing its name — the panel and the filter
 * disagreeing about what a "process" is.
 */
export function groupByProcess(ports: Port[]): ProcessOwnership[] {
  const groups = new Map<string, ProcessOwnership>();

  for (const p of ports) {
    const key = p.process ?? "unattributed";
    const existing = groups.get(key);
    if (existing) {
      existing.ports.push(p);
      existing.exposures.add(exposureOf(p));
      if (p.pid !== undefined && !existing.pids.includes(p.pid)) existing.pids.push(p.pid);
      continue;
    }
    groups.set(key, {
      key,
      process: key,
      pids: p.pid !== undefined ? [p.pid] : [],
      ports: [p],
      exposures: new Set([exposureOf(p)]),
      worst: "loopback",
    });
  }

  const out = [...groups.values()];
  for (const g of out) {
    g.worst = g.exposures.has("world") ? "world" : g.exposures.has("network") ? "network" : "loopback";
    g.ports.sort((a, b) => a.port - b.port);
    g.pids.sort((a, b) => a - b);
  }

  // Widest exposure first, then most sockets: the process to look at is the one
  // reachable from furthest away, not merely the one holding the most ports.
  const rank: Record<Exposure, number> = { world: 0, network: 1, loopback: 2 };
  return out.sort(
    (a, b) =>
      rank[a.worst] - rank[b.worst] ||
      b.ports.length - a.ports.length ||
      a.process.localeCompare(b.process),
  );
}

// --------------------------------------------------------------- well-known ---

/**
 * Names for ports whose number carries meaning.
 *
 * Deliberately short and only for ports where the convention is strong enough
 * to be useful. Atlas does not claim the service *is* this — it reports what
 * the number conventionally means, which is a hint for a reader, not an
 * identification. The owning process name is the real answer and is always
 * shown beside it.
 */
const WELL_KNOWN: Record<number, string> = {
  22: "SSH", 25: "SMTP", 53: "DNS", 80: "HTTP", 110: "POP3", 143: "IMAP",
  443: "HTTPS", 445: "SMB", 587: "SMTP", 993: "IMAPS", 995: "POP3S",
  1433: "MSSQL", 1521: "Oracle", 2049: "NFS", 3000: "dev server",
  3306: "MySQL", 3389: "RDP", 5000: "dev server", 5173: "Vite",
  5432: "PostgreSQL", 5672: "AMQP", 6379: "Redis", 8080: "HTTP alt",
  8443: "HTTPS alt", 9092: "Kafka", 9200: "Elasticsearch", 11211: "memcached",
  15672: "RabbitMQ UI", 27017: "MongoDB",
};

export function wellKnownName(port: number): string | undefined {
  return WELL_KNOWN[port];
}

/** Certificate lifetime as a 0–1 fraction elapsed, for the expiry timeline. */
export function certProgress(cert: Certificate): number | undefined {
  if (!cert.not_before || !cert.not_after) return undefined;
  const start = new Date(cert.not_before).getTime();
  const end = new Date(cert.not_after).getTime();
  const span = end - start;
  if (span <= 0) return undefined;
  return Math.min(Math.max((Date.now() - start) / span, 0), 1);
}
