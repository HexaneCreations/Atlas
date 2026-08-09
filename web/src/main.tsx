import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router";
import { App } from "./App";
import { ToastProvider } from "./components/Toast";
import "./styles.css";

/**
 * Application entry point.
 *
 * The QueryClient is created once at module scope. Creating it inside a
 * component would discard the entire cache on every re-render of that
 * component, which turns a dashboard into a request storm against the thing
 * it is monitoring.
 */
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Atlas data is inherently live; refetching when the operator returns
      // to the tab is the behaviour they expect from a monitoring view.
      refetchOnWindowFocus: true,
      // Retry policy is set per query, since it depends on the error code.
      retry: false,
    },
  },
});

const container = document.getElementById("root");
if (!container) {
  throw new Error("index.html is missing the #root element");
}

createRoot(container).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <ToastProvider>
          <App />
        </ToastProvider>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
