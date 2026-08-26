import { Suspense, lazy, useCallback, useState } from "react";
import { AnimatePresence, motion } from "framer-motion";
import { Navigate, Route, Routes, useLocation } from "react-router";
import { Sidebar } from "./shell/Sidebar";
import { Topbar } from "./shell/Topbar";
import { LoginPage } from "./pages/LoginPage";
import { useAuth } from "./auth/AuthContext";
const OverviewPage = lazy(() =>
  import("./pages/OverviewPage").then((m) => ({ default: m.OverviewPage })),
);
const NodesPage = lazy(() =>
  import("./pages/NodesPage").then((m) => ({ default: m.NodesPage })),
);
const ContainersPage = lazy(() =>
  import("./pages/ContainersPage").then((m) => ({ default: m.ContainersPage })),
);
const ContainerLogsPage = lazy(() =>
  import("./pages/ContainerLogsPage").then((m) => ({ default: m.ContainerLogsPage })),
);
const ProcessesPage = lazy(() =>
  import("./pages/ProcessesPage").then((m) => ({ default: m.ProcessesPage })),
);
const ServicesPage = lazy(() =>
  import("./pages/ServicesPage").then((m) => ({ default: m.ServicesPage })),
);
const CronPage = lazy(() =>
  import("./pages/CronPage").then((m) => ({ default: m.CronPage })),
);
const PortsPage = lazy(() =>
  import("./pages/PortsPage").then((m) => ({ default: m.PortsPage })),
);
const DisksPage = lazy(() =>
  import("./pages/DisksPage").then((m) => ({ default: m.DisksPage })),
);
const UsersPage = lazy(() =>
  import("./pages/UsersPage").then((m) => ({ default: m.UsersPage })),
);
import { NotFoundPage } from "./pages/ErrorPages";
import { PageSkeleton } from "./components/Skeleton";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { chromeFor } from "./lib/pageChrome";

/**
 * The Atlas shell: a fixed sidebar and top bar around a routed page body.
 *
 * Routing lives here rather than behind a nested layout route, because there
 * are exactly two layouts in this application: the shell below, and the
 * full-bleed login screen, which is simple enough to branch on directly
 * without a `<Outlet>`-based layout route. A third layout is what would
 * earn that indirection.
 */
export function App() {
  const { pathname } = useLocation();
  const { user, isLoading } = useAuth();

  // The login screen is deliberately outside the shell entirely: showing a
  // sidebar full of links that would all 401 before a session exists is not
  // useful chrome, and an already-authenticated visit to /login goes
  // straight back in rather than showing a form with nothing to do.
  if (pathname === "/login") {
    return user ? <Navigate to="/" replace /> : <LoginPage />;
  }

  // Nothing is known yet about whether a session exists — rendering the
  // shell now would flash protected content for a moment even when the
  // caller turns out to be anonymous.
  if (isLoading) {
    return <PageSkeleton />;
  }

  if (!user) {
    return <Navigate to="/login" replace />;
  }

  return <AuthenticatedApp pathname={pathname} />;
}

function AuthenticatedApp({ pathname }: { pathname: string }) {
  const chrome = chromeFor(pathname);
  const [navOpen, setNavOpen] = useState(false);
  const closeNav = useCallback(() => {
    setNavOpen(false);
  }, []);

  return (
    <div className="flex h-screen overflow-hidden bg-bg text-text">
      <Sidebar open={navOpen} onClose={closeNav} />
      <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
        <Topbar
          onOpenNav={() => {
            setNavOpen(true);
          }}
        />
        <main
          className="pattern-layer scroll-thin relative flex-1 overflow-y-auto p-4 sm:p-6"
          style={{ ["--pattern-url" as string]: `url(${chrome.pattern})` }}
        >
          <ErrorBoundary>
            <Suspense fallback={<PageSkeleton />}>
              {/* Keyed on the path so each navigation fades rather than
                  snapping. Short and opacity-only: a slide or a scale on a
                  dashboard reads as the layout breaking. */}
              <AnimatePresence mode="wait">
                <motion.div
                  key={pathname}
                  initial={{ opacity: 0 }}
                  animate={{ opacity: 1 }}
                  transition={{ duration: 0.18, ease: [0.22, 1, 0.36, 1] }}
                >
                  <Routes>
                    <Route path="/" element={<OverviewPage />} />
                    <Route path="/nodes" element={<NodesPage />} />
                    <Route path="/containers" element={<ContainersPage />} />
                    <Route path="/containers/:containerID/logs" element={<ContainerLogsPage />} />
                    <Route path="/processes" element={<ProcessesPage />} />
                    <Route path="/services" element={<ServicesPage />} />
                    <Route path="/cron" element={<CronPage />} />
                    <Route path="/ports" element={<PortsPage />} />
                    <Route path="/disks" element={<DisksPage />} />
                    <Route path="/users" element={<UsersPage />} />
                    <Route path="*" element={<NotFoundPage />} />
                  </Routes>
                </motion.div>
              </AnimatePresence>
            </Suspense>
          </ErrorBoundary>
        </main>
      </div>
    </div>
  );
}
