import type { LatestValue } from "../../api/types";

/**
 * Reading a node's live vitals out of its latest-metric values.
 *
 * Two availability classes run through this whole page and they must not be
 * conflated:
 *
 *   *Inventory* — cores, kernel, architecture, uptime at last contact — comes
 *   from the node record. It is last-known and survives the node going away,
 *   so it is shown for every node including ones that are down.
 *
 *   *Vitals* — CPU, memory, storage, network, load — comes from metrics, which
 *   the API resolves over a five-minute window. A node that stopped reporting
 *   returns nothing at all, and the UI must say "not reporting" rather than
 *   render zeroes.
 *
 * These helpers are deliberately node-scoped. The inventory endpoints
 * (`/containers`, `/ports`, `/processes`, `/mounts`) describe the host Atlas
 * itself runs on and are *not* per-node, so using them to describe an
 * arbitrary node would attribute the local machine's containers and sockets
 * to a remote one. Everything below reads `/metrics/latest?node=…` instead,
 * which is genuinely per-node and therefore correct for any of them.
 */

export interface Vitals {
  /** False when the node returned no metrics at all — it is not reporting. */
  reporting: boolean;
  cpu?: number | undefined;
  cores?: number | undefined;
  /** Per-core utilisation, ordered by core index. */
  coreUsage: { core: string; value: number }[];
  memoryPercent?: number | undefined;
  memoryTotal?: number | undefined;
  memoryUsed?: number | undefined;
  swapPercent?: number | undefined;
  swapTotal?: number | undefined;
  load1?: number | undefined;
  load5?: number | undefined;
  load15?: number | undefined;
  loadPerCore1?: number | undefined;
  uptime?: number | undefined;
  /** Worst filesystem by used percentage. */
  disk?: Filesystem | undefined;
  filesystems: Filesystem[];
  interfaces: Interface[];
  /** Aggregate throughput across every interface. */
  netRx?: number | undefined;
  netTx?: number | undefined;
  containersTotal?: number | undefined;
  containersRunning?: number | undefined;
  portsListening?: number | undefined;
  processesTotal?: number | undefined;
  zombies?: number | undefined;
}

export interface Filesystem {
  mountpoint: string;
  device?: string | undefined;
  fstype?: string | undefined;
  usedPercent: number;
  total?: number | undefined;
  used?: number | undefined;
  free?: number | undefined;
  inodesPercent?: number | undefined;
}

export interface Interface {
  name: string;
  rx: number;
  tx: number;
  rxErrors: number;
  txErrors: number;
  rxDropped: number;
  txDropped: number;
}

/** Reads one unlabelled metric. */
function scalar(values: LatestValue[], metric: string): number | undefined {
  return values.find((v) => v.metric === metric && !hasLabels(v))?.value;
}

/** Reads a metric filtered to one label value. */
function byLabel(
  values: LatestValue[],
  metric: string,
  key: string,
  label: string,
): number | undefined {
  return values.find((v) => v.metric === metric && v.labels?.[key] === label)?.value;
}

function hasLabels(v: LatestValue): boolean {
  return Object.keys(v.labels ?? {}).length > 0;
}

function sumBy(values: LatestValue[], metric: string): number | undefined {
  const parts = values.filter((v) => v.metric === metric);
  if (parts.length === 0) return undefined;
  return parts.reduce((s, v) => s + v.value, 0);
}

