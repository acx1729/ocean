// Removes the previous web build from internal/web/dist while keeping the
// directory and its .gitkeep (the Go embed pattern needs the directory).
import { existsSync, readdirSync, rmSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const dist = fileURLToPath(new URL("../../internal/web/dist", import.meta.url));
if (existsSync(dist)) {
  for (const entry of readdirSync(dist)) {
    if (entry === ".gitkeep") continue;
    rmSync(join(dist, entry), { recursive: true, force: true });
  }
}
