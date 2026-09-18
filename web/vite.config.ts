import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The mock server (web/mock/server.ts) listens here during development; the dev
// server proxies the API and the sync WebSocket to it so the app can use
// same-origin URLs exactly as it does behind the Go binary.
const MOCK = "http://127.0.0.1:8787";

export default defineConfig({
  // Served by internal/web at /app/.
  base: "/app/",
  plugins: [react()],
  resolve: {
    alias: [
      // The "web" build of loro-crdt initialises its WASM asynchronously (see
      // src/editor/loro.ts); the default browser build blocks on a synchronous XHR.
      { find: /^loro-crdt$/, replacement: "loro-crdt/web" },
      {
        find: "@kb/sync-client",
        replacement: fileURLToPath(new URL("./src/lib/sync-client/index.ts", import.meta.url)),
      },
      { find: "@", replacement: fileURLToPath(new URL("./src", import.meta.url)) },
    ],
  },
  build: {
    outDir: "../internal/web/dist",
    // scripts/clean-dist.mjs removes the previous build; the directory itself
    // (and its .gitkeep) must survive so that the Go embed pattern always matches.
    emptyOutDir: false,
    manifest: true,
    sourcemap: false,
    target: "es2022",
    chunkSizeWarningLimit: 1200,
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (id.includes("node_modules/loro-crdt")) return "loro";
          if (
            id.includes("node_modules/@codemirror") ||
            id.includes("node_modules/@lezer") ||
            id.includes("node_modules/loro-codemirror")
          )
            return "codemirror";
          if (
            id.includes("node_modules/viem") ||
            id.includes("node_modules/ox") ||
            id.includes("node_modules/@noble") ||
            id.includes("node_modules/abitype")
          )
            return "wallet";
          return undefined;
        },
      },
    },
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      "/rpc": { target: MOCK, changeOrigin: false },
      "/ws": { target: MOCK.replace("http", "ws"), ws: true },
    },
  },
  preview: {
    port: 4173,
    proxy: {
      "/rpc": { target: MOCK },
      "/ws": { target: MOCK.replace("http", "ws"), ws: true },
    },
  },
});
