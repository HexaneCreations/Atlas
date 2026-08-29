import { useMemo } from "react";
import { ArrowLeft } from "lucide-react";
import { Link, useParams, useSearchParams } from "react-router";
import { useContainers, useNodes, usePrimaryNodeID } from "../api/queries";
import type { Container, ContainerState } from "../api/types";
import { emptyArray } from "../api/empty";
import { Card } from "../components/Card";
import { Badge, type Tone } from "../components/Badge";
import { nodePrimaryLabel } from "../lib/nodeIdentity";
import { LogViewer } from "./containers/LogViewer";

const STATE_TONE: Record<ContainerState, Tone> = {
  running: "success",
  restarting: "warning",
  paused: "warning",
  created: "info",
  exited: "neutral",
  dead: "danger",
  removing: "neutral",
};

/**
 * The dedicated Logs page for one container: `/containers/:containerID/logs?node=`.
 *
 * Container and node identity live entirely in the URL — not component state
 * — so a direct link, a refresh, and a Cmd/Ctrl-click into a new tab all land
 * on the exact same view. Container and node names come from the list
 * queries [ContainersPage] already populates (shared cache, so usually free)
 * rather than a dedicated per-container fetch: [useContainerDetail]'s
 * `GET /containers/{id}` is local-only and would not resolve for a remote
 * node anyway.
 */
export function ContainerLogsPage() {
  const { containerID } = useParams<{ containerID: string }>();
  const [searchParams] = useSearchParams();
  const primaryNodeID = usePrimaryNodeID();
  // An explicit ?node= always wins, so a link generated for one node still
  // points at that node even after the operator switches the node switcher.
  // Falls back for both an absent param and an empty one (containerLogsPath
  // encodes an unresolved node as `node=`, not as a missing param).
  const nodeParam = searchParams.get("node");
  let nodeID = primaryNodeID;
  if (nodeParam) nodeID = nodeParam;

  const containers = useContainers(nodeID);
  const nodes = useNodes();

  const all = containers.data?.containers ?? emptyArray<Container>();
  const container = useMemo(() => all.find((c) => c.id === containerID), [all, containerID]);
  const node = nodes.data?.nodes.find((n) => n.node_id === nodeID);

  if (!containerID) {
    return null; // unreachable: the route requires this param
  }

  const backHref = nodeID ? `/containers?node=${encodeURIComponent(nodeID)}` : "/containers";

  return (
    <>
      <Link
        to={backHref}
        className="mb-4 inline-flex items-center gap-1.5 text-sm text-text-muted hover:text-text"
      >
        <ArrowLeft size={14} />
        Back to containers
      </Link>

      <Card level="floating" className="mb-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <h1 className="truncate text-lg font-semibold text-text" title={container?.name ?? containerID}>
              {container?.name ?? containerID}
            </h1>
            <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-text-muted">
              <span>
                Node:{" "}
                <span className="text-text">
                  {node ? nodePrimaryLabel(node) : (nodeID ?? "—")}
                </span>
              </span>
              {container ? (
                <>
                  <Badge tone={STATE_TONE[container.state]} pulse={container.state === "restarting"}>
                    {container.state}
                  </Badge>
                  <span className="truncate" title={container.image}>{container.image}</span>
                </>
              ) : containers.isPending ? (
                <span>Loading container…</span>
              ) : (
                <span className="text-warning">
                  Not in this node's current inventory — it may have been removed. Showing logs by id.
                </span>
              )}
              <span className="font-mono text-[11px] text-text-subtle">{containerID.slice(0, 12)}</span>
            </div>
          </div>
        </div>
      </Card>

      <Card level="floating">
        <LogViewer
          containerID={containerID}
          nodeID={nodeID}
          containerRef={container?.name ?? container?.short_id ?? containerID.slice(0, 12)}
        />
      </Card>
    </>
  );
}
