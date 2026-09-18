import type { DocHandle, SyncClient, SyncStatus } from "@kb/sync-client";
import type { LoroDoc } from "loro-crdt";
import { useCallback, useEffect, useState, useSyncExternalStore } from "react";
import { auth, type AuthState } from "../lib/auth";

export function useAuth(): AuthState {
  return useSyncExternalStore(auth.subscribe, auth.state, auth.state);
}

/** A counter that changes on every commit of the doc (local or remote). */
export function useDocVersion(doc: LoroDoc | null): number {
  const [v, setV] = useState(0);
  useEffect(() => {
    if (!doc) return;
    const unsub = doc.subscribe(() => setV((x) => x + 1));
    return () => {
      unsub();
    };
  }, [doc]);
  return v;
}

export function useSyncStatus(client: SyncClient | null): SyncStatus {
  const subscribe = useCallback((cb: () => void) => (client ? client.on("status", cb) : () => {}), [client]);
  const snapshot = useCallback((): SyncStatus => client?.status ?? "idle", [client]);
  return useSyncExternalStore(subscribe, snapshot, snapshot);
}

/** Re-renders when the handle's sync state (lastSeq, pending, synced) changes. */
export function useHandleSync(handle: DocHandle | null): number {
  const [v, setV] = useState(0);
  useEffect(() => {
    if (!handle) return;
    return handle.onSync(() => setV((x) => x + 1));
  }, [handle]);
  return v;
}

export function useLocalStorage(key: string): [string | null, (v: string | null) => void] {
  const [value, setValue] = useState<string | null>(() => {
    try {
      return localStorage.getItem(key);
    } catch {
      return null;
    }
  });
  const set = (v: string | null): void => {
    setValue(v);
    try {
      if (v === null) localStorage.removeItem(key);
      else localStorage.setItem(key, v);
    } catch {
      // ignore
    }
  };
  return [value, set];
}
