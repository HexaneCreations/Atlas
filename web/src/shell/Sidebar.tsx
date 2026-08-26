import { useEffect } from "react";
import { NavLink, useLocation } from "react-router";
import { AnimatePresence, motion } from "framer-motion";
import { BrandHeader } from "./BrandHeader";
import { Moon, Sun, X, type LucideIcon } from "lucide-react";
import { useSystemInfo } from "../api/queries";
import { useAuth } from "../auth/AuthContext";
import { useTheme } from "./useTheme";
import { NAV_PAGES } from "./pages";

/**
 * The primary navigation.
 *
 * Every entry corresponds to a real, working page backed by live data — see
 * ./pages.ts. The product vision has more sections than this — alerting, a
 * service catalog, cost reporting, settings — but none of that exists yet,
 * and a nav item leading to a page with nothing behind it is worse than no
 * nav item at all: it reads as broken rather than as "not built yet".
 */
const MONITOR_ITEMS = NAV_PAGES.filter((p) => p.section === "Monitor");
const INFRASTRUCTURE_ITEMS = NAV_PAGES.filter((p) => p.section === "Infrastructure");
const ADMIN_ITEMS = NAV_PAGES.filter((p) => p.section === "Admin");

/**
 * The sidebar.
 *
 * Below `lg` it is a slide-over drawer rather than a permanent column: at
 * 390px a fixed 240px rail leaves 150px for the actual dashboard, which is
 * not a narrower layout so much as an unusable one.
 */
export function Sidebar({ open, onClose }: { open: boolean; onClose: () => void }) {
  const location = useLocation();

  // Navigating on mobile should dismiss the drawer; leaving it open over the
  // page the operator just asked for is the classic mobile-nav bug.
  useEffect(() => { onClose(); }, [location.pathname, onClose]);

  return (
    <>
      <aside
        aria-label="Main navigation"
        className="hidden h-screen w-60 shrink-0 flex-col border-r border-border bg-bg lg:flex"
      >
        <SidebarBody />
      </aside>

      <AnimatePresence>
        {open ? (
          <div className="fixed inset-0 z-50 lg:hidden">
            <motion.button
              type="button"
              aria-label="Close navigation"
              onClick={onClose}
              initial={{ opacity: 0 }}
              animate={{ opacity: 1 }}
              exit={{ opacity: 0 }}
              className="absolute inset-0 bg-black/60"
            />
            <motion.aside
              initial={{ x: "-100%" }}
              animate={{ x: 0 }}
              exit={{ x: "-100%" }}
              transition={{ duration: 0.2, ease: [0.22, 1, 0.36, 1] }}
              className="relative flex h-full w-64 flex-col border-r border-border bg-bg"
            >
              <button
                type="button"
                onClick={onClose}
                aria-label="Close navigation"
                className="absolute top-5 right-4 text-text-muted hover:text-text"
              >
                <X size={18} />
              </button>
              <SidebarBody />
            </motion.aside>
          </div>
        ) : null}
      </AnimatePresence>
    </>
  );
}

function SidebarBody() {
  const info = useSystemInfo();
  const { theme, toggle } = useTheme();
  const { user } = useAuth();

  return (
    <>
      <BrandHeader />

      <nav className="scroll-thin flex-1 overflow-y-auto px-3 pt-4">
        <NavSection label="Monitor" items={MONITOR_ITEMS} />
        <NavSection label="Infrastructure" items={INFRASTRUCTURE_ITEMS} />
        {/* Only a caller Atlas itself has told us can manage users sees this
            — see CurrentUser.can_manage_users. The backend enforces the real
            boundary independently on every /users request either way. */}
        {user?.can_manage_users ? <NavSection label="Admin" items={ADMIN_ITEMS} /> : null}
      </nav>

      <div className="border-t border-border px-4 py-3">
        <button
          type="button"
          onClick={toggle}
          className="mb-3 flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm text-text-muted hover:bg-surface-hover hover:text-text"
        >
          {theme === "dark" ? <Moon size={15} /> : <Sun size={15} />}
          {theme === "dark" ? "Dark" : "Light"}
        </button>
        <p className="px-2 text-xs text-text-muted">
          {info.data ? (
            <>
              Atlas {info.data.version}
              {info.data.dirty ? " (dirty)" : ""}
            </>
          ) : (
            "Atlas"
          )}
        </p>
      </div>
    </>
  );
}

function NavSection({
  label,
  items,
}: {
  label: string;
  items: { to: string; label: string; icon: LucideIcon; end?: boolean }[];
}) {
  return (
    <div className="mb-5">
      <p className="mb-1.5 px-2 text-[11px] font-semibold uppercase tracking-wider text-text-muted">
        {label}
      </p>
      <ul className="flex flex-col gap-0.5">
        {items.map(({ to, label: itemLabel, icon: Icon, end }) => (
          <li key={to}>
            <NavLink
              to={to}
              end={end ?? false}
              className={({ isActive }) =>
                `flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm font-medium transition-colors ${
                  isActive
                    ? "bg-gradient-to-b from-primary-hover to-primary text-white shadow-[0_2px_8px_-2px_var(--primary)]"
                    : "text-text-muted hover:bg-surface-hover hover:text-text"
                }`
              }
            >
              <Icon size={16} strokeWidth={2} />
              {itemLabel}
            </NavLink>
          </li>
        ))}
      </ul>
    </div>
  );
}
