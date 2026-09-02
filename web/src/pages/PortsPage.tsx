import { useMemo, useState } from "react";
import { ArrowDown, ArrowUp, ChevronsUpDown } from "lucide-react";
import { usePorts, usePrimaryNodeID } from "../api/queries";
import { ApiError, inventoryGapKind } from "../api/client";
import { AgentSubjectGap } from "../components/AgentSubjectGap";
import type { Port } from "../api/types";
import { emptyArray } from "../api/empty";
import { Card } from "../components/Card";
import { EmptyState, EmptyAction } from "../components/EmptyState";
import { Badge, type Tone } from "../components/Badge";
import { PageHeader } from "../components/PageHeader";
import { QueryState } from "../components/QueryState";
import { SearchInput, FilterSelect, Toolbar } from "../components/Toolbar";
import { emptyArt, errorArt } from "../lib/assets";
import { ExposureAnalysis } from "./ports/ExposureAnalysis";
import { ProcessOwnership } from "./ports/ProcessOwnership";
import { SocketInspector } from "./ports/SocketInspector";
import { TLSPosture } from "./ports/TLSPosture";
import {
  EXPOSURE_LABEL, TLS_LABEL, exposureOf, readPosture, tlsStateOf, wellKnownName,
  type Exposure, type TLSState,
} from "./ports/portModel";
import { DEFAULT_FILTERS, socketKey, usePortTable, type SortKey } from "./ports/usePortTable";

const EXPOSURE_TONE: Record<Exposure, Tone> = {
  world: "warning",
  network: "info",
  loopback: "success",
};

const TLS_TONE: Record<TLSState, Tone> = {
  valid: "success",
  expiring: "warning",
  expired: "danger",
  "self-signed": "info",
  plaintext: "neutral",
  unprobed: "neutral",
};

const PROTOCOL_OPTIONS = [
  { value: "all", label: "All protocols" },
  { value: "tcp", label: "TCP" },
  { value: "udp", label: "UDP" },
] as const;

const EXPOSURE_OPTIONS = [
  { value: "all", label: "All exposure" },
  { value: "world", label: "All interfaces" },
  { value: "network", label: "Specific interface" },
  { value: "loopback", label: "Loopback only" },
] as const;

const TLS_OPTIONS = [
  { value: "all", label: "All transport" },
  { value: "valid", label: "Valid certificate" },
  { value: "expiring", label: "Expiring soon" },
  { value: "expired", label: "Expired" },
  { value: "self-signed", label: "Self-signed" },
  { value: "plaintext", label: "No TLS" },
  { value: "unprobed", label: "Not probed" },
] as const;

/**
 * Network exposure.
 *
 * The only page in Atlas that answers a security question rather than a
 * resource one, and it is structured around the fact that decides a listening
 * socket's risk: what can reach it. Exposure leads, transport security follows,
 * then who owns what, then the socket-by-socket explorer.
 *
 * Two claims this page is careful never to make.
 *
 * It does not say a socket is "reachable from the internet". Bindings are
 * observable; firewalls and routing are not, and the honest claim is the one
 * about the binding.
 *
 * It does not treat a missing certificate as plaintext. TLS probing is budgeted
 * and TCP-only, so an absent certificate is either a fact or a gap, and the API
 * reports `tls_probed` precisely so the two can be told apart. Reporting
 * absence of evidence as evidence of absence is the classic way a security
 * dashboard misleads.
 */
