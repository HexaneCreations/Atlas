import { createContext, useCallback, useContext, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchCurrentUser, login as apiLogin, logout as apiLogout } from "../api/client";
import type { CurrentUser } from "../api/types";

/**
 * Frontend authentication state.
 *
 * This is a UX convenience, not the security boundary — the backend enforces
 * authorization independently on every request regardless of what this
 * context believes. Its job is narrower: know whether a user is logged in,
 * so routes can redirect and actions can hide, without a page ever needing
 * to guess from the presence of a cookie it cannot read (the session cookie
 * is HttpOnly by design — see internal/api/session).
 */

const queryKey = ["auth", "me"] as const;

interface AuthContextValue {
  /** null means not authenticated. Undefined-vs-null is collapsed on purpose: a caller never needs to tell "still loading" apart from "anonymous" once isLoading is false. */
  user: CurrentUser | null;
  /** True only for the initial load — the one moment the app cannot yet say whether a session exists. */
  isLoading: boolean;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();

  // A 401 here means "not logged in", which is an ordinary outcome for this
  // one query, not a failure to retry or surface — react-query's default
  // retry: false (set globally in main.tsx) already stops it from retrying.
  const { data, isLoading } = useQuery<CurrentUser | null>({
    queryKey,
    queryFn: ({ signal }) => fetchCurrentUser(signal),
    retry: false,
  });

  const login = useCallback(
    async (username: string, password: string) => {
      const authenticated = await apiLogin(username, password);
      queryClient.setQueryData(queryKey, authenticated);
    },
    [queryClient],
  );

  const logout = useCallback(async () => {
    await apiLogout();
    queryClient.setQueryData(queryKey, null);
  }, [queryClient]);

  return (
    <AuthContext.Provider value={{ user: data ?? null, isLoading, login, logout }}>
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error("useAuth must be used within an AuthProvider");
  }
  return ctx;
}
