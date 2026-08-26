import { describe, expect, it } from "vitest";
import { containerKeys, containersPath, selectPrimaryNodeID } from "./queries";

// Regression: useContainers used to fetch "/containers" with no node param,
// which silently resolved to the control plane's own host — never the
// selected node — and read as "Docker is not available" the moment that
// host's Docker differed from (or was absent, unlike) the selected node's.
describe("containersPath", () => {
  it("scopes the request to the given node", () => {
    expect(containersPath("linux-relay-poc-node")).toBe("/containers?node=linux-relay-poc-node");
  });

  it("url-encodes the node id", () => {
    expect(containersPath("node with spaces")).toBe("/containers?node=node%20with%20spaces");
  });

  it("still issues a request with no node selected, scoped to nothing rather than silently defaulting", () => {
    expect(containersPath(undefined)).toBe("/containers?node=");
  });
});

describe("containerKeys.list", () => {
  it("keys the cache per node, so switching nodes cannot show a stale cache from a different one", () => {
    expect(containerKeys.list("node-a")).not.toEqual(containerKeys.list("node-b"));
  });
});

// Regression: a node-scoped viewer (granted exactly one node, not
// fleet-wide) used to default onto the control-plane's own node regardless
// of whether they could see it — every per-node page then 403'd against a
// node the operator never chose. GET /nodes is now filtered server-side to
// only the caller's authorized nodes, so `visibleNodes` here is already
// that filtered set; the default must never reach outside it.
describe("selectPrimaryNodeID", () => {
  it("prefers an explicit selection over anything else", () => {
    const visible = [{ node_id: "node-a" }, { node_id: "node-b" }];
    expect(selectPrimaryNodeID(visible, "node-b", "node-a")).toBe("node-a");
  });

  it("prefers the collector's node when it is one of the visible nodes", () => {
    const visible = [{ node_id: "node-a" }, { node_id: "node-b" }];
    expect(selectPrimaryNodeID(visible, "node-b", null)).toBe("node-b");
  });

  it("falls back to the first visible node when the collector's node is not visible — the single-node-scoped-viewer case", () => {
    const visible = [{ node_id: "cyrene-dev-v2" }];
    expect(selectPrimaryNodeID(visible, "828705f9daa5", null)).toBe("cyrene-dev-v2");
  });

  it("never defaults onto the collector's node for a viewer who cannot see it, even with several visible nodes", () => {
    const visible = [{ node_id: "node-a" }, { node_id: "node-b" }];
    expect(selectPrimaryNodeID(visible, "control-plane-host", null)).toBe("node-a");
  });

  it("returns undefined rather than a stale pick when no nodes are visible at all", () => {
    expect(selectPrimaryNodeID([], "control-plane-host", null)).toBeUndefined();
  });
});
