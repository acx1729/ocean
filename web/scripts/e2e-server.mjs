// Starts ../bin/kb for the end-to-end tests: a fresh database on the local
// test Postgres cluster, a throwaway data directory, dev mode on loopback.
//
//   KB_E2E_PORT       port to listen on (default 8787)
//   KB_E2E_ADMIN_DSN  superuser DSN used to create the database
//   KB_E2E_BIN        path to the kb binary (default ../bin/kb)
import { spawn, execFileSync } from "node:child_process";
import { existsSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const port = process.env.KB_E2E_PORT ?? "8787";
const admin = process.env.KB_E2E_ADMIN_DSN ?? "postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable";
const bin = process.env.KB_E2E_BIN ?? fileURLToPath(new URL("../../bin/kb", import.meta.url));

if (!existsSync(bin)) {
  console.error(`e2e: ${bin} not found. Build it first: (cd web && npm run build) && make build`);
  process.exit(1);
}

const dbName = `kb_e2e_${Date.now().toString(36)}`;
const psql =
  process.env.KB_E2E_PSQL ??
  (existsSync("/usr/lib/postgresql/16/bin/psql") ? "/usr/lib/postgresql/16/bin/psql" : "psql");
execFileSync(psql, [admin, "-v", "ON_ERROR_STOP=1", "-q", "-c", `CREATE DATABASE ${dbName}`], {
  stdio: "inherit",
});
const dsn = admin.replace(/\/postgres(\?|$)/, `/${dbName}$1`);
const dataDir = mkdtempSync(join(tmpdir(), "kb-e2e-"));

const env = {
  ...process.env,
  KB_ROLE: "all",
  KB_LISTEN: `127.0.0.1:${port}`,
  KB_PUBLIC_URL: `http://127.0.0.1:${port}`,
  KB_DEV: "true",
  KB_MIGRATE: "true",
  KB_DATABASE_URL: dsn,
  KB_FGA_DATABASE_URL: dsn,
  KB_RIVER_DATABASE_URL: dsn,
  KB_NATS_URL: process.env.KB_NATS_URL ?? "nats://127.0.0.1:4222",
  KB_DATA_DIR: dataDir,
  KB_OBJECT_STORE_DIR: join(dataDir, "objects"),
  KB_NODE_KEY_PASSPHRASE: "e2e-only-passphrase-not-for-production-0123456789",
  KB_LOG_LEVEL: process.env.KB_LOG_LEVEL ?? "warn",
  KB_LOG_FORMAT: "text",
  // Every browser context in the suite signs in from 127.0.0.1; the per-IP
  // limits that protect a real node would otherwise trip within a run.
  KB_LIMITS_RATE_CHALLENGE_PER_MINUTE: process.env.KB_LIMITS_RATE_CHALLENGE_PER_MINUTE ?? "10000",
  KB_LIMITS_RATE_ACCESS_PER_MINUTE: process.env.KB_LIMITS_RATE_ACCESS_PER_MINUTE ?? "100000",
};

console.log(`e2e: starting ${bin} on http://127.0.0.1:${port} with database ${dbName}`);
const child = spawn(bin, [], { env, stdio: "inherit" });
const stop = () => {
  if (!child.killed) child.kill("SIGTERM");
};
process.on("SIGINT", stop);
process.on("SIGTERM", stop);
child.on("exit", (code, signal) => {
  try {
    execFileSync(psql, [admin, "-q", "-c", `DROP DATABASE IF EXISTS ${dbName} WITH (FORCE)`], {
      stdio: "ignore",
    });
  } catch {
    // best effort
  }
  process.exit(code ?? (signal ? 1 : 0));
});
