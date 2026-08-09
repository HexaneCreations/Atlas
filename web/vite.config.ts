import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath, URL } from "node:url";

/**
 * Vite configuration for the Atlas frontend.
 *
 * In development the dev server proxies API traffic to a locally running
 * atlas-server. That keeps the browser on a single origin, so the frontend
 * exercises the same same-origin path it uses in production and never needs
 * CORS to be relaxed just to make development work — a relaxation that has a
 * habit of surviving into deployment.
 */
const apiTarget = process.env.ATLAS_API ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react(), tailwindcss()],

  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },

  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      // ATLAS_API overrides the proxy target so the frontend can be developed
      // against a host whose plugins this machine cannot run. The Services
      // page needs systemd, which macOS does not have, so it is built against
      // a Linux container publishing Atlas on 8081:
      //
      //   ATLAS_API=http://127.0.0.1:8081 npm run dev
      //
      // Everything else defaults to the local server as before.
      // ws: true so the dev proxy forwards the log-follow WebSocket upgrade
      // rather than only plain HTTP; without it the connection never leaves
      // "connecting" in development.
      "/api": { target: apiTarget, changeOrigin: false, ws: true },
      "/healthz": { target: apiTarget, changeOrigin: false },
      "/readyz": { target: apiTarget, changeOrigin: false },
    },
  },

  // Vitest shares this config, so tests resolve the same aliases as the app.
  // The suite targets the pure model modules — the domain logic that decides
  // what a page claims — rather than rendering components. Those functions
  // have produced every correctness bug worth catching so far.
  test: {
    environment: "node",
    include: ["src/**/*.test.ts"],
  },

  build: {
    outDir: "dist",
    // Atlas is an operations tool used during incidents, when the person
    // reading it is usually on a bad connection into a bastion host. Source
    // maps and a real chunk budget matter more here than usual.
    sourcemap: true,
    chunkSizeWarningLimit: 600,
  },
});
