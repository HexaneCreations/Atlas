import { useState } from "react";
import { useNavigate } from "react-router";
import { useAuth } from "../auth/AuthContext";
import { ApiError } from "../api/client";
import { Button } from "../components/Button";
import { Card } from "../components/Card";

/**
 * The login screen.
 *
 * Deliberately plain: this establishes protected-route behaviour end to end
 * (see App.tsx's route guard), and visual polish is a fast follow rather
 * than a blocker on that. It renders full-bleed, outside the sidebar/topbar
 * shell — the same reasoning [OfflinePage] would use if it were wired in:
 * navigation for pages you cannot yet reach is not useful chrome to show.
 */
export function LoginPage() {
  const { login } = useAuth();
  const navigate = useNavigate();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function submitCredentials() {
    setError(null);
    setSubmitting(true);
    try {
      await login(username, password);
      void navigate("/", { replace: true });
    } catch (cause) {
      setError(cause instanceof ApiError ? cause.message : "Could not reach Atlas.");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-bg px-4">
      <Card level="floating" className="w-full max-w-sm p-6">
        <h1 className="text-lg font-semibold text-text">Sign in to Atlas</h1>
        <p className="mt-1 text-sm text-text-muted">Observe everything. Control nothing.</p>

        <form
          onSubmit={(e) => {
            e.preventDefault();
            void submitCredentials();
          }}
          className="mt-6 flex flex-col gap-4"
        >
          <label className="flex flex-col gap-1.5 text-sm text-text">
            Username
            <input
              type="text"
              autoComplete="username"
              value={username}
              onChange={(e) => { setUsername(e.target.value); }}
              required
              className="rounded-lg border border-border bg-surface px-3 py-2 text-sm text-text outline-none focus-visible:ring-2 focus-visible:ring-primary"
            />
          </label>
          <label className="flex flex-col gap-1.5 text-sm text-text">
            Password
            <input
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => { setPassword(e.target.value); }}
              required
              className="rounded-lg border border-border bg-surface px-3 py-2 text-sm text-text outline-none focus-visible:ring-2 focus-visible:ring-primary"
            />
          </label>

          {error ? <p className="text-sm text-danger">{error}</p> : null}

          <Button type="submit" variant="primary" disabled={submitting} className="mt-2 w-full">
            {submitting ? "Signing in…" : "Sign in"}
          </Button>
        </form>
      </Card>
    </div>
  );
}
