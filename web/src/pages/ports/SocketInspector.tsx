import { useMemo, useState } from "react";
import { X } from "lucide-react";
import { Link } from "react-router";
import { usePrimaryNodeID, useProcesses } from "../../api/queries";
import type { Port } from "../../api/types";
import { Badge, type Tone } from "../../components/Badge";
import { CopyButton } from "../processes/ProcessInspector";
import { ListSkeleton } from "../../components/Skeleton";
import { formatBytes, formatDuration } from "../../format";
import {
  CERT_WARN_DAYS, EXPOSURE_DETAIL, EXPOSURE_LABEL, TLS_LABEL,
  certProgress, exposureOf, tlsStateOf, wellKnownName, type TLSState,
} from "./portModel";
import { socketKey } from "./usePortTable";

const TLS_TONE: Record<TLSState, Tone> = {
  valid: "success",
  expiring: "warning",
  expired: "danger",
  "self-signed": "info",
  plaintext: "neutral",
  unprobed: "neutral",
};

type Tab = "binding" | "process" | "certificate";

/**
 * The right-hand inspector for one listening socket.
 *
 * The process tab cross-references `/api/v1/processes` by pid — a real join
 * rather than an inference from names — and lists the other sockets the same
 * process holds, which is usually how "what is this thing" gets answered.
 *
 * Both endpoints describe the host Atlas runs on, so the join is sound today.
 * It will need revisiting when agents make these node-scoped.
 */
