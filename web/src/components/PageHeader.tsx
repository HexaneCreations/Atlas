import type { ReactNode } from "react";
import { useLocation } from "react-router";
import { PageHero, type HeroStat } from "./PageHero";
import { chromeFor } from "../lib/pageChrome";

/**
 * The title block every page opens with.
 *
 * It renders a full [PageHero] rather than plain text, and picks its imagery
 * from the route: the asset library is explicit that a plain black hero is
 * not an option where a background exists. Deriving the chrome from the
 * current path rather than from a prop means every page gets the treatment
 * automatically, and no page can drift onto imagery that contradicts its
 * subject.
 */
export function PageHeader({
  subtitle,
  action,
  stats,
}: {
  /** Retained for call sites that still pass it; the hero's headline comes
   *  from the route's identity, so a page cannot title itself something
   *  other than what the navigation calls it. */
  title?: string;
  subtitle?: string;
  action?: ReactNode;
  /** Live figures for the hero. Without these the most prominent area on
   *  the page says nothing an operator did not already know. */
  stats?: HeroStat[];
}) {
  const { pathname } = useLocation();
  const chrome = chromeFor(pathname);

  return (
    <PageHero
      identity={chrome.identity}
      premise={subtitle ?? chrome.premise}
      hero={chrome.hero}
      accent={chrome.accent}
      pattern={chrome.pattern}
      map={chrome.map}
      {...(action ? { action } : {})}
      {...(stats ? { stats } : {})}
    />
  );
}