export function PortsPage() {
  const nodeID = usePrimaryNodeID();
  const ports = usePorts(nodeID);
  const [selectedKey, setSelectedKey] = useState<string | null>(null);

  const all = ports.data?.ports ?? emptyArray<Port>();
  const posture = useMemo(() => readPosture(all), [all]);
  const { filters, setFilters, processes, sorted, sortKey, direction, toggleSort } =
    usePortTable(all);

  const selected = useMemo(
    () => all.find((p) => socketKey(p) === selectedKey) ?? null,
    [all, selectedKey],
  );

  // Every socket held by the selected socket's process, for the inspector's
  // cross-reference.
  const siblings = useMemo(
    () => (selected?.pid ? all.filter((p) => p.pid === selected.pid) : selected ? [selected] : []),
    [all, selected],
  );

  const attention = posture.expired.length + posture.expiring.length + posture.exposedPlaintext.length;

  if (inventoryGapKind(ports.error) === "agent") {
    return <AgentSubjectGap subject="listening ports" />;
  }

  if (ports.error instanceof ApiError && ports.error.code === "not_implemented") {
    return (
      <>
        <PageHeader subtitle="Listening ports are not available on this host." />
        <Card>
          <EmptyState
            kind="unavailable"
            art={errorArt.forbidden}
            title="Listening ports are not available"
            description="Atlas could not read the connection table on this host."
            hint="Enumerating sockets and their owning processes needs elevated privilege on most platforms. Without it the table is unreadable rather than empty."
          />
        </Card>
      </>
    );
  }

  if (ports.isPending || ports.error || all.length === 0) {
    return (
      <>
        <PageHeader
          stats={["Listening", "All interfaces", "Certificates", "Processes"].map((label) => ({
            label,
            value: "—",
            hint: ports.error ? "unavailable" : ports.isPending ? "loading" : "none",
          }))}
        />
        <Card level="floating" className="p-0">
          <QueryState
            isPending={ports.isPending}
            error={ports.error}
            isEmpty={all.length === 0}
            onRetry={() => void ports.refetch()}
            rows={5}
            empty={{
              art: emptyArt.data,
              title: "Nothing is listening on this host",
              description:
                "No process holds a listening TCP or UDP socket. For a host with no network role this is exactly right, and it is the smallest attack surface a machine can have.",
              hint: "Only listening sockets are read. Outbound connections are never enumerated.",
            }}
          />
        </Card>
      </>
    );
  }

  return (
    <>
      <PageHeader
        stats={[
          {
            label: "Listening",
            value: String(posture.total),
            hint: `${posture.tcp} TCP · ${posture.udp} UDP`,
          },
          {
            label: "All interfaces",
            value: String(posture.byExposure.world.length),
            hint:
              posture.byExposure.world.length > 0
                ? "nothing in the binding limits who connects"
                : "none bound wide",
            tone: posture.byExposure.world.length > 0 ? "warning" : "success",
          },
          {
            label: "Certificates",
            value: String(posture.certificates.length),
            hint: `${posture.probed} of ${posture.probeable} TCP probed`,
            tone: posture.expired.length > 0 ? "danger" : "default",
          },
          {
            label: "Needs review",
            value: String(attention),
            hint:
              attention > 0
                ? "expiring, expired, or exposed plaintext"
                : "no certificate or exposure findings",
            tone: posture.expired.length > 0 ? "danger" : attention > 0 ? "warning" : "success",
          },
        ]}
      />

      <h2 className="eyebrow mb-3">Exposure</h2>
      <ExposureAnalysis
        posture={posture}
        selected={filters.exposure === "all" ? null : filters.exposure}
        onSelect={(e) => { setFilters((f) => ({ ...f, exposure: e ?? "all" })); }}
      />

      <h2 className="eyebrow mb-3">Transport security</h2>
      <TLSPosture posture={posture} />

      <h2 className="eyebrow mb-3">Ownership</h2>
      <ProcessOwnership
        ports={all}
        selected={filters.process === "all" ? null : filters.process}
        onSelect={(p) => { setFilters((f) => ({ ...f, process: p ?? "all" })); }}
      />

      <h2 className="eyebrow mb-3">Socket explorer</h2>

      <div className={`grid grid-cols-1 gap-4 ${selected ? "xl:grid-cols-[minmax(0,1fr)_22rem]" : ""}`}>
        <Card level="floating" className="min-w-0 p-0">
          <div className="border-b border-border p-4">
            <Toolbar>
              <SearchInput
                value={filters.search}
                onChange={(v) => { setFilters((f) => ({ ...f, search: v })); }}
                placeholder="Search port, address, process, pid, or certificate…"
              />
              <FilterSelect
                value={filters.protocol}
                onChange={(v) => { setFilters((f) => ({ ...f, protocol: v })); }}
                options={[...PROTOCOL_OPTIONS]}
              />
              <FilterSelect
                value={filters.exposure}
                onChange={(v) => { setFilters((f) => ({ ...f, exposure: v })); }}
                options={[...EXPOSURE_OPTIONS]}
              />
              <FilterSelect
                value={filters.tls}
                onChange={(v) => { setFilters((f) => ({ ...f, tls: v })); }}
                options={[...TLS_OPTIONS]}
              />
              <FilterSelect
                value={filters.process}
                onChange={(v) => { setFilters((f) => ({ ...f, process: v })); }}
                options={[
                  { value: "all", label: "All processes" },
                  ...processes.map((p) => ({ value: p, label: p })),
                ]}
              />
            </Toolbar>
            <p className="mt-2 text-xs text-text-muted">
              {sorted.length} of {all.length} socket{all.length === 1 ? "" : "s"} · click a row to
              inspect
            </p>
          </div>

          <QueryState
            isPending={false}
            isEmpty={sorted.length === 0}
            rows={4}
            empty={{
              kind: "filtered",
              art: emptyArt.search,
              title: "No sockets match these filters",
              description: `${all.length.toLocaleString()} sockets are listening; the current combination of search, protocol, exposure, transport and process excludes every one.`,
              action: (
                <EmptyAction onClick={() => { setFilters(DEFAULT_FILTERS); }}>
                  Clear filters
                </EmptyAction>
              ),
            }}
          />

          {sorted.length > 0 ? (
            <div className="scroll-thin max-h-[34rem] overflow-auto">
              <table aria-label="Socket explorer" className="w-full border-collapse text-sm">
                <thead className="sticky top-0 z-10 bg-surface-hover">
                  <tr>
                    <SortableTh label="Port" col="port" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Protocol" col="protocol" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Address" col="address" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Exposure" col="exposure" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Transport" col="tls" {...{ sortKey, direction, toggleSort }} />
                    <SortableTh label="Process" col="process" {...{ sortKey, direction, toggleSort }} />
                  </tr>
                </thead>
                <tbody>
                  {sorted.map((p) => (
                    <SocketRow
                      key={socketKey(p)}
                      socket={p}
                      selected={socketKey(p) === selectedKey}
                      onSelect={() => {
                        setSelectedKey(socketKey(p) === selectedKey ? null : socketKey(p));
                      }}
                    />
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </Card>

        {selected ? (
          <SocketInspector
            socket={selected}
            siblings={siblings}
            onClose={() => { setSelectedKey(null); }}
          />
        ) : null}
      </div>
    </>
  );
}

function SortableTh({
  label,
  col,
  sortKey,
  direction,
  toggleSort,
}: {
  label: string;
  col: SortKey;
  sortKey: SortKey;
  direction: "asc" | "desc";
  toggleSort: (k: SortKey) => void;
}) {
  const active = sortKey === col;
  const Icon = !active ? ChevronsUpDown : direction === "asc" ? ArrowUp : ArrowDown;

  return (
    <th
      aria-sort={active ? (direction === "asc" ? "ascending" : "descending") : "none"}
      className="px-3 py-2.5 text-[11px] font-semibold tracking-wider text-text-muted uppercase"
    >
      <button
        type="button"
        onClick={() => { toggleSort(col); }}
        className="flex w-full items-center gap-1 hover:text-text"
      >
        <span>{label}</span>
        <Icon size={11} className={active ? "text-primary" : "opacity-50"} />
      </button>
    </th>
  );
}

function SocketRow({
  socket: s,
  selected,
  onSelect,
}: {
  socket: Port;
  selected: boolean;
  onSelect: () => void;
}) {
  const exposure = exposureOf(s);
  const tls = tlsStateOf(s);
  const known = wellKnownName(s.port);

  return (
    <tr
      onClick={onSelect}
      className={`cursor-pointer border-b border-border/60 transition-colors last:border-0 ${
        selected ? "bg-primary/10" : "hover:bg-surface-hover"
      }`}
    >
      <td className="px-3 py-2">
        <span className="font-mono text-sm font-medium text-text">{s.port}</span>
        {known ? <span className="ml-2 text-[11px] text-text-subtle">{known}</span> : null}
      </td>

      <td className="px-3 py-2 text-xs text-text-muted uppercase">{s.protocol}</td>

      <td className="max-w-[12rem] px-3 py-2">
        <span className="block truncate font-mono text-xs text-text-muted" title={s.address}>
          {s.address}
        </span>
      </td>

      <td className="px-3 py-2">
        <Badge tone={EXPOSURE_TONE[exposure]}>{EXPOSURE_LABEL[exposure]}</Badge>
      </td>

      <td className="px-3 py-2">
        {/* "Not probed" is rendered as a plain absence, not a badge: it is the
            lack of an observation, and giving it a badge would make it look
            like a verdict. */}
        {tls === "unprobed" ? (
          <span className="text-xs text-text-subtle" title="Atlas did not attempt a handshake here">
            not probed
          </span>
        ) : tls === "plaintext" ? (
          <span className="text-xs text-text-muted">no TLS</span>
        ) : (
          <Badge tone={TLS_TONE[tls]} pulse={tls === "expired"}>
            {TLS_LABEL[tls]}
          </Badge>
        )}
      </td>

      <td className="max-w-[14rem] px-3 py-2">
        {s.process ? (
          <>
            <span className="block truncate text-xs text-text" title={s.process}>
              {s.process}
            </span>
            {s.pid ? (
              <span className="block font-mono text-[11px] text-text-subtle">pid {s.pid}</span>
            ) : null}
          </>
        ) : (
          <span className="text-xs text-text-subtle">not resolved</span>
        )}
      </td>
    </tr>
  );
}
