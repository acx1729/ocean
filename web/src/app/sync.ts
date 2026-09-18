// One SyncClient per workspace for the tab. The client survives route changes
// inside the workspace; switching workspaces tears it down.

import { IndexedDBPersistence, SyncClient, uuidv7 } from "@kb/sync-client";
import { auth } from "../lib/auth";
import { config } from "../lib/config";

let current: { workspaceId: string; client: SyncClient } | null = null;

/** Stable per-tab client id (kept across reloads so re-sent updates dedupe). */
export function tabClientId(): string {
  try {
    const existing = sessionStorage.getItem("kb.clientId");
    if (existing) return existing;
    const id = uuidv7();
    sessionStorage.setItem("kb.clientId", id);
    return id;
  } catch {
    return uuidv7();
  }
}

export function syncClientFor(workspaceId: string): SyncClient {
  if (current && current.workspaceId === workspaceId) return current.client;
  current?.client.destroy();
  const client = new SyncClient({
    persistence: new IndexedDBPersistence(),
    log: (msg, ...args) => console.info(msg, ...args),
  });
  client.on("error", (e) => {
    if (e.code !== "unauthenticated" && e.code !== "token") console.warn("sync error", e);
  });
  client.connect({
    url: config.syncUrl,
    workspaceId,
    getAccessToken: (r) => auth.accessToken(r),
    clientId: tabClientId(),
  });
  current = { workspaceId, client };
  return client;
}

export function stopSync(): void {
  current?.client.destroy();
  current = null;
}

// Test hook (dev builds only): lets end-to-end tests cut and restore the
// connection, since browser offline emulation leaves open sockets alone.
if (config.dev) {
  (globalThis as unknown as { __kbSync?: unknown }).__kbSync = {
    disconnect: () => current?.client.disconnect(),
    reconnect: () => {
      if (!current) return;
      current.client.connect({
        url: config.syncUrl,
        workspaceId: current.workspaceId,
        getAccessToken: (r) => auth.accessToken(r),
        clientId: tabClientId(),
      });
    },
    status: () => current?.client.status ?? "none",
    docs: () => current?.client.inspect() ?? [],
  };
}