export function SocketInspector({
  socket: s,
  siblings,
  onClose,
}: {
  socket: Port;
  /** Every socket held by the same process, including this one. */
  siblings: Port[];
  onClose: () => void;
}) {
  const [tab, setTab] = useState<Tab>("binding");
  const exposure = exposureOf(s);
  const tls = tlsStateOf(s);

  return (
    <aside
      aria-label={`Socket details: port ${String(s.port)}`}
      className="elev-3 sticky top-0 flex max-h-[calc(100vh-7rem)] flex-col overflow-hidden rounded-xl"
    >
      <header className="flex items-start justify-between gap-3 border-b border-border p-4">
        <div className="min-w-0">
          <h2 className="flex items-baseline gap-2 text-base font-semibold text-text">
            <span className="font-mono">{s.port}</span>
            <span className="eyebrow">{s.protocol}</span>
            {wellKnownName(s.port) ? (
              <span className="truncate text-xs font-normal text-text-muted">
                {wellKnownName(s.port)}
              </span>
            ) : null}
          </h2>
          <p className="mt-0.5 truncate font-mono text-xs text-text-muted">{s.address}</p>
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

      <nav className="flex gap-1 border-b border-border px-2 py-1.5" role="tablist">
        {(
          [
            ["binding", "Binding"],
            ["process", "Process"],
            ["certificate", "Certificate"],
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
        {tab === "binding" ? (
          <div className="flex flex-col gap-4">
            <dl className="flex flex-col">
              <Field label="Port" value={<span className="font-mono">{s.port}</span>} />
              <Field label="Protocol" value={s.protocol.toUpperCase()} />
              <Field label="Bind address" value={<span className="font-mono">{s.address}</span>} />
              <Field label="Exposure" value={EXPOSURE_LABEL[exposure]} />
              <Field
                label="Transport"
                value={<Badge tone={TLS_TONE[tls]}>{TLS_LABEL[tls]}</Badge>}
              />
            </dl>

            <p className="text-[11px] leading-relaxed text-text-muted">
              {EXPOSURE_DETAIL[exposure]}
            </p>

            {tls === "unprobed" ? (
              // The single most important sentence on this panel: absence of a
              // certificate here is absence of evidence, not evidence of
              // plaintext.
              <p className="surface-warn rounded-lg p-2.5 text-[11px] leading-relaxed">
                Atlas did not attempt a TLS handshake on this socket. Probing is bounded per cycle
                and covers TCP only, so this is unknown rather than plaintext.
              </p>
            ) : null}

            {wellKnownName(s.port) ? (
              <p className="text-[11px] leading-relaxed text-text-subtle">
                Port {s.port} conventionally carries {wellKnownName(s.port)}. That is what the number
                usually means, not what Atlas observed — the owning process is the real answer.
              </p>
            ) : null}

            <div>
              <div className="mb-1.5 flex items-center justify-between">
                <h3 className="eyebrow">Address</h3>
                <CopyButton value={`${s.address}:${String(s.port)}`} />
              </div>
              <code className="elev-1 block rounded-lg p-2.5 font-mono text-[11px] break-all text-text">
                {s.address}:{s.port}/{s.protocol}
              </code>
            </div>
          </div>
        ) : null}

        {tab === "process" ? <ProcessTab socket={s} siblings={siblings} /> : null}
        {tab === "certificate" ? <CertificateTab socket={s} /> : null}
      </div>
    </aside>
  );
}

function ProcessTab({ socket: s, siblings }: { socket: Port; siblings: Port[] }) {
  const processes = useProcesses(usePrimaryNodeID());

  // A real join on pid, not a name match: two processes can share a name, and
  // the pid is what the socket table actually recorded.
  const proc = useMemo(
    () => (s.pid ? processes.data?.processes.find((p) => p.pid === s.pid) : undefined),
    [processes.data, s.pid],
  );

  if (!s.pid) {
    return (
      <p className="py-6 text-center text-xs leading-relaxed text-text-muted">
        Atlas could not resolve this socket's owner. Reading the process behind a socket needs
        privilege it does not have here — the socket is real and listening; only its owner is
        unknown.
      </p>
    );
  }

  const others = siblings.filter((o) => socketKey(o) !== socketKey(s));

  return (
    <div className="flex flex-col gap-4">
      <dl className="flex flex-col">
        <Field label="Process" value={s.process ?? "unknown"} />
        <Field label="PID" value={<span className="font-mono">{s.pid}</span>} />
        {proc ? (
          <>
            <Field label="User" value={proc.username ?? "unavailable"} />
            <Field label="State" value={proc.state} />
            <Field label="CPU" value={`${proc.cpu_percent.toFixed(1)}%`} />
            <Field label="Memory" value={formatBytes(proc.memory_rss)} />
            <Field
              label="Running for"
              value={proc.running_for_seconds ? formatDuration(proc.running_for_seconds) : "unknown"}
            />
          </>
        ) : null}
      </dl>

      {/* The join is against a separate, larger request. While it is in flight
          the panel said nothing at all, which read as "there is no more to
          know" rather than "still loading". */}
      {processes.isPending ? (
        <ListSkeleton rows={2} />
      ) : processes.error ? (
        <p className="text-[11px] leading-relaxed text-text-subtle">
          The process list could not be read, so this socket's owner cannot be described beyond its
          name and pid.
        </p>
      ) : !proc ? (
        <p className="text-[11px] leading-relaxed text-text-subtle">
          No process with pid {s.pid} is in the current process list. It most likely exited between
          the two reads — the socket list and the process list are separate snapshots.
        </p>
      ) : null}

      {proc?.cmdline ? (
        <div>
          <div className="mb-1.5 flex items-center justify-between">
            <h3 className="eyebrow">Command</h3>
            <CopyButton value={proc.cmdline} />
          </div>
          <code className="elev-1 block rounded-lg p-2.5 font-mono text-[11px] leading-relaxed break-all text-text">
            {proc.cmdline}
          </code>
        </div>
      ) : null}

      <section>
        <h3 className="eyebrow mb-2">
          Other sockets held by this process
          {others.length > 0 ? ` (${String(others.length)})` : ""}
        </h3>
        {others.length === 0 ? (
          <p className="text-xs text-text-muted">This is the only socket it holds.</p>
        ) : (
          <ul className="flex flex-col gap-1.5">
            {others.map((o) => (
              <li key={socketKey(o)} className="flex items-center gap-2 text-xs">
                <span className="font-mono font-semibold text-text">{o.port}</span>
                <span className="eyebrow">{o.protocol}</span>
                <span className="ml-auto truncate font-mono text-[11px] text-text-muted">
                  {o.address}
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>

      <Link
        to="/processes"
        className="text-xs font-medium text-primary hover:underline"
      >
        Open in the process explorer →
      </Link>
    </div>
  );
}

function CertificateTab({ socket: s }: { socket: Port }) {
  const cert = s.tls;
  const state = tlsStateOf(s);

  if (!cert) {
    return (
      <p className="py-6 text-center text-xs leading-relaxed text-text-muted">
        {state === "unprobed"
          ? "This socket was not probed for TLS. Probing is bounded per cycle and covers TCP only, so no conclusion can be drawn about its transport security."
          : "This socket answered a handshake attempt without presenting a certificate. It is serving plaintext, which is expected for a non-TLS protocol."}
      </p>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <dl className="flex flex-col">
        <Field label="Status" value={<Badge tone={TLS_TONE[state]}>{TLS_LABEL[state]}</Badge>} />
        <Field label="Subject" value={cert.subject ?? "—"} />
        <Field label="Issuer" value={cert.issuer ?? "—"} />
        <Field
          label="Valid from"
          value={cert.not_before ? new Date(cert.not_before).toLocaleDateString() : "—"}
        />
        <Field
          label="Expires"
          value={cert.not_after ? new Date(cert.not_after).toLocaleString() : "—"}
        />
        <Field
          label="Remaining"
          value={
            cert.expired
              ? "expired"
              : `${String(cert.days_until_expiry)} day${cert.days_until_expiry === 1 ? "" : "s"}`
          }
        />
        <Field label="Self-signed" value={cert.self_signed ? "yes" : "no"} />
      </dl>

      {certProgress(cert) !== undefined ? (
        <div>
          <h3 className="eyebrow mb-1.5">Lifetime elapsed</h3>
          <div className="h-1.5 overflow-hidden rounded-full bg-surface-hover">
            <div
              className={`h-full rounded-full ${
                cert.expired
                  ? "bg-danger"
                  : cert.days_until_expiry <= CERT_WARN_DAYS
                    ? "bg-warning"
                    : "bg-success"
              }`}
              style={{ width: `${String((certProgress(cert) ?? 0) * 100)}%` }}
            />
          </div>
        </div>
      ) : null}

      {cert.sans && cert.sans.length > 0 ? (
        <section>
          <h3 className="eyebrow mb-1.5">Also valid for</h3>
          <ul className="flex flex-wrap gap-1">
            {cert.sans.map((san) => (
              <li key={san} className="elev-1 rounded px-1.5 py-0.5 font-mono text-[11px] text-text-muted">
                {san}
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {/* Atlas reports the presented chain; it does not validate it. Saying so
          prevents "valid certificate" being read as "trusted by clients". */}
      <p className="border-t border-border pt-3 text-[11px] leading-relaxed text-text-subtle">
        Atlas records the certificate a service presents. It does not verify the chain against a
        trust store, so “valid” here means unexpired and well-formed, not trusted by any particular
        client.
      </p>
    </div>
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
