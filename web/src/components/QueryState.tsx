import type { ReactNode } from "react";
import { EmptyAction, EmptyState, type EmptyKind } from "./EmptyState";
import { ListSkeleton } from "./Skeleton";
import { emptyArt, errorArt } from "../lib/assets";

/**
 * The loading / error / empty triad, consistent across every page and table.
 *
 * Distinguishing these three is not optional in an operations tool. Rendered
 * identically — which, as three near-identical grey paragraphs, they
 * effectively were — a broken query reads as a healthy, quiet system. Each
 * now has its own shape, its own artwork and its own tone: a skeleton that
 * looks like data arriving, an amber failure that says what broke and offers
 * a retry, and a composed empty state that says why there is nothing.
 *
 * Lives beside [EmptyState] rather than inside PageHeader, where it used to
 * sit: it is a data-state primitive, not part of the page header, and eight
 * files were importing it from a module about titles.
 */

export interface EmptyDescriptor {
  /** A URL from lib/assets. */
  art?: string;
  title: string;
  description?: string;
  /** The quiet line on *why* — interval, permission, socket. */
  hint?: string;
  action?: ReactNode;
  kind?: EmptyKind;
}

export function QueryState({
  isPending,
  error,
  isEmpty,
  emptyText,
  empty,
  onRetry,
  rows = 3,
}: {
  isPending: boolean;
  error?: Error | null;
  isEmpty?: boolean;
  /** Shorthand for `empty={{ title }}`. */
  emptyText?: string;
  /** The composed empty state. Takes precedence over `emptyText`. */
  empty?: EmptyDescriptor;
  /** Usually a TanStack Query `refetch`. Without it the error state has
   *  nothing to offer but the message. */
  onRetry?: () => void;
  /** Skeleton rows while pending. Match roughly what the panel will hold. */
  rows?: number;
}): ReactNode {
  if (isPending) return <ListSkeleton rows={rows} />;

  if (error) {
    return (
      <EmptyState
        kind="unavailable"
        art={errorArt.offline}
        title="This query failed"
        description={error.message}
        hint="The panel is empty because Atlas could not complete the request — not because the resource has nothing in it."
        compact
        {...(onRetry ? { action: <EmptyAction onClick={onRetry}>Try again</EmptyAction> } : {})}
      />
    );
  }

  if (isEmpty) {
    const d: EmptyDescriptor = empty ?? {
      art: emptyArt.data,
      title: emptyText ?? "Nothing here yet.",
    };
    return (
      <EmptyState
        art={d.art ?? emptyArt.data}
        title={d.title}
        kind={d.kind ?? "empty"}
        compact
        {...(d.description ? { description: d.description } : {})}
        {...(d.hint ? { hint: d.hint } : {})}
        {...(d.action ? { action: d.action } : {})}
      />
    );
  }

  return null;
}
