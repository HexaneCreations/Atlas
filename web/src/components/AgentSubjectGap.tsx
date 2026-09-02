import { Card } from "./Card";
import { EmptyState } from "./EmptyState";
import { PageHeader } from "./PageHeader";
import { emptyArt } from "../lib/assets";

/**
 * Shown when a node is reachable through its agent but that agent's inventory
 * does not include this subject.
 *
 * Distinct from the subject's plugin being absent on the host Atlas itself
 * runs on — "no Docker here", "no systemd here" — which each page words for
 * itself. Both arrive as `not_implemented`; `details.reason` tells them apart
 * (see [inventoryGapKind]).
 */
export function AgentSubjectGap({ subject }: { subject: string }) {
  return (
    <>
      <PageHeader subtitle={`This node's agent does not report ${subject}.`} />
      <Card>
        <EmptyState
          kind="unavailable"
          art={emptyArt.data}
          title={`This node's agent does not report ${subject}`}
          description="Atlas is reaching this node through its agent, and that agent's inventory does not include this section. It is a fact about how the agent is configured, not a failure on this page."
          hint="A co-located agent reports only the subjects its plugins cover. Other sections for this node may still be populated."
        />
      </Card>
    </>
  );
}
