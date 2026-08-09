import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { Search } from "lucide-react";
import { NAV_PAGES } from "./pages";
import { EmptyState, EmptyAction } from "../components/EmptyState";
import { emptyArt } from "../lib/assets";

/**
 * A quick-jump palette across Atlas's pages, opened from the search box or
 * Cmd/Ctrl+K.
 *
 * This is deliberately just page navigation, not a search over live
 * infrastructure data (container names, process names, node hostnames). That
 * would be a genuinely useful feature, but it means querying several
 * endpoints and merging results into one ranked list — real scope, not a
 * weekend addition to a design pass. A search box that visually promises
 * that and only jumps between six pages would be worse than not having one;
 * this is scoped to what it actually does.
 */
export function CommandPalette() {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [highlighted, setHighlighted] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const navigate = useNavigate();

  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "k") {
        e.preventDefault();
        setOpen((o) => !o);
      }
      if (e.key === "Escape") setOpen(false);
    };
    window.addEventListener("keydown", onKeyDown);
    return () => { window.removeEventListener("keydown", onKeyDown); };
  }, []);

  useEffect(() => {
    if (open) {
      setQuery("");
      setHighlighted(0);
      // Focus after the dialog paints, so the click that opened it doesn't
      // immediately blur the field on some browsers.
      requestAnimationFrame(() => inputRef.current?.focus());
    }
  }, [open]);

  const matches = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return NAV_PAGES;
    return NAV_PAGES.filter((p) => p.label.toLowerCase().includes(q));
  }, [query]);

  // The filtered list shrinks as the operator types; clamp rather than reset
  // so backspacing doesn't jump the selection back to the top every time.
  useEffect(() => {
    setHighlighted((h) => Math.min(h, Math.max(matches.length - 1, 0)));
  }, [matches.length]);

  const go = (to: string) => {
    void navigate(to);
    setOpen(false);
  };

  const onInputKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setHighlighted((h) => Math.min(h + 1, matches.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setHighlighted((h) => Math.max(h - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      const target = matches[highlighted];
      if (target) go(target.to);
    }
  };

  return (
    <>
      <button
        type="button"
        onClick={() => { setOpen(true); }}
        className="flex w-full max-w-80 items-center gap-2.5 rounded-lg border border-border bg-bg px-3 py-2 text-left text-sm text-text-muted transition-colors hover:border-text-muted"
      >
        <Search size={15} />
        <span className="flex-1 truncate">Jump to…</span>
        <kbd className="hidden rounded border border-border px-1.5 py-0.5 font-sans text-[11px] text-text-muted sm:block">
          ⌘K
        </kbd>
      </button>

      {open ? (
        <div
          className="fixed inset-0 z-50 flex items-start justify-center bg-black/50 pt-[15vh]"
          onClick={() => { setOpen(false); }}
        >
          <div
            className="w-full max-w-lg overflow-hidden rounded-xl border border-border bg-surface shadow-2xl"
            onClick={(e) => { e.stopPropagation(); }}
          >
            <div className="flex items-center gap-2.5 border-b border-border px-4 py-3">
              <Search size={16} className="text-text-muted" />
              <input
                ref={inputRef}
                value={query}
                onChange={(e) => { setQuery(e.target.value); }}
                onKeyDown={onInputKeyDown}
                placeholder="Jump to a page…"
                className="flex-1 bg-transparent text-sm text-text outline-none placeholder:text-text-muted"
              />
            </div>
            <ul className="max-h-80 overflow-y-auto p-2">
              {matches.length === 0 ? (
                <li>
                  {/* Compact rather than the full treatment: the palette is a
                      transient overlay a few hundred pixels tall, and a large
                      illustration inside it would push the input off screen. */}
                  <EmptyState
                    kind="filtered"
                    art={emptyArt.search}
                    title={`Nothing matches “${query.trim()}”`}
                    description="The palette searches page names only."
                    action={
                      <EmptyAction variant="ghost" onClick={() => { setQuery(""); }}>
                        Clear search
                      </EmptyAction>
                    }
                    compact
                  />
                </li>
              ) : (
                matches.map((p, i) => (
                  <li key={p.to}>
                    <button
                      type="button"
                      onClick={() => { go(p.to); }}
                      onMouseEnter={() => { setHighlighted(i); }}
                      className={`flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-left text-sm ${
                        i === highlighted ? "bg-primary text-white" : "text-text hover:bg-surface-hover"
                      }`}
                    >
                      <p.icon size={15} className={i === highlighted ? "text-white" : "text-text-muted"} />
                      {p.label}
                    </button>
                  </li>
                ))
              )}
            </ul>
          </div>
        </div>
      ) : null}
    </>
  );
}
