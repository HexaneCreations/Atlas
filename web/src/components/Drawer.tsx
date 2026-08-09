import { useEffect } from "react";
import { X } from "lucide-react";

/**
 * A right-side slide-over for one row's full detail — the same pattern the
 * reference design uses for a process or container detail panel, rather
 * than a modal that blocks the rest of the page: an operator comparing one
 * row's detail against the table it came from is the actual use case, and a
 * modal would hide the table while they do it.
 */
export function Drawer({
  title,
  subtitle,
  onClose,
  children,
}: {
  title: string;
  subtitle?: string;
  onClose: () => void;
  children: React.ReactNode;
}) {
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKeyDown);
    return () => { window.removeEventListener("keydown", onKeyDown); };
  }, [onClose]);

  return (
    <div className="fixed inset-0 z-40 flex justify-end">
      <button
        type="button"
        aria-label="Close"
        onClick={onClose}
        className="absolute inset-0 bg-black/50"
      />
      <aside className="scroll-thin relative flex h-full w-full max-w-md flex-col overflow-y-auto border-l border-border bg-surface p-5 shadow-2xl">
        <div className="mb-5 flex items-start justify-between gap-3">
          <div className="min-w-0">
            <h2 className="truncate text-card-title font-semibold text-text">{title}</h2>
            {subtitle ? <p className="mt-0.5 truncate text-xs text-text-muted">{subtitle}</p> : null}
          </div>
          <button type="button" onClick={onClose} className="shrink-0 text-text-muted hover:text-text">
            <X size={18} />
          </button>
        </div>
        {children}
      </aside>
    </div>
  );
}

/** A label/value row inside a [Drawer]. */
export function DrawerField({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-4 border-b border-border py-2.5 text-sm last:border-0">
      <dt className="text-text-muted">{label}</dt>
      <dd className="max-w-[65%] text-right break-words text-text">{value}</dd>
    </div>
  );
}
