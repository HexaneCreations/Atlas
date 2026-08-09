import { describe, expect, it } from "vitest";
import { containerKeys, containersPath } from "./queries";

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
