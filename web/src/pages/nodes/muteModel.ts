import type { MuteDuration } from "../../api/types";
import { formatDateTime } from "../../format";

/** What the server reports for a mute with no end. */
export const MUTED_INDEFINITELY = "9999-01-01T00:00:00Z";

export const MUTE_CHOICES: { duration: MuteDuration; label: string }[] = [
  { duration: "1h", label: "1 hour" },
  { duration: "4h", label: "4 hours" },
  { duration: "24h", label: "24 hours" },
  { duration: "7d", label: "7 days" },
  { duration: "until_unmuted", label: "Until I unmute" },
];

/** How a mute reads, or null when the node is not muted. */
export function muteLabel(mutedUntil: string | null): string | null {
  if (mutedUntil === null) return null;
  return mutedUntil === MUTED_INDEFINITELY ? "until unmuted" : `muted until ${formatDateTime(mutedUntil)}`;
}
