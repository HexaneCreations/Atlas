import type { ButtonHTMLAttributes, ReactNode } from "react";
import type { LucideIcon } from "lucide-react";

type Variant = "primary" | "secondary" | "ghost" | "danger";
type Size = "sm" | "md";

const VARIANTS: Record<Variant, string> = {
  primary:
    "bg-gradient-to-b from-primary-hover to-primary text-white hover:brightness-110 shadow-[0_2px_8px_-2px_var(--primary)]",
  secondary: "border border-border bg-surface text-text hover:bg-surface-hover",
  ghost: "text-text-muted hover:bg-surface-hover hover:text-text",
  danger: "border border-danger/40 bg-danger/10 text-danger hover:bg-danger/20",
};

const SIZES: Record<Size, string> = {
  sm: "h-8 px-3 text-xs gap-1.5",
  md: "h-9 px-4 text-sm gap-2",
};

/**
 * The button.
 *
 * Note what is absent: there is no "destructive action" story here beyond the
 * danger variant's colour, because Atlas has no destructive actions. It is
 * read-only by design, so every button in this application navigates,
 * filters, or reveals. The danger variant exists for surfacing a failed
 * state, not for triggering one.
 */
export function Button({
  variant = "secondary",
  size = "md",
  icon: Icon,
  iconRight,
  children,
  className = "",
  ...rest
}: {
  variant?: Variant;
  size?: Size;
  icon?: LucideIcon;
  iconRight?: boolean;
  children?: ReactNode;
} & ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button
      className={`inline-flex items-center justify-center rounded-lg font-medium transition-all focus-visible:ring-2 focus-visible:ring-primary focus-visible:outline-none disabled:pointer-events-none disabled:opacity-50 ${VARIANTS[variant]} ${SIZES[size]} ${className}`}
      {...rest}
    >
      {Icon && !iconRight ? <Icon size={size === "sm" ? 13 : 15} /> : null}
      {children}
      {Icon && iconRight ? <Icon size={size === "sm" ? 13 : 15} /> : null}
    </button>
  );
}
