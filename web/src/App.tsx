import { Suspense, lazy, useCallback, useState } from "react";
import { AnimatePresence, motion } from "framer-motion";
import { Route, Routes, useLocation } from "react-router";
import { Sidebar } from "./shell/Sidebar";
import { Topbar } from "./shell/Topbar";
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
import { NotFoundPage } from "./pages/ErrorPages";
import { PageSkeleton } from "./components/Skeleton";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { chromeFor } from "./lib/pageChrome";

/**
 * The Atlas shell: a fixed sidebar and top bar around a routed page body.
 *
 * Routing lives here rather than behind a nested layout route, because there
 * is exactly one layout in this application — every page is a peer inside
 * the same shell. A `<Outlet>`-based layout route earns its keep once a
 * second layout (a full-bleed page, an auth screen) exists; until then it is
 * a level of indirection with nothing on the other side of it.
 */
export function App() {
  const { pathname } = useLocation();
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
