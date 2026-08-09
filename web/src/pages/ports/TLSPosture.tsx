import { ShieldCheck } from "lucide-react";
import type { Port } from "../../api/types";
import { CERT_WARN_DAYS, certProgress, exposureOf, type Posture } from "./portModel";
import { socketKey } from "./usePortTable";
import { Card, CardHeader } from "../../components/Card";
import { Badge, type Tone } from "../../components/Badge";
import { EmptyState } from "../../components/EmptyState";
import { emptyArt } from "../../lib/assets";

/**
 * Certificates found on this host, and how far Atlas actually looked.
 *
 * The coverage line is the important part and is stated first. TLS probing is
 * budgeted and TCP-only, so "one certificate" on its own invites the reader to
 * conclude that everything else is plaintext. It usually is not — some sockets
 * were never probed — and the panel says which is which rather than letting
 * the absence speak.
 *
 * The timeline shows each certificate's position in its own validity window
 * rather than on a shared calendar axis. Certificates have wildly different
 * lifetimes — 90 days for an ACME cert, two years for an internal CA — and a
 * shared axis makes the short-lived one look permanently near expiry.
 */
export function TLSPosture({ posture }: { posture: Posture }) {
  const coverage =
    posture.probeable > 0 ? Math.round((posture.probed / posture.probeable) * 100) : 0;

  return (
    <Card level="flat" className="mb-6">
      <CardHeader
        title="TLS posture"
        action={
          <span className="text-xs text-text-muted">
            {posture.certificates.length} certificate{posture.certificates.length === 1 ? "" : "s"}{" "}
            found
          </span>
        }
      />

      {/* Coverage before findings: a reader who does not know how much was
          looked at cannot interpret how little was found. */}
      <div className="mb-4 rounded-lg border border-border p-3">
        <div className="mb-1.5 flex items-baseline justify-between gap-2">
          <span className="eyebrow">Probe coverage</span>
          <span className="text-xs tabular-nums text-text-muted">
            {posture.probed} of {posture.probeable} TCP sockets · {coverage}%
          </span>
        </div>
        <div className="h-1.5 overflow-hidden rounded-full bg-surface-hover">
          <div className="h-full rounded-full bg-primary" style={{ width: `${String(coverage)}%` }} />
        </div>
        <p className="mt-2 text-[11px] leading-relaxed text-text-subtle">
          Handshakes are attempted on a bounded number of TCP sockets each cycle, so a socket with no
          certificate here was either genuinely plaintext or never reached. The explorer distinguishes
          them. UDP is never probed and is excluded from this denominator.
        </p>
      </div>

      {posture.certificates.length === 0 ? (
        <EmptyState
          art={emptyArt.data}
          title="No certificates found"
          description="No probed socket presented a certificate. On a host running only plaintext or non-TLS services this is the expected result."
          hint={`${String(posture.probeable - posture.probed)} TCP socket${posture.probeable - posture.probed === 1 ? " was" : "s were"} not probed this cycle, so this is not a complete survey.`}
          compact
        />
      ) : (
        <ul className="flex flex-col gap-2.5">
          {[...posture.certificates]
            // Soonest to expire first: the timeline exists to surface deadlines.
            .sort((a, b) => (a.tls?.days_until_expiry ?? 0) - (b.tls?.days_until_expiry ?? 0))
            .map((p) => (
              <CertificateRow key={socketKey(p)} port={p} />
            ))}
        </ul>
      )}

      {posture.expired.length > 0 || posture.expiring.length > 0 || posture.selfSigned.length > 0 ? (
        <div className="mt-4 flex flex-wrap gap-2 border-t border-border pt-3 text-xs">
          {posture.expired.length > 0 ? (
            <Badge tone="danger">{posture.expired.length} expired</Badge>
          ) : null}
          {posture.expiring.length > 0 ? (
            <Badge tone="warning">
              {posture.expiring.length} expiring within {CERT_WARN_DAYS} days
            </Badge>
          ) : null}
          {posture.selfSigned.length > 0 ? (
            <Badge tone="info">{posture.selfSigned.length} self-signed</Badge>
          ) : null}
        </div>
      ) : null}
    </Card>
  );
}

function CertificateRow({ port: p }: { port: Port }) {
  const cert = p.tls;
  if (!cert) return null;

  const days = cert.days_until_expiry;
  const tone: Tone = cert.expired
    ? "danger"
    : days <= CERT_WARN_DAYS
      ? "warning"
      : cert.self_signed
        ? "info"
        : "success";

  const progress = certProgress(cert);
  const bar = cert.expired ? "bg-danger" : days <= CERT_WARN_DAYS ? "bg-warning" : "bg-success";

  return (
    <li className="elev-1 rounded-lg p-3">
      <div className="mb-1.5 flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
        <span className="flex min-w-0 items-center gap-2">
          <ShieldCheck size={13} className="shrink-0 text-text-muted" aria-hidden="true" />
          <span className="truncate text-sm font-medium text-text" title={cert.subject}>
            {cert.subject ?? `port ${String(p.port)}`}
          </span>
          <span className="shrink-0 font-mono text-[11px] text-text-subtle">
            :{p.port}
          </span>
        </span>
        <Badge tone={tone}>
          {cert.expired
            ? "expired"
            : `${String(days)} day${days === 1 ? "" : "s"} left`}
        </Badge>
      </div>

      {/* Position within this certificate's own lifetime, not a shared axis —
          a 90-day cert and a 2-year cert are not comparable on one scale. */}
      {progress !== undefined ? (
        <div className="mb-1.5 h-1 overflow-hidden rounded-full bg-surface-hover">
          <div className={`h-full rounded-full ${bar}`} style={{ width: `${String(progress * 100)}%` }} />
        </div>
      ) : null}

      <dl className="flex flex-wrap gap-x-4 gap-y-0.5 text-[11px] text-text-muted">
        {cert.issuer ? (
          <span className="truncate">
            <dt className="inline text-text-subtle">issued by </dt>
            <dd className="inline">{cert.issuer}</dd>
          </span>
        ) : null}
        {cert.not_after ? (
          <span>
            <dt className="inline text-text-subtle">expires </dt>
            <dd className="inline tabular-nums">{new Date(cert.not_after).toLocaleDateString()}</dd>
          </span>
        ) : null}
        <span>
          <dt className="inline text-text-subtle">on </dt>
          <dd className="inline">
            {p.process ?? "unattributed"} · {exposureOf(p) === "loopback" ? "loopback" : p.address}
          </dd>
        </span>
      </dl>

      {cert.self_signed ? (
        <p className="mt-1.5 text-[11px] leading-relaxed text-text-subtle">
          Self-signed. Fine for an internal endpoint whose clients trust it explicitly; a problem for
          anything a browser or third party is expected to verify.
        </p>
      ) : null}

      {cert.sans && cert.sans.length > 0 ? (
        <p className="mt-1 truncate text-[11px] text-text-subtle" title={cert.sans.join(", ")}>
          also valid for {cert.sans.join(", ")}
        </p>
      ) : null}
    </li>
  );
}
