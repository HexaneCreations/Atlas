import { useEffect } from "react";
import { X } from "lucide-react";

/**
 * A centered, blocking dialog — distinct from [Drawer]'s side panel.
 *
 * A drawer suits inspecting one row alongside the table it came from; this
 * suits a short, focused task that should have the operator's full attention
 * before anything else on the page is usable again — creating a user,
 * confirming a role revoke, revealing a one-time password. Atlas had no such
 * task before the admin Users page: every other page only navigates,
 * filters, or reveals (see Button's own doc), so there was nothing here to
 * reuse.
 */
export function Modal({
  title,
  onClose,
  children,
  width = "sm",
}: {
  title: string;
  onClose: () => void;
  children: React.ReactNode;
  width?: "sm" | "md";
}) {
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKeyDown);
    return () => { window.removeEventListener("keydown", onKeyDown); };
  }, [onClose]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
      onClick={onClose}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(e) => { e.stopPropagation(); }}
        className={`w-full ${width === "md" ? "max-w-md" : "max-w-sm"} rounded-xl border border-border bg-surface p-5 shadow-2xl`}
      >
        <div className="mb-4 flex items-start justify-between gap-3">
          <h2 className="text-card-title font-semibold text-text">{title}</h2>
          <button type="button" onClick={onClose} aria-label="Close" className="shrink-0 text-text-muted hover:text-text">
            <X size={18} />
          </button>
        </div>
        {children}
      </div>
    </div>
  );
}

/** The footer button row every [Modal] in this file ends with. */
export function ModalActions({ children }: { children: React.ReactNode }) {
  return <div className="mt-5 flex items-center justify-end gap-2">{children}</div>;
}
