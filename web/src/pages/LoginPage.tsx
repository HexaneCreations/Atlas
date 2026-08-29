import { useState } from "react";
import { useNavigate } from "react-router";
import { Eye, EyeOff, LineChart, Lock, ShieldCheck, User, Zap, type LucideIcon } from "lucide-react";
import { useAuth } from "../auth/AuthContext";
import { ApiError } from "../api/client";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { brand, maps, patterns } from "../lib/assets";

/**
 * The login screen.
 *
 * A two-panel marketing layout — branding on the left, the sign-in card on
 * the right — rendered full-bleed, outside the sidebar/topbar shell (see
 * App.tsx's route branch on "/login").
 *
 * Only the presentation changed here. The credential is still a username (not
 * an email or a display name), the submit still calls the same
 * POST /api/v1/auth/login through [useAuth], and the same per-username/per-IP
 * rate limiting and wrong-password handling apply unchanged.
 *
 * There is deliberately no self-service anywhere on this page: Atlas has no
 * public signup and no email-based password reset. Account creation and
 * password resets are admin-only actions (the user create + reset-password
 * CLI/API). The "contact your administrator" lines below are static text, not
 * links to flows that do not exist.
 */
export function LoginPage() {
  const { login } = useAuth();
  const navigate = useNavigate();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [showResetHint, setShowResetHint] = useState(false);
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
    <div className="flex min-h-screen bg-bg text-text">
      <BrandingPanel />

      <main className="flex flex-1 flex-col items-center justify-center px-4 py-10">
        <Card level="floating" className="w-full max-w-[26rem] p-7 sm:p-8">
          <div className="flex flex-col items-center text-center">
            <span className="glow-soft rounded-2xl">
              <img
                src={brand.mark}
                alt=""
                aria-hidden="true"
                width={56}
                height={56}
                className="h-14 w-14 drop-shadow-[0_0_16px_rgb(79_127_255/0.55)]"
              />
            </span>
            <h1 className="mt-4 text-2xl font-semibold text-text">Welcome back</h1>
            <p className="mt-1 text-sm text-text-muted">Sign in to your Atlas account</p>
          </div>

          <form
            onSubmit={(e) => {
              e.preventDefault();
              void submitCredentials();
            }}
            className="mt-7 flex flex-col gap-5"
          >
            <label className="flex flex-col gap-1.5 text-sm font-medium text-text">
              Username
              <span className="relative">
                <User
                  size={16}
                  aria-hidden="true"
                  className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 text-text-muted"
                />
                <input
                  type="text"
                  autoComplete="username"
                  placeholder="Enter your username"
                  value={username}
                  onChange={(e) => { setUsername(e.target.value); }}
                  required
                  className="w-full rounded-lg border border-border bg-surface py-2.5 pr-3 pl-9 text-sm text-text outline-none placeholder:text-text-subtle focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary/40"
                />
              </span>
            </label>

            <label className="flex flex-col gap-1.5 text-sm font-medium text-text">
              Password
              <span className="relative">
                <Lock
                  size={16}
                  aria-hidden="true"
                  className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 text-text-muted"
                />
                <input
                  type={showPassword ? "text" : "password"}
                  autoComplete="current-password"
                  placeholder="Enter your password"
                  value={password}
                  onChange={(e) => { setPassword(e.target.value); }}
                  required
                  className="w-full rounded-lg border border-border bg-surface py-2.5 pr-10 pl-9 text-sm text-text outline-none placeholder:text-text-subtle focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary/40"
                />
                <button
                  type="button"
                  onClick={() => { setShowPassword((v) => !v); }}
                  aria-label={showPassword ? "Hide password" : "Show password"}
                  aria-pressed={showPassword}
                  className="absolute top-1/2 right-2 -translate-y-1/2 rounded p-1 text-text-muted hover:text-text"
                >
                  {showPassword ? <EyeOff size={16} /> : <Eye size={16} />}
                </button>
              </span>
            </label>

            <div className="-mt-1 text-right">
              <button
                type="button"
                onClick={() => { setShowResetHint((v) => !v); }}
                aria-expanded={showResetHint}
                className="text-sm font-medium text-primary hover:underline"
              >
                Forgot password?
              </button>
              {showResetHint ? (
                <p className="mt-1 text-xs text-text-muted">
                  Contact your administrator to reset your password.
                </p>
              ) : null}
            </div>

            {error ? <p className="text-sm text-danger">{error}</p> : null}

            <Button type="submit" variant="primary" disabled={submitting} className="w-full">
              {submitting ? "Signing in…" : "Sign in"}
            </Button>
          </form>

          <div className="my-6 flex items-center gap-3">
            <span className="h-px flex-1 bg-border" />
            <span className="text-xs text-text-muted">or</span>
            <span className="h-px flex-1 bg-border" />
          </div>

          {/* Static, intentionally not a link: Atlas has no self-service
              account creation — an admin creates users via the user create
              CLI/API. A live signup path here would undo that decision. */}
          <p className="text-center text-sm text-text-muted">
            Need an account? Contact your administrator.
          </p>
        </Card>

        <p className="mt-6 flex items-center gap-2 text-xs text-text-muted">
          <Lock size={13} aria-hidden="true" />
          Your data is encrypted in transit and at rest
        </p>
      </main>
    </div>
  );
}

