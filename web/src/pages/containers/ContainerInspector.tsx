import { useState } from "react";
import { X } from "lucide-react";
import { useActivity, useContainerDetail } from "../../api/queries";
import type { Container, ContainerDetail } from "../../api/types";
import { Badge, type Tone } from "../../components/Badge";
import { QueryState } from "../../components/QueryState";
import { CopyButton } from "../processes/ProcessInspector";
import { formatAgo, formatBytes, formatDuration, formatValue } from "../../format";
import {
  STANDALONE, effectiveHealth, readExit, type ContainerUsage, type EffectiveHealth,
} from "./containerModel";

/** Falls back when a string field is present but empty.
 *
 *  The API returns "" rather than omitting these — an unset user or working
 *  directory is still a field — so `??` never fires and `||` reads as a bug to
 *  a linter. Being explicit about "empty means unset" says what is meant. */
function orElse(value: string | undefined, fallback: string): string {
  return value === undefined || value === "" ? fallback : value;
}

/** As [orElse], for numeric limits where Docker uses 0 to mean "not set". */
function limitOrElse(value: number, fallback: string): string {
  return value === 0 ? fallback : String(value);
}

const STATE_TONE: Record<Container["state"], Tone> = {
  running: "success",
  restarting: "warning",
  paused: "warning",
  created: "info",
  exited: "neutral",
  dead: "danger",
  removing: "neutral",
};

const HEALTH_TONE: Record<EffectiveHealth, Tone> = {
  healthy: "success",
  unhealthy: "danger",
  starting: "warning",
  none: "neutral",
  unknown: "neutral",
};

type Tab = "overview" | "config" | "network" | "storage" | "events";

/**
 * The right-hand inspector for one container.
 *
 * Configuration — command, restart policy, limits, mounts, networks, ports —
 * is fetched on demand from `GET /api/v1/containers/{id}`, which the server
 * backs with a single inspect call. It is not in the list payload because
 * inspecting every container on every poll would multiply the daemon's load by
 * the container count for fields that change only when a container is
 * recreated.
 *
 * There is no environment tab, and there will not be one. Container
 * environments routinely hold database passwords, API tokens and signing keys;
 * the API does not serve them and this panel could not show them if it wanted
 * to. The same boundary governs process environments on the Processes page.
 * This is a decision, not a gap.
 */
