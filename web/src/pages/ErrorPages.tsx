import { Link } from "react-router";
import { ArrowLeft, RefreshCw } from "lucide-react";
import { motion } from "framer-motion";
import { errorArt } from "../lib/assets";
import { fadeUp } from "../lib/motion";
import { Button } from "../components/Button";

/**
 * Full-page error screens.
 *
 * Each one says what happened, why it might have happened, and what the
 * operator can do next — the same three questions every screen in Atlas is
 * expected to answer. The illustration is decorative; removing it would cost
 * nothing but the mood.
 */
function ErrorScreen({
  art,
  code,
  title,
  description,
  action,
}: {
  art: string;
  code?: string;
  title: string;
  description: string;
  action?: React.ReactNode;
}) {
  return (
    <motion.div
      variants={fadeUp}
      initial="hidden"
      animate="visible"
      className="flex min-h-[70vh] flex-col items-center justify-center px-6 text-center"
    >
      <img
        src={art}
        alt=""
        aria-hidden="true"
        className="illus mb-8 w-full max-w-sm"
      />
      {code ? (
        <p className="mb-2 font-mono text-xs tracking-widest text-text-muted uppercase">{code}</p>
      ) : null}
      <h1 className="text-section font-semibold tracking-tight text-text">{title}</h1>
      <p className="mt-2 max-w-md text-sm text-text-muted">{description}</p>
      <div className="mt-7 flex flex-wrap items-center justify-center gap-3">
        {action}
        <Link to="/">
          <Button variant="secondary" icon={ArrowLeft}>
            Back to overview
          </Button>
        </Link>
      </div>
    </motion.div>
  );
}

/** The catch-all route. */
export function NotFoundPage() {
  return (
    <ErrorScreen
      art={errorArt.notFound}
      code="404"
      title="This page does not exist"
      description="The address you followed is not part of Atlas. If you arrived from a bookmark, the page may have moved between versions."
    />
  );
}

/**
 * Shown when Atlas itself cannot be reached.
 *
 * Distinct from a failed query inside a panel: if the whole API is
 * unreachable, the shell around it is showing stale data, and saying so is
 * more useful than letting every panel fail independently.
 */
export function OfflinePage({ onRetry }: { onRetry?: () => void }) {
  return (
    <ErrorScreen
      art={errorArt.offline}
      title="Cannot reach Atlas"
      description="The API is not responding. Atlas may be restarting, or the network path between this browser and the server may be down. Collection continues on the host regardless — nothing is lost while this page cannot see it."
      action={
        onRetry ? (
          <Button variant="primary" icon={RefreshCw} onClick={onRetry}>
            Try again
          </Button>
        ) : null
      }
    />
  );
}

/** Rendered by the top-level error boundary. */
export function CrashPage({ onReload }: { onReload: () => void }) {
  return (
    <ErrorScreen
      art={errorArt.serverError}
      code="Unexpected error"
      title="Something broke in the interface"
      description="This is a bug in Atlas's frontend, not a problem with the infrastructure it is monitoring. Reloading usually clears it; the details are in the browser console."
      action={
        <Button variant="primary" icon={RefreshCw} onClick={onReload}>
          Reload
        </Button>
      }
    />
  );
}
