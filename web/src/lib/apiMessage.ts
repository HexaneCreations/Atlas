import { ApiError } from "../api/client";

/** What to show for a failed write. */
export function messageFor(error: unknown): string {
  return error instanceof ApiError ? error.message : "Could not reach Atlas.";
}