export function readVitals(values: LatestValue[]): Vitals {
  if (values.length === 0) {
    return { reporting: false, coreUsage: [], filesystems: [], interfaces: [] };
  }

  const filesystems = readFilesystems(values);
  const interfaces = readInterfaces(values);

  // The fullest filesystem is the one that matters: a node with nine mounts
  // is healthy only as far as its worst one.
  const disk = [...filesystems].sort((a, b) => b.usedPercent - a.usedPercent)[0];

  const coreUsage = values
    .filter((v) => v.metric === "system.cpu.core.usage" && v.labels?.core)
    .map((v) => ({ core: v.labels?.core ?? "", value: v.value }))
    .sort((a, b) => collateNumeric(a.core, b.core));

  return {
    reporting: true,
    cpu: scalar(values, "system.cpu.usage"),
    cores: scalar(values, "system.cpu.cores"),
    coreUsage,
    memoryPercent: scalar(values, "system.memory.usage"),
    memoryTotal: scalar(values, "system.memory.total"),
    memoryUsed: scalar(values, "system.memory.used"),
    swapPercent: scalar(values, "system.swap.usage"),
    swapTotal: scalar(values, "system.swap.total"),
    load1: byLabel(values, "system.load.average", "window", "1m"),
    load5: byLabel(values, "system.load.average", "window", "5m"),
    load15: byLabel(values, "system.load.average", "window", "15m"),
    loadPerCore1: byLabel(values, "system.load.per_core", "window", "1m"),
    uptime: scalar(values, "system.uptime"),
    ...(disk ? { disk } : {}),
    filesystems,
    interfaces,
    netRx: sumBy(values, "system.network.rx.bytes"),
    netTx: sumBy(values, "system.network.tx.bytes"),
    containersTotal: scalar(values, "docker.containers.total"),
    containersRunning: byLabel(values, "docker.containers.count", "state", "running"),
    portsListening: scalar(values, "port.listening.total"),
    processesTotal: scalar(values, "process.total"),
    zombies: scalar(values, "process.zombies"),
  };
}

/**
 * Filesystems, one row per mountpoint.
 *
 * Capacity arrives as six separate metrics sharing a mountpoint label rather
 * than as one record, so they are joined here. Deduplication by capacity
 * signature is deliberate and matches the Disks page: APFS presents a single
 * container as several synthetic volumes with identical totals, and counting
 * them would report the same disk filling up four times.
 */
function readFilesystems(values: LatestValue[]): Filesystem[] {
  const byMount = new Map<string, Filesystem>();

  for (const v of values) {
    const mountpoint = v.labels?.mountpoint;
    if (!mountpoint) continue;

    const fs = byMount.get(mountpoint) ?? { mountpoint, usedPercent: 0 };
    if (v.labels?.device) fs.device = v.labels.device;
    if (v.labels?.fstype) fs.fstype = v.labels.fstype;

    switch (v.metric) {
      case "system.disk.usage": fs.usedPercent = v.value; break;
      case "system.disk.total": fs.total = v.value; break;
      case "system.disk.used": fs.used = v.value; break;
      case "system.disk.free": fs.free = v.value; break;
      case "system.disk.inodes.usage": fs.inodesPercent = v.value; break;
      default: break;
    }
    byMount.set(mountpoint, fs);
  }

  const pools = new Map<string, Filesystem>();
  for (const fs of byMount.values()) {
    const key = `${String(fs.total ?? 0)}:${String(fs.free ?? 0)}`;
    const existing = pools.get(key);
    // Keep the shortest mountpoint of a pool: "/" reads better than
    // "/System/Volumes/Update/mnt1" for the same physical device.
    if (!existing || fs.mountpoint.length < existing.mountpoint.length) pools.set(key, fs);
  }

  return [...pools.values()].sort((a, b) => b.usedPercent - a.usedPercent);
}

function readInterfaces(values: LatestValue[]): Interface[] {
  const byName = new Map<string, Interface>();

  const put = (name: string, field: keyof Omit<Interface, "name">, value: number) => {
    const i = byName.get(name) ?? {
      name, rx: 0, tx: 0, rxErrors: 0, txErrors: 0, rxDropped: 0, txDropped: 0,
    };
    i[field] = value;
    byName.set(name, i);
  };

  for (const v of values) {
    const name = v.labels?.interface;
    if (!name) continue;
    switch (v.metric) {
      case "system.network.rx.bytes": put(name, "rx", v.value); break;
      case "system.network.tx.bytes": put(name, "tx", v.value); break;
      case "system.network.rx.errors": put(name, "rxErrors", v.value); break;
      case "system.network.tx.errors": put(name, "txErrors", v.value); break;
      case "system.network.rx.dropped": put(name, "rxDropped", v.value); break;
      case "system.network.tx.dropped": put(name, "txDropped", v.value); break;
      default: break;
    }
  }

  // Busiest first: an idle loopback should not head the list.
  return [...byName.values()].sort((a, b) => b.rx + b.tx - (a.rx + a.tx));
}

/** Sorts "0","1","10","2" as 0,1,2,10 — core labels are numeric strings. */
function collateNumeric(a: string, b: string): number {
  const na = Number(a);
  const nb = Number(b);
  if (Number.isFinite(na) && Number.isFinite(nb)) return na - nb;
  return a.localeCompare(b);
}
