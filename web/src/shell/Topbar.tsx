import { Link } from "react-router";
import { AlertTriangle, Menu } from "lucide-react";
import { useCollectors, useSystemHealth } from "../api/queries";
import type { HealthStatus } from "../api/types";
import { CommandPalette } from "./CommandPalette";
import { NodeSwitcher } from "./NodeSwitcher";

/**
 * The top bar: quick navigation, and the two things worth seeing from every
 * page without navigating away — overall health, and whether any collector
 * is failing to produce the data the rest of the UI depends on.
 *
 * There is no notification bell or user menu here. Atlas has no alerting
 * engine and no authentication yet — a bell with a fabricated badge count,
 * or an avatar for a user model that does not exist, is exactly the kind of
 * placeholder this project was built without from the start.
 */
export function Topbar({ onOpenNav }: { onOpenNav: () => void }) {
  const health = useSystemHealth();
  const collectors = useCollectors();

  const failing = collectors.data?.collectors.filter((c) => !c.healthy).length ?? 0;

  return (
    <header className="flex h-16 shrink-0 items-center justify-between gap-3 border-b border-border px-4 sm:px-6">
      <button
        type="button"
        onClick={onOpenNav}
        aria-label="Open navigation"
        className="shrink-0 rounded-lg p-2 text-text-muted hover:bg-surface-hover hover:text-text lg:hidden"
      >
        <Menu size={18} />
      </button>

      <CommandPalette />

      <div className="flex items-center gap-3">
        {failing > 0 ? (
          <Link
            to="/"
            className="flex items-center gap-1.5 rounded-full border border-warning/40 bg-warning/10 px-3 py-1.5 text-xs font-medium text-warning"
            title={`${failing} collector${failing === 1 ? "" : "s"} failing`}
          >
            <AlertTriangle size={13} />
            {failing} issue{failing === 1 ? "" : "s"}
          </Link>
        ) : null}

        <NodeSwitcher />

        {health.data ? <HealthPill status={health.data.status} /> : null}
      </div>
    </header>
  );
}

/**
 * This pill reports `/api/v1/system/health`, which is Atlas's own dependency
 * health — storage, migrations — and says nothing about the fleet.
 *
 * It used to read "All systems operational", which on a page listing two
 * critical infrastructure findings directly below it was read as a verdict on
 * the user's estate rather than on the monitoring tool. The labels now name
 * their subject, so the pill and the Attention panel cannot appear to
 * contradict each other while both being right.
 */
const TONE: Record<HealthStatus, { dot: string; text: string; label: string; title: string }> = {
  healthy: {
    dot: "bg-success",
    text: "text-success",
    label: "Atlas healthy",
    title: "Atlas's own dependencies are healthy. This does not describe your infrastructure — see Attention required on the Overview.",
  },
  degraded: {
    dot: "bg-warning",
    text: "text-warning",
    label: "Atlas degraded",
    title: "A non-critical dependency of Atlas itself is reporting problems.",
  },
  unhealthy: {
    dot: "bg-danger",
    text: "text-danger",
    label: "Atlas unhealthy",
    title: "A critical dependency of Atlas itself is failing. Data on every page may be stale or incomplete.",
  },
};

function HealthPill({ status }: { status: HealthStatus }) {
  const tone = TONE[status];
  return (
    <span
      title={tone.title}
      className="flex items-center gap-2 rounded-full border border-border bg-surface px-3 py-1.5 text-xs font-medium text-text"
    >
      <span className={`h-1.5 w-1.5 rounded-full ${tone.dot} ${status !== "healthy" ? "animate-pulse-dot" : ""}`} />
      {tone.label}
    </span>
  );
}
