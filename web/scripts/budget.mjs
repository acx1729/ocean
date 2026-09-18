// Size budgets for the production build (gzip). Run after `npm run build`.
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { gzipSync } from "node:zlib";

const assets = fileURLToPath(new URL("../../internal/web/dist/assets", import.meta.url));
const budgets = {
  // Scripts needed for the first meaningful screen: app shell + editor + engine glue.
  initialJs: 480 * 1024,
  // Loaded on demand for wallet sign-in only.
  walletJs: 120 * 1024,
  wasm: 1200 * 1024,
  css: 24 * 1024,
};

let files;
try {
  files = readdirSync(assets);
} catch {
  console.error(`budget: ${assets} not found; run npm run build first`);
  process.exit(1);
}

const sizes = files.map((f) => ({
  file: f,
  raw: statSync(join(assets, f)).size,
  gz: gzipSync(readFileSync(join(assets, f))).length,
}));
const sum = (pred) => sizes.filter(pred).reduce((n, s) => n + s.gz, 0);
const totals = {
  initialJs: sum((s) => s.file.endsWith(".js") && !s.file.startsWith("wallet-")),
  walletJs: sum((s) => s.file.endsWith(".js") && s.file.startsWith("wallet-")),
  wasm: sum((s) => s.file.endsWith(".wasm")),
  css: sum((s) => s.file.endsWith(".css")),
};

const kb = (n) => `${(n / 1024).toFixed(1)} KB`;
for (const s of sizes)
  console.log(`${s.file.padEnd(40)} ${kb(s.raw).padStart(11)}  gzip ${kb(s.gz).padStart(10)}`);
let failed = false;
for (const [k, limit] of Object.entries(budgets)) {
  const ok = totals[k] <= limit;
  if (!ok) failed = true;
  console.log(`${ok ? "ok  " : "OVER"} ${k.padEnd(10)} ${kb(totals[k]).padStart(10)} / ${kb(limit)}`);
}
process.exit(failed ? 1 : 0);
