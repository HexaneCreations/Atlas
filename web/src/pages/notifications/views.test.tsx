import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { isValidElement, type ReactElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import { ApiError } from "../../api/client";
import { notificationKeys } from "../../api/queries";
import type { NotificationChannel, NotificationDelivery } from "../../api/types";
import { ToastProvider } from "../../components/Toast";
import { NotificationsPage } from "../NotificationsPage";
import { ChannelFields, DeleteConfirm, FormButtons, FormStatus } from "./ChannelForm";
import { ChannelList } from "./ChannelList";
import { DeliveryList } from "./DeliveryList";
import { toDraft } from "./channelModel";

const noop = () => undefined;

function elements(node: ReactNode): ReactElement<Record<string, unknown>>[] {
  if (Array.isArray(node)) return node.flatMap(elements);
  if (!isValidElement<Record<string, unknown>>(node)) return [];
  return [node, ...elements(node.props.children as ReactNode)];
}

function button(tree: ReactNode, label: string) {
  const found = elements(tree).find((e) => e.props.children === label);
  if (!found) throw new Error(`no button labelled ${label}`);
  return found.props as { onClick: () => void; disabled?: boolean };
}

function channel(over: Partial<NotificationChannel> = {}): NotificationChannel {
  return {
    id: "c1", name: "ops hook", type: "webhook", enabled: true, min_severity: "critical",
    config: { template: "{{.msg}}" }, secret_configured: true, monitor_default: false, destination_host: "hooks.example.com",
    header_names: ["X-Api"], created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", ...over,
  };
}

function renderPage(seed: (client: QueryClient) => void): string {
  const client = new QueryClient({ defaultOptions: { queries: { retryOnMount: false, staleTime: Infinity } } });
  client.setQueryData([...notificationKeys.deliveries, 25], { deliveries: [], total: 0 });
  seed(client);
  return renderToStaticMarkup(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/notifications"]}>
        <ToastProvider><NotificationsPage /></ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function failChannels(client: QueryClient, error: Error) {
  client.getQueryCache().build(client, { queryKey: notificationKeys.channels }).setState({ status: "error", error });
}

describe("NotificationsPage", () => {
  it("lists channels without anything from the secret but the host", () => {
    const html = renderPage((c) => c.setQueryData(notificationKeys.channels, {
      channels: [channel(), channel({ id: "c2", name: "quiet", type: "teams", enabled: false, secret_configured: false, destination_host: "", min_severity: "warning" })],
      total: 2,
    }));
    for (const text of ["ops hook", "Webhook", "Enabled", "Critical", "hooks.example.com", "Secret configured", "quiet", "Microsoft Teams", "Disabled", "Warning", "No secret configured", "Add notification", "Recent deliveries"]) {
      expect(html).toContain(text);
    }
  });

  it("has an empty state", () => {
    expect(renderPage((c) => c.setQueryData(notificationKeys.channels, { channels: [], total: 0 }))).toContain("No notifications yet");
  });

  it("explains a missing encryption key, and offers nothing to add", () => {
    const html = renderPage((c) => {
      failChannels(c, new ApiError(503, { code: "unavailable", message: "notifications are disabled: no encryption key is configured" }));
    });
    expect(html).toContain("Notifications are disabled");
    expect(html).toContain("encryption key");
    expect(html).not.toContain("This query failed");
    expect(html).not.toContain("Add notification");
  });

  it("shows any other 503 as a failed query", () => {
    const html = renderPage((c) => { failChannels(c, new ApiError(503, { code: "unavailable", message: "db down" })); });
    expect(html).toContain("This query failed");
    expect(html).not.toContain("Notifications are disabled");
  });
});

describe("ChannelFields", () => {
  const render = (c: NotificationChannel | null, over = {}, errors = {}) =>
    renderToStaticMarkup(<ChannelFields draft={{ ...toDraft(c), ...over }} channel={c} errors={errors} onChange={noop} />);

  it("offers Teams and Webhook, with Email disabled", () => {
    expect(render(null)).toContain('<option value="email" disabled="">Email (coming soon)</option>');
  });

  it("shows the body and headers for a webhook only", () => {
    expect(render(null)).not.toContain("Request body");
    const html = render(null, { type: "webhook" });
    for (const text of ["Request body", "Additional headers", "Reset to default", "msg, eventJSON, nodeJSON, monitorJSON, ruleJSON", "Select for new websites"]) {
      expect(html).toContain(text);
    }
  });

  it("is write-only for a saved channel", () => {
    const html = render(channel());
    expect(html).toMatch(/<input id="channel-url"[^>]* value=""/);
    expect(html).toContain("https://hooks.example.com/…");
    expect(html).toContain("Leave empty to keep the current URL");
    expect(html).toContain("{\n  &quot;X-Api&quot;: &quot;&quot;\n}");
    expect(html).toContain("Leave a value empty to keep it");
    expect(html).toMatch(/<select id="channel-type" disabled=""/);
  });

  it("says when the stored secret cannot be read", () => {
    const html = render(channel({ destination_host: "", header_names: [] }));
    expect(html).toContain("cannot be read");
    expect(html).not.toContain("Leave empty to keep");
  });

  it("labels every control and ties each error to its field", () => {
    const html = render(null, { type: "webhook" }, { url: "bad url" });
    for (const id of ["type", "name", "url", "template", "headers", "severity"]) {
      expect(html).toContain(`for="channel-${id}"`);
      expect(html).toContain(`id="channel-${id}"`);
    }
    expect(html).toMatch(/<p id="channel-url-error" aria-live="polite"[^>]*>bad url</);
    expect(html).toMatch(/<input id="channel-url"[^>]* aria-invalid="true" aria-describedby="channel-url-help channel-url-error"/);
  });

  it("is enabled for all nodes, with no per-node assignment", () => {
    const html = render(null);
    expect(html).toContain("Enabled for all nodes");
    expect(html).toContain("use Mute on the Nodes page");
    expect(html).not.toMatch(/apply on all/i);
  });

  it("resets the body to the default", () => {
    const onChange = vi.fn();
    const tree = ChannelFields({ draft: { ...toDraft(channel()), template: "x" }, channel: channel(), errors: {}, onChange });
    const action = elements(tree).map((e) => e.props.action).find(isValidElement<Record<string, unknown>>);
    (action?.props.onClick as () => void)();
    expect((onChange.mock.calls[0]?.[0] as { template: string }).template).toContain("{{.msg}}");
  });
});

describe("the form's actions", () => {
  const props = { existing: true, saving: false, testing: false, wait: 0, onDelete: noop, onTest: noop };

  it("deletes only an existing channel", () => {
    expect(renderToStaticMarkup(<FormButtons {...props} />)).toContain("Delete");
    expect(renderToStaticMarkup(<FormButtons {...props} existing={false} />)).not.toContain("Delete");
  });

  it("disables Test for the countdown, and while testing", () => {
    expect(button(FormButtons(props), "Test").disabled).toBe(false);
    expect(button(FormButtons({ ...props, wait: 4 }), "Test (4s)").disabled).toBe(true);
    expect(button(FormButtons({ ...props, testing: true }), "Testing…").disabled).toBe(true);
  });

  it("says how a test went", () => {
    expect(renderToStaticMarkup(<FormStatus error={undefined} test={{ kind: "ok" }} wait={0} />)).toContain("Test sent");
    expect(renderToStaticMarkup(
      <FormStatus error={undefined} test={{ kind: "failed", message: "the destination returned status 500" }} wait={0} />,
    )).toContain("Test failed: the destination returned status 500");
    expect(renderToStaticMarkup(<FormStatus error={undefined} test={{ kind: "limited", retryAt: 1 }} wait={3} />)).toContain("Too many tests");
  });

  it("asks before deleting", () => {
    const onDelete = vi.fn();
    button(FormButtons({ ...props, onDelete }), "Delete").onClick();
    expect(onDelete).toHaveBeenCalledOnce();

    const onConfirm = vi.fn();
    const onClose = vi.fn();
    const tree = DeleteConfirm({ name: "ops hook", pending: false, error: null, onClose, onConfirm });
    button(tree, "Cancel").onClick();
    expect(onClose).toHaveBeenCalledOnce();
    expect(onConfirm).not.toHaveBeenCalled();
    button(tree, "Delete").onClick();
    expect(onConfirm).toHaveBeenCalledOnce();
    expect(renderToStaticMarkup(tree)).toContain("ops hook");
  });
});

describe("the lists", () => {
  it("opens a channel for editing", () => {
    const onEdit = vi.fn();
    button(ChannelList({ channels: [channel()], onEdit }), "Edit").onClick();
    expect(onEdit).toHaveBeenCalledWith(channel());
  });

  it("shows deliveries read-only, without a destination's own words", () => {
    const d: NotificationDelivery = {
      id: "d1", event_id: "e1", channel_id: "c1", channel_type: "webhook", kind: "firing", trigger: "node_down",
      status: "failed", attempts: 3, last_error: "webhook destination returned status 500: T0KEN",
      created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z",
    };
    const html = renderToStaticMarkup(<DeliveryList deliveries={[d, { ...d, id: "d2", channel_id: "gone" }]} channelNames={new Map([["c1", "ops hook"]])} />);
    for (const text of ["ops hook", "Webhook", "firing", "node_down", "failed", "The destination returned status 500"]) expect(html).toContain(text);
    const recovered = renderToStaticMarkup(
      <DeliveryList deliveries={[{ ...d, kind: "recovered", title: "Container exited" }]} channelNames={new Map()} />,
    );
    expect(recovered).toContain("Recovered: Container exited");
    expect(recovered).toContain(">recovered<");
    expect(html).not.toContain("T0KEN");
    expect(html).not.toContain("<button");
  });
});
