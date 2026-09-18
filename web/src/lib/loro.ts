// The "web" build of loro-crdt initialises its WebAssembly asynchronously. The
// app awaits ensureLoro() once before rendering anything that constructs docs.
// The glue module is imported by file so that its default export (the init
// function) is typed; "loro-crdt" itself is aliased to the same build in
// vite.config.ts, so there is exactly one WASM instance.

import init from "loro-crdt/web/loro_wasm.js";
import wasmUrl from "loro-crdt/web/loro_wasm_bg.wasm?url";

let ready: Promise<void> | null = null;

export function ensureLoro(): Promise<void> {
  const p = ready ?? (ready = init({ module_or_path: wasmUrl }).then(() => undefined));
  return p;
}
