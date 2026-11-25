import { describe, expect, it } from "vitest";
import type { Node } from "../../api/types";
import { compare, matchesSearch } from "./useNodeTable";

function node(over: Partial<Node> = {}): Node {
  return {
    node_id: "node-1", hostname: "host-1", status: "up",
    first_seen_at: "2026-01-01T00:00:00Z", last_seen_at: "2026-01-01T00:00:00Z",
    seconds_since_seen: 5,
    ...over,
  };
}

describe("search matches the IP", () => {
  // Addresses throughout are from the documentation ranges (RFC 5737 /
  // RFC 2544), never a real one from the repo or a fixture.
  it("matches a full public IP", () => {
    expect(matchesSearch(node({ public_ip: "203.0.113.42" }), "203.0.113.42")).toBe(true);
  });

  it("matches a partial, case-insensitive substring of the IP", () => {
    expect(matchesSearch(node({ public_ip: "198.51.100.17" }), "51.100")).toBe(true);
  });

  it("does not match an unrelated query against a node with no observed IP", () => {
    expect(matchesSearch(node(), "203.0.113")).toBe(false);
  });

  // The query still has to match *something* — searching an address present
  // on another node must not surface every node with no address at all.
  it("a query that only matches another node's IP excludes this node", () => {
    const n = node({ hostname: "host-1", node_id: "node-1" });
    expect(matchesSearch(n, "198.51.100.17")).toBe(false);
  });

  it("still matches on hostname, node ID, platform, and kernel as before", () => {
    expect(matchesSearch(node({ hostname: "web-03" }), "web-03")).toBe(true);
    expect(matchesSearch(node({ node_id: "abc123" }), "abc123")).toBe(true);
    expect(matchesSearch(node({ platform: "ubuntu" }), "ubuntu")).toBe(true);
    expect(matchesSearch(node({ kernel: "6.8.0-generic" }), "6.8.0")).toBe(true);
  });
});

describe("sorting by IP", () => {
  it("orders ascending by address", () => {
    const a = node({ node_id: "a", public_ip: "198.51.100.5" });
    const b = node({ node_id: "b", public_ip: "203.0.113.1" });
    expect(compare(a, b, "ip")).toBeLessThan(0);
    expect(compare(b, a, "ip")).toBeGreaterThan(0);
  });

  it("is stable (zero) comparing a node against itself", () => {
    const a = node({ public_ip: "203.0.113.42" });
    expect(compare(a, a, "ip")).toBe(0);
  });

  // The empty-value case: a node with no observed IP at all (no connection
  // yet, or the local node) must still sort deterministically rather than
  // throwing or comparing undefined against a string.
  describe("empty value", () => {
    it("treats an unobserved IP as sorting before any real address", () => {
      const noIP = node({ node_id: "no-ip" });
      const withIP = node({ node_id: "with-ip", public_ip: "203.0.113.1" });
      expect(compare(noIP, withIP, "ip")).toBeLessThan(0);
      expect(compare(withIP, noIP, "ip")).toBeGreaterThan(0);
    });

    it("two nodes with no observed IP compare equal", () => {
      const a = node({ node_id: "a" });
      const b = node({ node_id: "b" });
      expect(compare(a, b, "ip")).toBe(0);
    });
  });
});
