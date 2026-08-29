/**
 * How a node is named in the UI.
 *
 * `node_id` is the stable identity: it is derived from the OS machine id (see
 * internal/platform/hostid) or set explicitly by the operator (ATLAS_NODE_ID),
 * and it survives hostname changes and re-imaging. `hostname` is mutable
 * display detail — a container restart alone rewrites it, which made every
 * restart of a self-monitored server look like a brand-new node.
 *
 * So `node_id` is the primary/bold label everywhere a node is shown, and
 * `hostname` is secondary context. This module is the one place that policy
 * lives; call sites ask it rather than reaching for `.hostname` themselves.
 */

interface NodeLike {
  node_id: string;
  hostname?: string | null;
}

/** The bold, human-recognizable label for a node: its stable id. */
export function nodePrimaryLabel(node: NodeLike): string {
  return node.node_id;
}

/**
 * The hostname, for display as smaller secondary text — or `undefined` when
 * it would add nothing (absent, blank, or identical to the id already shown
 * as the primary label).
 */
export function nodeSecondaryLabel(node: NodeLike): string | undefined {
  const hostname = node.hostname?.trim();
  if (!hostname || hostname === node.node_id) return undefined;
  return hostname;
}
