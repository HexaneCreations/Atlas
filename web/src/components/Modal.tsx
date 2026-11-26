import { useEffect, useRef, useState } from "react";
import { X } from "lucide-react";
import { FOCUSABLE, focusAfterTab } from "./focusTrap";

/** Open modals, newest last. Only the newest answers the keyboard. */
const openModals: symbol[] = [];

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
  width?: "sm" | "md" | "lg";
}) {
  const dialogRef = useRef<HTMLDivElement>(null);
  const onCloseRef = useRef(onClose);
  // Read while rendering: a child's autoFocus has already moved it by the
  // time an effect runs.
  const [opener] = useState(() => (typeof document === "undefined" ? null : document.activeElement));

  useEffect(() => { onCloseRef.current = onClose; }, [onClose]);

  useEffect(() => {
    const id = Symbol("modal");
    openModals.push(id);
    const dialog = dialogRef.current;
    if (dialog && !dialog.contains(document.activeElement)) dialog.focus();

    const onKeyDown = (e: KeyboardEvent) => {
      if (openModals.at(-1) !== id) return;
      if (e.key === "Escape") {
        onCloseRef.current();
      } else if (e.key === "Tab" && dialog) {
        const inside = Array.from(dialog.querySelectorAll<HTMLElement>(FOCUSABLE));
        const next = focusAfterTab(inside, document.activeElement, e.shiftKey);
        if (next) {
          e.preventDefault();
          next.focus();
        }
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => {
      window.removeEventListener("keydown", onKeyDown);
      openModals.splice(openModals.indexOf(id), 1);
      if (opener instanceof HTMLElement) opener.focus();
    };
  }, [opener]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
      onClick={onClose}
    >
      <div
        ref={dialogRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(e) => { e.stopPropagation(); }}
        className={`scroll-thin max-h-[90vh] w-full overflow-y-auto ${width === "lg" ? "max-w-3xl" : width === "md" ? "max-w-md" : "max-w-sm"} rounded-xl border border-border bg-surface p-5 shadow-2xl outline-none`}
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
