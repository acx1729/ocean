import React from "react";
import { createRoot } from "react-dom/client";
import { App } from "./app/App";
import { ensureLoro } from "./lib/loro";
import "./styles.css";

async function boot(): Promise<void> {
  const root = document.getElementById("root");
  if (!root) throw new Error("missing #root");
  try {
    await ensureLoro();
  } catch (err) {
    root.textContent = `The editor engine failed to load: ${err instanceof Error ? err.message : String(err)}`;
    return;
  }
  createRoot(root).render(
    <React.StrictMode>
      <App />
    </React.StrictMode>,
  );
}

void boot();
