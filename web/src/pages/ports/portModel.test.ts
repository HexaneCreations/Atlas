import { describe, expect, it } from "vitest";
import type { Port } from "../../api/types";
import {
  exposureOf, groupByProcess, readPosture, tlsNeedsAttention, tlsStateOf, wellKnownName,
} from "./portModel";

function port(over: Partial<Port> = {}): Port {
  return { protocol: "tcp", address: "127.0.0.1", port: 8080, tls_probed: true, ...over };
}

describe("exposure", () => {
  it("classifies wildcard binds as reachable on every interface", () => {
    expect(exposureOf(port({ address: "0.0.0.0" }))).toBe("world");
    expect(exposureOf(port({ address: "::" }))).toBe("world");
    // lsof prints a bare asterisk on macOS and the BSDs.
    expect(exposureOf(port({ address: "*" }))).toBe("world");
  });

  it("classifies loopback in both address families", () => {
    expect(exposureOf(port({ address: "127.0.0.1" }))).toBe("loopback");
    expect(exposureOf(port({ address: "::1" }))).toBe("loopback");
    // The whole 127/8 block is loopback, not just .0.1.
    expect(exposureOf(port({ address: "127.0.1.5" }))).toBe("loopback");
  });

  it("classifies a specific interface address as network-reachable", () => {
    expect(exposureOf(port({ address: "192.168.1.10" }))).toBe("network");
  });
});

describe("TLS state", () => {
  // Probing is budgeted and TCP-only, so "no certificate" means either
  // plaintext or never looked at. Conflating them reports absence of evidence
  // as evidence of absence.
  it("distinguishes an unprobed socket from a plaintext one", () => {
    expect(tlsStateOf(port({ tls_probed: false }))).toBe("unprobed");
    expect(tlsStateOf(port({ tls_probed: true }))).toBe("plaintext");
  });

  it("ranks expiry, self-signature and validity", () => {
    expect(tlsStateOf(port({ tls: { days_until_expiry: -1, expired: true, self_signed: false } })))
      .toBe("expired");
    expect(tlsStateOf(port({ tls: { days_until_expiry: 400, expired: false, self_signed: true } })))
      .toBe("self-signed");
    expect(tlsStateOf(port({ tls: { days_until_expiry: 10, expired: false, self_signed: false } })))
      .toBe("expiring");
    expect(tlsStateOf(port({ tls: { days_until_expiry: 200, expired: false, self_signed: false } })))
      .toBe("valid");
  });

  // Expiry outranks self-signature: an expired self-signed certificate is
  // expired first, and renewing it is the action.
  it("reports an expired self-signed certificate as expired", () => {
    expect(tlsStateOf(port({ tls: { days_until_expiry: -5, expired: true, self_signed: true } })))
      .toBe("expired");
  });
});

describe("what deserves attention", () => {
  // Plaintext on loopback is the norm almost everywhere. Flagging it would
  // bury the one exposed plaintext socket under twenty that do not matter.
  it("ignores plaintext on loopback", () => {
    expect(tlsNeedsAttention(port({ address: "127.0.0.1", tls_probed: true }))).toBe(false);
  });

  it("flags plaintext on a wildcard bind", () => {
    expect(tlsNeedsAttention(port({ address: "0.0.0.0", tls_probed: true }))).toBe(true);
  });

  // An unprobed socket is unknown, and an unknown is not a finding.
  it("never flags an unprobed socket", () => {
    expect(tlsNeedsAttention(port({ address: "0.0.0.0", tls_probed: false }))).toBe(false);
  });

  it("flags expiry regardless of exposure", () => {
    expect(
      tlsNeedsAttention(
        port({ address: "127.0.0.1", tls: { days_until_expiry: 5, expired: false, self_signed: false } }),
      ),
    ).toBe(true);
  });

  it("flags a self-signed certificate only where something can reach it", () => {
    const cert = { days_until_expiry: 300, expired: false, self_signed: true };
    expect(tlsNeedsAttention(port({ address: "127.0.0.1", tls: cert }))).toBe(false);
    expect(tlsNeedsAttention(port({ address: "0.0.0.0", tls: cert }))).toBe(true);
  });
});

describe("posture", () => {
  it("counts probe coverage against TCP only", () => {
    const posture = readPosture([
      port({ protocol: "tcp", tls_probed: true }),
      port({ protocol: "tcp", port: 81, tls_probed: false }),
      // UDP is never probed and must not drag the denominator down.
      port({ protocol: "udp", port: 53, tls_probed: false }),
    ]);

    expect(posture.probeable).toBe(2);
    expect(posture.probed).toBe(1);
    expect(posture.udp).toBe(1);
  });

  it("reports exposed plaintext as the combination worth reviewing", () => {
    const posture = readPosture([
      port({ address: "0.0.0.0", port: 6379, tls_probed: true }),
      port({ address: "127.0.0.1", port: 5432, tls_probed: true }),
    ]);

    expect(posture.exposedPlaintext).toHaveLength(1);
    expect(posture.exposedPlaintext[0]?.port).toBe(6379);
  });

  it("counts sockets whose owner could not be resolved", () => {
    const posture = readPosture([port({ pid: 42 }), port({ port: 99 })]);
    expect(posture.unattributed).toHaveLength(1);
  });
});

describe("process ownership", () => {
  // Grouping by pid produced a dozen near-identical rows for one browser and
  // disagreed with the explorer's name-based filter.
  it("groups by program name, keeping every pid", () => {
    const groups = groupByProcess([
      port({ port: 1, process: "Code Helper", pid: 10 }),
      port({ port: 2, process: "Code Helper", pid: 11 }),
      port({ port: 3, process: "Code Helper", pid: 12 }),
    ]);

    expect(groups).toHaveLength(1);
    expect(groups[0]?.pids).toEqual([10, 11, 12]);
    expect(groups[0]?.ports).toHaveLength(3);
  });

  // The process to look at is the one reachable from furthest away, not the
  // one holding the most sockets.
  it("sorts widest exposure first, not most sockets", () => {
    const groups = groupByProcess([
      port({ port: 1, process: "local-tool", pid: 1, address: "127.0.0.1" }),
      port({ port: 2, process: "local-tool", pid: 1, address: "127.0.0.1" }),
      port({ port: 3, process: "local-tool", pid: 1, address: "127.0.0.1" }),
      port({ port: 4, process: "exposed", pid: 2, address: "0.0.0.0" }),
    ]);

    expect(groups[0]?.process).toBe("exposed");
    expect(groups[0]?.worst).toBe("world");
  });

  it("groups unattributed sockets rather than dropping them", () => {
    const groups = groupByProcess([port({ port: 1 })]);
    expect(groups[0]?.process).toBe("unattributed");
    expect(groups[0]?.pids).toHaveLength(0);
  });
});

describe("well-known ports", () => {
  it("names ports whose number carries a convention", () => {
    expect(wellKnownName(5432)).toBe("PostgreSQL");
    expect(wellKnownName(6379)).toBe("Redis");
  });

  it("returns nothing for an arbitrary high port", () => {
    expect(wellKnownName(51234)).toBeUndefined();
  });
});
