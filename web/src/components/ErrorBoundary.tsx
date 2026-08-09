import { Component, type ErrorInfo, type ReactNode } from "react";
import { CrashPage } from "../pages/ErrorPages";

interface State {
  error: Error | null;
}

/**
 * Catches render errors so one broken panel cannot blank the whole tool.
 *
 * This is a class component because React offers no hook equivalent —
 * componentDidCatch has no functional counterpart. The distinction the
 * fallback draws matters in an operations context: a crash here is a bug in
 * Atlas's own interface, and an operator must not spend the first minutes of
 * an incident wondering whether their infrastructure caused it.
 */
export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  override state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  override componentDidCatch(error: Error, info: ErrorInfo) {
    // Left in the console deliberately: there is no error-reporting backend,
    // and swallowing it would make the bug harder to find, not easier.
    console.error("Atlas UI crashed:", error, info.componentStack);
  }

  override render() {
    if (this.state.error) {
      return <CrashPage onReload={() => { window.location.reload(); }} />;
    }
    return this.props.children;
  }
}
