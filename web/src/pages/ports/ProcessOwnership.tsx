import { Globe, Lock, Network } from "lucide-react";
import type { Port } from "../../api/types";
import { EXPOSURE_LABEL, groupByProcess, wellKnownName, type Exposure } from "./portModel";
import { socketKey } from "./usePortTable";
import { Card, CardHeader } from "../../components/Card";

/**
 * Which process holds which sockets.
 *
 * The inverse of the explorer's view and the one that answers "what is this
 * machine actually serving". A process holding eight loopback sockets is a
 * development tool; a process holding one socket on 0.0.0.0 is the host's
 * attack surface, and sorting by exposure rather than by socket count puts the
 * second one first.
 *
 * Sockets whose owner Atlas could not resolve are grouped and explained rather
 * than dropped: on most platforms that is a permission boundary, and silently
 * omitting them would understate what is listening.
 */

const ICON: Record<Exposure, typeof Globe> = { world: Globe, network: Network, loopback: Lock };
const TONE: Record<Exposure, string> = {
  world: "text-warning",
  network: "text-info",
  loopback: "text-success",
};

export function ProcessOwnership({
  ports,
  selected,
  onSelect,
}: {
  ports: Port[];
  selected: string | null;
  onSelect: (process: string | null) => void;
}) {
  const groups = groupByProcess(ports);
  const unattributed = ports.filter((p) => !p.pid).length;

  return (
    <Card level="flat" className="mb-6">
      <CardHeader
        title="Process ownership"
        action={
          <span className="text-xs text-text-muted">
            {groups.length} program{groups.length === 1 ? "" : "s"} holding {ports.length} socket
            {ports.length === 1 ? "" : "s"}
          </span>
        }
      />

      <ul className="flex flex-col">
        {groups.map((g) => {
          const Icon = ICON[g.worst];
          const isSelected = selected === g.process;

          return (
            <li key={g.key}>
              <button
                type="button"
                onClick={() => { onSelect(isSelected ? null : g.process); }}
                aria-pressed={isSelected}
                className={`flex w-full items-center gap-3 rounded-lg px-2 py-2 text-left transition-colors ${
                  isSelected ? "bg-primary/10" : "hover:bg-surface-hover"
                }`}
              >
                <Icon
                  size={13}
                  className={`shrink-0 ${TONE[g.worst]}`}
                  aria-label={EXPOSURE_LABEL[g.worst]}
                />

                <span className="min-w-0 flex-1">
                  <span className="block truncate text-sm text-text" title={g.process}>
                    {g.process}
                  </span>
                  <span className="block truncate font-mono text-[11px] text-text-subtle">
                    {g.pids.length === 0
                      ? "owner not resolved"
                      : g.pids.length === 1
                        ? `pid ${String(g.pids[0])}`
                        : `${String(g.pids.length)} processes · pid ${g.pids.slice(0, 3).join(", ")}${g.pids.length > 3 ? "…" : ""}`}
                  </span>
                </span>

                <span className="hidden max-w-[16rem] flex-wrap justify-end gap-1 sm:flex">
                  {g.ports.slice(0, 5).map((p) => (
                    <span
                      key={socketKey(p)}
                      title={`${p.address}:${String(p.port)}/${p.protocol}`}
                      className="elev-1 rounded px-1.5 py-0.5 font-mono text-[11px] text-text-muted"
                    >
                      {p.port}
                      {wellKnownName(p.port) ? (
                        <span className="ml-1 font-sans text-text-subtle">
                          {wellKnownName(p.port)}
                        </span>
                      ) : null}
                    </span>
                  ))}
                  {g.ports.length > 5 ? (
                    <span className="px-1 py-0.5 text-[11px] text-text-subtle">
                      +{g.ports.length - 5}
                    </span>
                  ) : null}
                </span>

                <span className="w-16 shrink-0 text-right text-xs tabular-nums text-text-muted">
                  {g.ports.length} port{g.ports.length === 1 ? "" : "s"}
                </span>
              </button>
            </li>
          );
        })}
      </ul>

      {unattributed > 0 ? (
        <p className="mt-3 border-t border-border pt-2.5 text-[11px] leading-relaxed text-text-subtle">
          {unattributed} socket{unattributed === 1 ? " has" : "s have"} no resolvable owner. Reading
          the process behind a socket needs privilege Atlas does not have here — the socket is real
          and listening; only its owner is unknown.
        </p>
      ) : null}
    </Card>
  );
}
