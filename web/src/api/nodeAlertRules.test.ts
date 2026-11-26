import { MutationObserver, QueryClient } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { nodeAlertRuleKeys, nodeAlertRuleMutation } from "./queries";
import type { ListNodeAlertRulesResponse, NodeAlertRule } from "./types";

const RULE: NodeAlertRule = {
  key: "disk_pct", area: "resources", title: "Disk usage", description: "d", default_severity: "warning",
  default_enabled: true, global_enabled: true, override: null, enabled: true, source: "global",
};
const LIST: ListNodeAlertRulesResponse = { node_id: "n1", rules: [RULE] };

function setup(respond: () => Promise<Response>) {
  const fetchMock = vi.fn(respond);
  vi.stubGlobal("fetch", fetchMock);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  client.setQueryData(nodeAlertRuleKeys.node("n1"), LIST);
  const seen: (NodeAlertRule | undefined)[] = [];
  const observer = new MutationObserver(client, {
    ...nodeAlertRuleMutation(client, "n1"),
    mutationFn: async (vars: { ruleKey: string; override: boolean | null }) => {
      seen.push(client.getQueryData<ListNodeAlertRulesResponse>(nodeAlertRuleKeys.node("n1"))?.rules[0]);
      await nodeAlertRuleMutation(client, "n1").mutationFn(vars);
    },
  });
  const current = () => client.getQueryData<ListNodeAlertRulesResponse>(nodeAlertRuleKeys.node("n1"))?.rules[0];
  return { fetchMock, observer, seen, current };
}

afterEach(() => { vi.unstubAllGlobals(); });

describe("nodeAlertRuleMutation", () => {
  it("shows the new value at once and sends it", async () => {
    const { fetchMock, observer, seen } = setup(() =>
      Promise.resolve(new Response(JSON.stringify({ ...RULE, override: false, enabled: false, source: "node" }), { status: 200 })),
    );
    await observer.mutate({ ruleKey: "disk_pct", override: false });
    expect(seen[0]).toMatchObject({ enabled: false, source: "node" });
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toContain("/nodes/n1/alert-rules/disk_pct");
    expect(init.method).toBe("PUT");
    expect(init.body).toBe(JSON.stringify({ enabled: false }));
  });

  it("puts the list back when the server refuses", async () => {
    const { observer, seen, current } = setup(() =>
      Promise.resolve(new Response(JSON.stringify({ error: { code: "forbidden", message: "no" } }), { status: 403 })),
    );
    await expect(observer.mutate({ ruleKey: "disk_pct", override: false })).rejects.toBeTruthy();
    expect(seen[0]?.enabled).toBe(false);
    expect(current()).toEqual(RULE);
  });

  it("resets with DELETE", async () => {
    const { fetchMock, observer } = setup(() => Promise.resolve(new Response(null, { status: 204 })));
    await observer.mutate({ ruleKey: "disk_pct", override: null });
    expect((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].method).toBe("DELETE");
  });
});
