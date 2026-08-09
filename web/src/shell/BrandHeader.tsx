import { Link } from "react-router";
import { brand, patterns } from "../lib/assets";

/**
 * The brand block at the top of the sidebar.
 *
 * This is the first thing anyone sees, so it is built as a considered panel
 * rather than a favicon beside a label: the official mark at a size where its
 * detail is legible, a violet bloom behind it, a hairline-glass surface, and
 * the product's motto set quietly underneath.
 *
 * The wordmark is set in type rather than using the packaged lockup image.
 * That is not a shortcut — the lockup has "Observe. Understand. Optimize."
 * baked into it, and Atlas's motto is "Observe Everything. Control Nothing."
 * Shipping the image would put a tagline on screen that contradicts the
 * product's central promise, and the promise is the part that matters. The
 * mark itself is the supplied file, unmodified apart from having its opaque
 * white ground keyed out so it can sit on a dark surface.
 */
export function BrandHeader({ compact = false }: { compact?: boolean }) {
  return (
    <Link
      to="/"
      aria-label="Atlas — go to overview"
      className="group relative block overflow-hidden border-b border-border px-5 py-6"
    >
      {/* Pattern, bloom and gloss, in the layering order the rest of the
          application uses. */}
      <span
        aria-hidden="true"
        className="pointer-events-none absolute inset-0 opacity-[0.06]"
        style={{ backgroundImage: `url(${patterns.hex})`, backgroundSize: "160px" }}
      />
      <span
        aria-hidden="true"
        className="pointer-events-none absolute -top-16 -left-10 h-40 w-40 rounded-full opacity-40 blur-3xl transition-opacity duration-500 group-hover:opacity-60"
        style={{ background: "var(--grad-primary)" }}
      />
      <span
        aria-hidden="true"
        className="pointer-events-none absolute inset-x-0 top-0 h-px"
        style={{ background: "linear-gradient(90deg, transparent, var(--primary), transparent)" }}
      />

      <span className="relative flex items-center gap-3.5">
        <span className="glow-soft shrink-0 rounded-xl">
          <img
            src={brand.mark}
            alt=""
            aria-hidden="true"
            width={40}
            height={40}
            className="h-10 w-10 drop-shadow-[0_0_12px_rgb(79_127_255/0.55)]"
          />
        </span>

        <span className="min-w-0">
          <span className="grad-text block text-xl leading-none font-semibold tracking-[0.18em]">
            ATLAS
          </span>
          {!compact ? (
            // The motto, at a size that reads as a mark of intent rather than
            // as body copy. It is the product's one-line thesis and belongs
            // next to the logo, not buried in a footer.
            <span className="mt-1.5 block text-[10px] leading-tight tracking-wide text-text-muted">
              Observe everything.
              <br />
              Control nothing.
            </span>
          ) : null}
        </span>
      </span>
    </Link>
  );
}
