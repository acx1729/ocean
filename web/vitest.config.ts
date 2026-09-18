import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// Unit tests run in Node (loro-crdt resolves to its nodejs build there) and
// never touch the browser aliases of vite.config.ts.
export default defineConfig({
  resolve: {
    alias: [
      {
        find: "@kb/sync-client",
        replacement: fileURLToPath(new URL("./src/lib/sync-client/index.ts", import.meta.url)),
      },
      { find: "@", replacement: fileURLToPath(new URL("./src", import.meta.url)) },
    ],
  },
  test: {
    include: ["src/**/*.test.ts"],
    environment: "node",
    testTimeout: 20000,
  },
});
