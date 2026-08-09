import { useState } from "react";
import { ChevronDown, Server } from "lucide-react";
import { useNodes, usePrimaryNodeID } from "../api/queries";
import { setSelectedNodeID } from "../lib/selectedNode";
import type { NodeStatus } from "../api/types";

const STATUS_DOT: Record<NodeStatus, string> = {
  up: "bg-success",
  stale: "bg-warning",
  down: "bg-danger",
};

/**
 * Picks which node every page reads. The only UI surface for the Agent
 * milestone: proving a remote node's data is reachable and distinguishable
 * from the control plane's own host.
 */
export function NodeSwitcher() {
  const [open, setOpen] = useState(false);
  const nodes = useNodes();
  const activeID = usePrimaryNodeID();

  const active = nodes.data?.nodes.find((n) => n.node_id === activeID);
  const list = nodes.data?.nodes ?? [];

  if (list.length <= 1) return null;

  return (
    <div className="relative">
      <button
        type="button"
        onClick={() => { setOpen((v) => !v); }}
        className="flex items-center gap-2 rounded-full border border-border bg-surface px-3 py-1.5 text-xs font-medium text-text hover:bg-surface-hover"
      >
        <Server size={13} className="text-text-muted" />
        <span className={`h-1.5 w-1.5 rounded-full ${active ? STATUS_DOT[active.status] : "bg-text-muted"}`} />
        <span className="max-w-[10rem] truncate">{active?.hostname ?? "Select node"}</span>
        <ChevronDown size={13} className="text-text-muted" />
      </button>

      {open ? (
        <>
          <button
            type="button"
            aria-label="Close node switcher"
            className="fixed inset-0 z-40 cursor-default"
            onClick={() => { setOpen(false); }}
          />
          <div className="absolute right-0 z-50 mt-2 w-72 rounded-xl border border-border bg-surface-raised p-1.5 shadow-xl">
            {list.map((n) => (
              <button
                key={n.node_id}
                type="button"
                onClick={() => {
                  setSelectedNodeID(n.node_id);
                  setOpen(false);
                }}
                className={`flex w-full items-center justify-between gap-2 rounded-lg px-3 py-2 text-left text-xs hover:bg-surface-hover ${
                  n.node_id === activeID ? "bg-surface-hover" : ""
                }`}
              >
                <span className="flex min-w-0 items-center gap-2">
                  <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${STATUS_DOT[n.status]}`} />
                  <span className="min-w-0">
                    <span className="block truncate font-medium text-text">{n.hostname}</span>
                    <span className="block truncate text-text-muted">
                      {n.environment ?? "untagged"} · {n.node_id.slice(0, 12)}
                    </span>
                  </span>
                </span>
              </button>
            ))}
          </div>
        </>
      ) : null}
    </div>
  );
}
