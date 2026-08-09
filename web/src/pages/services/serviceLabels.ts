import type { EdgeKind, GraphHealth } from "../../api/types";
import type { Tone } from "../../components/Badge";

/**
 * Shared vocabulary for the Services page.
 *
 * systemd's directives are precise and widely misread, so every one is
 * explained in the UI rather than shown as a bare keyword. The distinction
 * that matters most — `After` orders, it does not require — is the one an
 * operator is most likely to get wrong, and it is stated wherever the word
 * appears.
 */

export const HEALTH_LABEL: Record<GraphHealth, string> = {
  healthy: "healthy",
  degraded: "degraded",
  failed: "failed",
  inactive: "inactive",
  unknown: "unknown",
};

export const HEALTH_TONE: Record<GraphHealth, Tone> = {
  healthy: "success",
  degraded: "warning",
  failed: "danger",
  inactive: "neutral",
  unknown: "neutral",
};

/** What each health verdict actually means, for tooltips and legends. */
export const HEALTH_EXPLANATION: Record<GraphHealth, string> = {
  healthy: "Active, and everything it requires is active too.",
  degraded:
    "The unit itself is running, but something it requires has failed. Whether it is actually impaired depends on the service.",
  failed: "systemd has given up on this unit.",
  inactive: "Not running, and not failed. Often correct — a oneshot that finished, or a unit nothing has started.",
  unknown: "Atlas has no state for this unit.",
};

export const KIND_EXPLANATION: Record<EdgeKind, string> = {
  requires:
    "Requires — a hard requirement. If it fails to start, this unit is not started either.",
  wants:
    "Wants — a soft requirement. This unit starts regardless of whether the other one does.",
  binds_to:
    "BindsTo — Requires plus lifecycle. This unit stops if the other stops for any reason.",
  part_of:
    "PartOf — stop and restart propagate from the other unit to this one, but start does not.",
  after:
    "After — ordering only. This unit starts later; it does not require the other one at all.",
  conflicts: "Conflicts — mutual exclusion. Starting one stops the other.",
};