interface Feature {
  icon: LucideIcon;
  title: string;
  body: string;
}

const FEATURES: Feature[] = [
  {
    icon: ShieldCheck,
    title: "Secure by Design",
    body: "Built with security-first principles to protect your data and environment.",
  },
  {
    icon: LineChart,
    title: "Real-time Observability",
    body: "Monitor everything that matters with powerful, real-time insights.",
  },
  {
    icon: Zap,
    title: "Fleet Management",
    body: "Manage your entire fleet of agents from a single, intuitive control plane.",
  },
];

function BrandingPanel() {
  return (
    <aside className="relative hidden w-[42%] shrink-0 flex-col justify-between overflow-hidden border-r border-border bg-bg px-12 py-10 lg:flex">
      {/* Decorative layers, in the same order the rest of the app stacks
          them. All aria-hidden — the panel is usable, and the form works, if
          none of these load. */}
      <span
        aria-hidden="true"
        className="pointer-events-none absolute inset-0 opacity-[0.05]"
        style={{ backgroundImage: `url(${patterns.hex})`, backgroundSize: "180px" }}
      />
      <span
        aria-hidden="true"
        className="pointer-events-none absolute -top-24 -left-16 h-72 w-72 rounded-full opacity-40 blur-3xl"
        style={{ background: "var(--grad-primary)" }}
      />
      <img
        src={maps.network}
        alt=""
        aria-hidden="true"
        className="pointer-events-none absolute top-1/2 left-1/2 w-[125%] max-w-none -translate-x-1/2 -translate-y-1/2 opacity-30"
      />

      <div className="relative flex items-center gap-3">
        <img src={brand.mark} alt="" aria-hidden="true" width={36} height={36} className="h-9 w-9" />
        <span className="grad-text text-2xl leading-none font-semibold tracking-[0.2em]">ATLAS</span>
      </div>

      <div className="relative max-w-md">
        <h1 className="text-4xl leading-tight font-semibold text-text">
          Control. Observe.
          <br />
          <span className="text-primary">Securely.</span>
        </h1>
        <p className="mt-5 text-sm leading-relaxed text-text-muted">
          Atlas gives you complete visibility into your infrastructure, applications, and services —
          in real time.
        </p>
      </div>

      <ul className="relative flex flex-col gap-6">
        {FEATURES.map((f) => (
          <li key={f.title} className="flex gap-4">
            <span className="flex h-11 w-11 shrink-0 items-center justify-center rounded-xl border border-border bg-surface text-primary">
              <f.icon size={18} />
            </span>
            <span>
              <span className="block text-sm font-semibold text-text">{f.title}</span>
              <span className="mt-1 block text-xs leading-relaxed text-text-muted">{f.body}</span>
            </span>
          </li>
        ))}
      </ul>

      <p className="relative text-xs text-text-subtle">
        © {new Date().getFullYear()} Atlas. All rights reserved.
      </p>
    </aside>
  );
}
