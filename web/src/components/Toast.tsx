import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { AnimatePresence, motion } from "framer-motion";
import { AlertTriangle, CheckCircle2, Info, X, XCircle } from "lucide-react";
import { fadeUp } from "../lib/motion";

export type ToastTone = "info" | "success" | "warning" | "danger";

interface Toast {
  id: number;
  tone: ToastTone;
  title: string;
  description?: string;
}

interface ToastApi {
  /** Shows a toast. Returns its id so a caller can dismiss it early. */
  push: (t: Omit<Toast, "id">) => number;
  dismiss: (id: number) => void;
}

const ToastContext = createContext<ToastApi | null>(null);

/** How long a toast stays before dismissing itself. */
const LIFETIME_MS = 6_000;

const TONE = {
  info: { icon: Info, cls: "text-info" },
  success: { icon: CheckCircle2, cls: "text-success" },
  warning: { icon: AlertTriangle, cls: "text-warning" },
  danger: { icon: XCircle, cls: "text-danger" },
} as const;

/**
 * Toasts.
 *
 * These are for things that happen *to* the page while the operator is
 * looking elsewhere — a WebSocket dropping, a refresh failing repeatedly.
 * They are not used to confirm actions, because Atlas has no actions to
 * confirm; a read-only tool that toasts "loaded successfully" is noise.
 */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);

  const dismiss = useCallback((id: number) => {
    setToasts((list) => list.filter((t) => t.id !== id));
  }, []);

  const push = useCallback(
    (t: Omit<Toast, "id">) => {
      const id = Date.now() + Math.random();
      setToasts((list) => [...list, { ...t, id }]);
      window.setTimeout(() => { dismiss(id); }, LIFETIME_MS);
      return id;
    },
    [dismiss],
  );

  const api = useMemo(() => ({ push, dismiss }), [push, dismiss]);

  return (
    <ToastContext.Provider value={api}>
      {children}
      <div
        role="region"
        aria-label="Notifications"
        aria-live="polite"
        className="pointer-events-none fixed right-4 bottom-4 z-[60] flex w-full max-w-sm flex-col gap-2"
      >
        <AnimatePresence initial={false}>
          {toasts.map((t) => {
            const { icon: Icon, cls } = TONE[t.tone];
            return (
              <motion.div
                key={t.id}
                layout
                variants={fadeUp}
                initial="hidden"
                animate="visible"
                exit={{ opacity: 0, x: 16 }}
                className="pointer-events-auto flex items-start gap-3 rounded-xl border border-border bg-surface p-3.5 shadow-lg"
              >
                <Icon size={16} className={`mt-0.5 shrink-0 ${cls}`} />
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium text-text">{t.title}</p>
                  {t.description ? (
                    <p className="mt-0.5 text-xs text-text-muted">{t.description}</p>
                  ) : null}
                </div>
                <button
                  type="button"
                  onClick={() => { dismiss(t.id); }}
                  aria-label="Dismiss"
                  className="shrink-0 text-text-muted hover:text-text"
                >
                  <X size={14} />
                </button>
              </motion.div>
            );
          })}
        </AnimatePresence>
      </div>
    </ToastContext.Provider>
  );
}

/** Access the toast API. Throws outside a [ToastProvider], which is a wiring
 *  bug rather than a runtime condition worth handling. */
export function useToast(): ToastApi {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error("useToast must be used within a ToastProvider");
  return ctx;
}
