export type Tone = "success" | "warning" | "danger" | "info" | "neutral";

const TONE_CLASSES: Record<Tone, { badge: string; dot: string }> = {
  success: { badge: "border-success/30 bg-success/10 text-success", dot: "bg-success" },
  warning: { badge: "border-warning/30 bg-warning/10 text-warning", dot: "bg-warning" },
  danger: { badge: "border-danger/30 bg-danger/10 text-danger", dot: "bg-danger" },
  info: { badge: "border-info/30 bg-info/10 text-info", dot: "bg-info" },
  neutral: { badge: "border-border bg-surface-hover text-text-muted", dot: "bg-text-muted" },
};

/**
 * A status pill: a coloured dot plus text, never colour alone.
 *
 * The dot is `aria-hidden` and the text carries the actual meaning, so a
 * viewer who cannot distinguish the tones loses nothing — the same
 * commitment the rest of Atlas's UI makes everywhere status appears.
 */
export function Badge({
  tone,
  children,
  pulse = false,
}: {
  tone: Tone;
  children: React.ReactNode;
  pulse?: boolean;
}) {
  const t = TONE_CLASSES[tone];
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium ${t.badge}`}
    >
      <span
        aria-hidden="true"
        className={`h-1.5 w-1.5 rounded-full ${t.dot} ${pulse ? "animate-pulse-dot" : ""}`}
      />
      {children}
    </span>
  );
}
