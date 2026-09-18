import { defineConfig } from "@playwright/test";

// End-to-end tests drive the real Go binary (bin/kb, built with the embedded
// web app) against the local test Postgres. Set KB_E2E_BASE_URL to point the
// tests at a server you started yourself; otherwise scripts/e2e-server.mjs
// creates a fresh database and starts ../bin/kb on KB_E2E_PORT.
const PORT = Number(process.env.KB_E2E_PORT ?? 8787);
const external = process.env.KB_E2E_BASE_URL;

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  workers: 1,
  timeout: 120_000,
  expect: { timeout: 15_000 },
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    baseURL: external ?? `http://127.0.0.1:${PORT}`,
    trace: "retain-on-failure",
  },
  webServer: external
    ? undefined
    : {
        command: `node scripts/e2e-server.mjs`,
        url: `http://127.0.0.1:${PORT}/healthz`,
        reuseExistingServer: false,
        timeout: 120_000,
        stdout: "pipe",
        stderr: "pipe",
        env: { KB_E2E_PORT: String(PORT) },
      },
  projects: [{ name: "chromium", use: { browserName: "chromium" } }],
});
