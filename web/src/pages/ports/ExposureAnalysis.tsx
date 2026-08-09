import { Globe, Lock, Network } from "lucide-react";
import type { Port } from "../../api/types";
import {
  EXPOSURE_DETAIL, EXPOSURE_LABEL, EXPOSURE_ORDER, tlsStateOf, wellKnownName,
  type Exposure, type Posture,
} from "./portModel";
import { Card, CardHeader } from "../../components/Card";

/**
 * Exposure: who can reach each listening socket.
 *
 * This is the page's spine. A socket's risk is decided almost entirely by its
 * binding — the same database on loopback and on 0.0.0.0 is the same service
 * and a completely different exposure — and no other field carries that.
 *
 * The wording is careful about what Atlas can actually claim. "All interfaces"
 * is a fact about the binding; "reachable from the internet" would be a claim
 * about firewalls and routing Atlas cannot see, and is never made here.
 */

const ICON: Record<Exposure, typeof Globe> = {
  world: Globe,
  network: Network,
  loopback: Lock,
};

const TONE: Record<Exposure, { text: string; bar: string; ring: string }> = {
  world: { text: "text-warning", bar: "bg-warning", ring: "bg-warning/12" },
  network: { text: "text-info", bar: "bg-info", ring: "bg-info/12" },
  loopback: { text: "text-success", bar: "bg-success", ring: "bg-success/12" },
};

export function ExposureAnalysis({
  posture,
  selected,
  onSelect,
}: {
  posture: Posture;
  selected: Exposure | null;
  onSelect: (e: Exposure | null) => void;
}) {
  const present = EXPOSURE_ORDER.filter((e) => posture.byExposure[e].length > 0);

  return (
    <Card level="flat" className="mb-6">
      <CardHeader
        title="Exposure"
        action={
          <span className="text-xs text-text-muted">
            {posture.total} socket{posture.total === 1 ? "" : "s"} · click to filter the explorer
          </span>
        }
      />

      {/* One proportional bar: the shape of the host's exposure reads before
          any number does. */}
      <div className="mb-4 flex h-2 overflow-hidden rounded-full bg-surface-hover">
        {present.map((e) => (
          <span
            key={e}
            className={TONE[e].bar}
            style={{ width: `${String((posture.byExposure[e].length / posture.total) * 100)}%` }}
            title={`${String(posture.byExposure[e].length)} ${EXPOSURE_LABEL[e]}`}
          />
        ))}
      </div>

      <div className="grid grid-cols-1 gap-3 lg:grid-cols-3">
        {EXPOSURE_ORDER.map((e) => (
          <ExposureCard
            key={e}
            exposure={e}
            ports={posture.byExposure[e]}
            total={posture.total}
            selected={selected === e}
            onSelect={() => { onSelect(selected === e ? null : e); }}
          />
        ))}
      </div>
    </Card>
  );
}

function ExposureCard({
  exposure,
  ports,
  total,
  selected,
  onSelect,
}: {
  exposure: Exposure;
  ports: Port[];
  total: number;
  selected: boolean;
  onSelect: () => void;
}) {
  const Icon = ICON[exposure];
  const tone = TONE[exposure];
  const plaintext = ports.filter((p) => tlsStateOf(p) === "plaintext").length;

  // Ports are the useful evidence here, so a few are named. Sorted by number
  // rather than by name: an operator scanning for "is 5432 open" reads numbers.
  const sample = [...ports].sort((a, b) => a.port - b.port).slice(0, 8);

  return (
    <button
      type="button"
      onClick={onSelect}
      aria-pressed={selected}
      disabled={ports.length === 0}
      className={`rounded-lg border p-4 text-left transition-colors ${
        ports.length === 0
          ? "border-dashed border-border opacity-60"
          : selected
            ? "border-primary bg-primary/5"
            : "border-border hover:bg-surface-hover"
      }`}
    >
      <div className="mb-2 flex items-center gap-2">
        <span className={`flex h-7 w-7 shrink-0 items-center justify-center rounded-lg ${tone.ring}`}>
          <Icon size={14} className={tone.text} aria-hidden="true" />
        </span>
        <span className="text-sm font-medium text-text">{EXPOSURE_LABEL[exposure]}</span>
      </div>

      <div className="mb-2 flex items-baseline gap-1.5">
        <span className={`text-2xl font-semibold tabular-nums ${ports.length > 0 ? tone.text : "text-text-subtle"}`}>
          {ports.length}
        </span>
        <span className="text-xs text-text-muted">
          socket{ports.length === 1 ? "" : "s"}
        </span>
        {ports.length > 0 ? (
          <span className="ml-auto text-xs tabular-nums text-text-subtle">
            {((ports.length / total) * 100).toFixed(0)}%
          </span>
        ) : null}
      </div>

      <p className="mb-2.5 text-[11px] leading-relaxed text-text-muted">
        {EXPOSURE_DETAIL[exposure]}
      </p>

      {sample.length > 0 ? (
        <ul className="flex flex-wrap gap-1">
          {sample.map((p) => (
            <li
              key={`${p.protocol}:${p.address}:${String(p.port)}`}
              title={`${p.process ?? "unattributed"} · ${p.address}:${String(p.port)}/${p.protocol}`}
              className="elev-1 rounded px-1.5 py-0.5 font-mono text-[11px] text-text-muted"
            >
              {p.port}
              {wellKnownName(p.port) ? (
                <span className="ml-1 font-sans text-text-subtle">{wellKnownName(p.port)}</span>
              ) : null}
            </li>
          ))}
          {ports.length > sample.length ? (
            <li className="px-1.5 py-0.5 text-[11px] text-text-subtle">
              +{ports.length - sample.length} more
            </li>
          ) : null}
        </ul>
      ) : null}

      {/* Plaintext is only called out where something can reach it. On loopback
          it is the norm, and flagging it would bury the one that matters. */}
      {exposure === "world" && plaintext > 0 ? (
        <p className="mt-2.5 border-t border-border pt-2 text-[11px] leading-relaxed text-warning">
          {plaintext} of these answered a probe without TLS. On a binding this open, that is worth a
          look — though a non-HTTP protocol legitimately has no certificate to present.
        </p>
      ) : null}
    </button>
  );
}