export function ContainerInspector({
  container: c,
  usage,
  onClose,
}: {
  container: Container;
  usage: ContainerUsage | undefined;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<Tab>("overview");
  const detail = useContainerDetail(c.id);
  const health = effectiveHealth(c);

  return (
    <aside
      aria-label={`Container details: ${c.name}`}
      className="elev-3 sticky top-0 flex max-h-[calc(100vh-7rem)] flex-col overflow-hidden rounded-xl"
    >
      <header className="flex items-start justify-between gap-3 border-b border-border p-4">
        <div className="min-w-0">
          <h2 className="truncate text-base font-semibold text-text" title={c.name}>
            {c.name}
          </h2>
          <div className="mt-1 flex flex-wrap items-center gap-1.5">
            <Badge tone={STATE_TONE[c.state]} pulse={c.state === "restarting"}>
              {c.state}
            </Badge>
            {/* State and health are separate axes and are shown as separate
                badges. A running container can be unhealthy; a stopped one has
                no current health at all. */}
            {health !== "unknown" && health !== "none" ? (
              <Badge tone={HEALTH_TONE[health]} pulse={health === "unhealthy"}>
                {health}
              </Badge>
            ) : null}
            <span className="truncate font-mono text-[11px] text-text-subtle">{c.short_id}</span>
          </div>
        </div>
        <button
          type="button"
          onClick={onClose}
          aria-label="Close inspector"
          className="shrink-0 rounded p-1 text-text-muted hover:bg-surface-hover hover:text-text"
        >
          <X size={16} />
        </button>
      </header>

      <nav className="flex flex-wrap gap-1 border-b border-border px-2 py-1.5" role="tablist">
        {(
          [
            ["overview", "Overview"],
            ["config", "Configuration"],
            ["network", "Network"],
            ["storage", "Storage"],
            ["events", "Events"],
          ] as const
        ).map(([id, label]) => (
          <button
            key={id}
            role="tab"
            aria-selected={tab === id}
            type="button"
            onClick={() => { setTab(id); }}
            className={`rounded-md px-2.5 py-1.5 text-xs font-medium whitespace-nowrap transition-colors ${
              tab === id ? "bg-primary/15 text-primary" : "text-text-muted hover:bg-surface-hover"
            }`}
          >
            {label}
          </button>
        ))}
      </nav>

      <div className="scroll-thin flex-1 overflow-y-auto p-4">
        {tab === "overview" ? <Overview container={c} usage={usage} health={health} /> : null}
        {tab === "config" ? <Configuration container={c} query={detail} /> : null}
        {tab === "network" ? <Network query={detail} /> : null}
        {tab === "storage" ? <Storage query={detail} /> : null}
        {tab === "events" ? <Events container={c} /> : null}
      </div>
    </aside>
  );
}

function Overview({
  container: c,
  usage,
  health,
}: {
  container: Container;
  usage: ContainerUsage | undefined;
  health: EffectiveHealth;
}) {
  const exit = readExit(c);
  const running = c.state === "running";

  return (
    <div className="flex flex-col gap-4">
      <dl className="flex flex-col">
        <Field label="State" value={<Badge tone={STATE_TONE[c.state]}>{c.state}</Badge>} />
        <Field
          label="Health"
          value={
            health === "unknown" ? (
              // Not "no healthcheck" and not the last recorded result: the
              // container is not running, so there is no current health.
              <span className="text-text-subtle">not running</span>
            ) : health === "none" ? (
              <span className="text-text-subtle">no healthcheck</span>
            ) : (
              <Badge tone={HEALTH_TONE[health]}>{health}</Badge>
            )
          }
        />
        <Field label="Image" value={<span className="truncate" title={c.image}>{c.image}</span>} />
        <Field label="Project" value={c.compose_project ?? STANDALONE} />
        {c.compose_service ? <Field label="Service" value={c.compose_service} /> : null}
        <Field label="Restarts" value={c.restart_count} />
        {running ? (
          <Field label="Uptime" value={c.uptime_seconds ? formatDuration(c.uptime_seconds) : "—"} />
        ) : null}
        {c.created_at ? (
          <Field label="Created" value={new Date(c.created_at).toLocaleString()} />
        ) : null}
        {!running && c.finished_at ? (
          <Field label="Finished" value={new Date(c.finished_at).toLocaleString()} />
        ) : null}
      </dl>

      {exit.kind !== "none" ? (
        <div className={exit.abnormal ? "surface-alert rounded-lg p-3" : "elev-1 rounded-lg p-3"}>
          <div className="flex items-baseline justify-between gap-2">
            <span className="eyebrow">Exit</span>
            <span className={`font-mono text-xs ${exit.abnormal ? "text-danger" : "text-text-muted"}`}>
              code {exit.code}
            </span>
          </div>
          <p className="mt-1 text-xs leading-relaxed text-text-muted">{exit.reason}</p>
        </div>
      ) : null}

      <section>
        <h3 className="eyebrow mb-2">Live usage</h3>
        {!usage ? (
          <p className="text-xs leading-relaxed text-text-subtle">
            {running
              ? "This container is running but has not reported a stats sample yet."
              : "Resource usage is sampled from the running container. A stopped container has none — this is why the figure is absent rather than zero."}
          </p>
        ) : (
          <dl className="flex flex-col">
            <Field label="CPU" value={usage.cpu !== undefined ? formatValue(usage.cpu, "percent") : "—"} />
            <Field
              label="Memory"
              value={
                usage.memory !== undefined
                  ? `${formatBytes(usage.memory)}${usage.memoryLimit ? ` of ${formatBytes(usage.memoryLimit)}` : " · no limit"}`
                  : "—"
              }
            />
            <Field
              label="Network"
              value={`↓ ${formatValue(usage.netRx ?? 0, "bytes_per_second")} · ↑ ${formatValue(usage.netTx ?? 0, "bytes_per_second")}`}
            />
            <Field
              label="Disk"
              value={`R ${formatValue(usage.blockRead ?? 0, "bytes_per_second")} · W ${formatValue(usage.blockWrite ?? 0, "bytes_per_second")}`}
            />
            <Field label="Processes" value={usage.pids ?? "—"} />
          </dl>
        )}
      </section>
    </div>
  );
}

/** The on-demand configuration tabs share one loading and failure treatment. */
function DetailGate({
  query,
  children,
}: {
  query: ReturnType<typeof useContainerDetail>;
  children: (d: ContainerDetail) => React.ReactNode;
}) {
  if (query.isPending || query.error) {
    return (
      <QueryState
        isPending={query.isPending}
        error={query.error}
        onRetry={() => void query.refetch()}
        rows={4}
      />
    );
  }
  // Narrowed by the guard above: a settled, error-free query has data.
  return <>{children(query.data)}</>;
}

function Configuration({
  container: c,
  query,
}: {
  container: Container;
  query: ReturnType<typeof useContainerDetail>;
}) {
  return (
    <DetailGate query={query}>
      {(d) => (
        <div className="flex flex-col gap-4">
          <dl className="flex flex-col">
            <Field label="Restart policy" value={orElse(d.restart_policy.name, "no")} />
            {d.restart_policy.maximum_retry_count !== undefined &&
            d.restart_policy.maximum_retry_count > 0 ? (
              <Field label="Maximum retries" value={d.restart_policy.maximum_retry_count} />
            ) : null}
            <Field label="User" value={orElse(d.user, "default")} />
            <Field label="Working directory" value={orElse(d.working_dir, "/")} />
            {d.platform ? <Field label="Platform" value={d.platform} /> : null}
            {d.driver ? <Field label="Storage driver" value={d.driver} /> : null}
          </dl>

          <section>
            <h3 className="eyebrow mb-2">Resource limits</h3>
            <dl className="flex flex-col">
              {/* Zero is Docker's "unlimited" and is a real answer worth
                  showing: an unlimited container can exhaust the host. */}
              <Field label="Memory" value={d.limits.memory_bytes ? formatBytes(d.limits.memory_bytes) : "unlimited"} />
              <Field
                label="CPU"
                value={d.limits.nano_cpus ? `${(d.limits.nano_cpus / 1e9).toFixed(2)} cores` : "unlimited"}
              />
              <Field label="CPU shares" value={limitOrElse(d.limits.cpu_shares, "default")} />
              <Field label="PIDs" value={limitOrElse(d.limits.pids_limit, "unlimited")} />
            </dl>
          </section>

          {d.entrypoint?.length || d.command?.length ? (
            <section>
              <div className="mb-1.5 flex items-center justify-between">
                <h3 className="eyebrow">Command</h3>
                <CopyButton value={[...(d.entrypoint ?? []), ...(d.command ?? [])].join(" ")} />
              </div>
              <code className="elev-1 block rounded-lg p-2.5 font-mono text-[11px] leading-relaxed break-all text-text">
                {[...(d.entrypoint ?? []), ...(d.command ?? [])].join(" ")}
              </code>
            </section>
          ) : null}

          <p className="border-t border-border pt-3 text-[11px] leading-relaxed text-text-subtle">
            Environment variables are not collected. They routinely carry database passwords and API
            tokens, and a monitoring tool that reads them makes every viewer a holder of every secret
            on the host.
          </p>

          {Object.keys(c.labels ?? {}).length > 0 ? (
            <section>
              <h3 className="eyebrow mb-2">Labels</h3>
              <dl className="flex flex-col gap-1">
                {Object.entries(c.labels ?? {}).map(([k, v]) => (
                  <div key={k} className="min-w-0">
                    <dt className="truncate font-mono text-[11px] text-text-muted" title={k}>{k}</dt>
                    <dd className="truncate font-mono text-[11px] text-text" title={v}>{v || "—"}</dd>
                  </div>
                ))}
              </dl>
            </section>
          ) : null}
        </div>
      )}
    </DetailGate>
  );
}

function Network({ query }: { query: ReturnType<typeof useContainerDetail> }) {
  return (
    <DetailGate query={query}>
      {(d) => (
        <div className="flex flex-col gap-5">
          <section>
            <h3 className="eyebrow mb-2">Networks</h3>
            {!d.networks?.length ? (
              <p className="text-xs text-text-muted">Not attached to any network.</p>
            ) : (
              <ul className="flex flex-col gap-2">
                {d.networks.map((n) => (
                  <li key={n.name} className="elev-1 rounded-lg p-2.5">
                    <div className="flex items-baseline justify-between gap-2">
                      <span className="truncate text-sm font-medium text-text">{n.name}</span>
                      <span className="shrink-0 font-mono text-[11px] text-text-muted">
                        {orElse(n.ip_address, "no address")}
                      </span>
                    </div>
                    {n.aliases?.length ? (
                      <p className="mt-1 truncate text-[11px] text-text-subtle">
                        aliases: {n.aliases.join(", ")}
                      </p>
                    ) : null}
                  </li>
                ))}
              </ul>
            )}
          </section>

          <section>
            <h3 className="eyebrow mb-2">Ports</h3>
            {!d.ports?.length ? (
              <p className="text-xs text-text-muted">No ports declared.</p>
            ) : (
              <ul className="flex flex-col gap-1.5">
                {d.ports.map((p) => (
                  <li
                    key={`${String(p.container_port)}/${p.protocol}/${p.host_port ?? "none"}`}
                    className="flex items-center gap-2 text-xs"
                  >
                    <span className="font-mono font-semibold text-text">{p.container_port}</span>
                    <span className="eyebrow">{p.protocol}</span>
                    {p.host_port ? (
                      <span className="ml-auto font-mono text-text-muted">
                        {orElse(p.host_ip, "0.0.0.0")}:{p.host_port}
                      </span>
                    ) : (
                      // Exposed but unpublished is a real and materially safer
                      // state than bound to a host interface.
                      <span className="ml-auto text-text-subtle">exposed, not published</span>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </section>
        </div>
      )}
    </DetailGate>
  );
}

function Storage({ query }: { query: ReturnType<typeof useContainerDetail> }) {
  return (
    <DetailGate query={query}>
      {(d) =>
        !d.mounts?.length ? (
          <p className="py-6 text-center text-xs text-text-muted">
            No volumes or bind mounts. This container's filesystem is entirely its image layers plus
            its writable layer, so nothing it writes survives being recreated.
          </p>
        ) : (
          <ul className="flex flex-col gap-2">
            {d.mounts.map((m) => (
              <li key={m.destination} className="elev-1 rounded-lg p-2.5">
                <div className="mb-1 flex items-center gap-2">
                  <span className="eyebrow">{m.type}</span>
                  <Badge tone={m.read_write ? "neutral" : "info"}>
                    {m.read_write ? "read-write" : "read-only"}
                  </Badge>
                </div>
                <p className="truncate font-mono text-[11px] text-text" title={m.destination}>
                  {m.destination}
                </p>
                <p className="truncate font-mono text-[11px] text-text-subtle" title={m.name ?? m.source}>
                  ← {m.name ?? m.source ?? "anonymous"}
                </p>
              </li>
            ))}
          </ul>
        )
      }
    </DetailGate>
  );
}

/**
 * Daemon events mentioning this container.
 *
 * Filtered from the shared activity feed, which the server holds in memory and
 * which therefore starts empty after a restart — stated plainly rather than
 * implying nothing has ever happened to this container.
 */
function Events({ container: c }: { container: Container }) {
  const activity = useActivity(60);
  const entries = (activity.data?.entries ?? []).filter(
    // `detail` is optional, so its `includes` yields boolean | undefined —
    // compared explicitly rather than leaned on for truthiness.
    (e) =>
      e.title.includes(c.name) ||
      e.detail?.includes(c.name) === true ||
      e.detail?.includes(c.short_id) === true,
  );

  if (activity.isPending) {
    return <QueryState isPending rows={3} />;
  }

  if (entries.length === 0) {
    return (
      <p className="py-6 text-center text-xs leading-relaxed text-text-muted">
        No recorded events mention this container. The feed is held in memory and starts empty when
        Atlas restarts, so this is not the same as nothing having happened.
      </p>
    );
  }

  return (
    <ul className="flex flex-col gap-2">
      {entries.map((e) => (
        <li key={e.id} className="elev-1 rounded-lg p-2.5">
          <div className="flex items-baseline justify-between gap-2">
            <span className="truncate text-xs font-medium text-text">{e.title}</span>
            <span className="shrink-0 text-[11px] text-text-subtle">
              {formatAgo((Date.now() - new Date(e.time).getTime()) / 1000)}
            </span>
          </div>
          {e.detail ? <p className="mt-0.5 text-[11px] text-text-muted">{e.detail}</p> : null}
        </li>
      ))}
    </ul>
  );
}

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3 border-b border-border py-2.5 text-sm last:border-0">
      <dt className="shrink-0 text-text-muted">{label}</dt>
      <dd className="min-w-0 truncate text-right text-text">{value}</dd>
    </div>
  );
}
