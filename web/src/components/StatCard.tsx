import type { LucideIcon } from "lucide-react";

const ICON_TONE = {
  primary: "bg-primary/10 text-primary",
  success: "bg-success/10 text-success",
  warning: "bg-warning/10 text-warning",
  danger: "bg-danger/10 text-danger",
  info: "bg-info/10 text-info",
  neutral: "bg-surface-hover text-text-muted",
} as const;

/** One headline number: a KPI tile, the "what happened, at a glance" unit
 *  every page in Atlas leads with. Icon badges vary by what the number
 *  means — success-tinted for a count of healthy things, danger-tinted for
 *  a count of problems — the same convention the reference design uses so a
 *  row of tiles reads at a glance before anyone reads the numbers. */
export function StatCard({
  label,
  value,
  icon: Icon,
  sub,
  tone = "neutral",
  iconTone = "primary",
}: {
  label: string;
  value: string;
  icon?: LucideIcon;
  sub?: string | undefined;
  tone?: "neutral" | "warning" | "danger";
  iconTone?: keyof typeof ICON_TONE;
}) {
  const subColor =
    tone === "danger" ? "text-danger" : tone === "warning" ? "text-warning" : "text-text-muted";

  return (
    <div className="elev-2 lift rounded-xl p-5">
      <div className="mb-2 flex items-center justify-between">
        <h3 className="truncate text-label font-medium text-text-muted" title={label}>
          {label}
        </h3>
        {Icon ? (
          <span className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-lg ${ICON_TONE[iconTone]}`}>
            <Icon size={16} />
          </span>
        ) : null}
      </div>
      <p className="text-3xl font-semibold tracking-tight text-text">{value}</p>
      {sub ? <p className={`mt-1 text-xs ${subColor}`}>{sub}</p> : null}
    </div>
  );
}
