// Presence through Loro's ephemeral store: each peer publishes one entry
// keyed by its client id; entries expire after the timeout. The client sends
// our entry's bytes over the wire and applies remote peers' bytes here.

import { EphemeralStore } from "loro-crdt";
import type { DocAwareness, PresenceState } from "./types";

type Store = EphemeralStore<Record<string, PresenceState>>;

export class Awareness implements DocAwareness {
  readonly store: Store;
  private listeners = new Set<(peers: Map<string, PresenceState>) => void>();

  constructor(
    readonly peerId: string,
    timeoutMs: number,
  ) {
    this.store = new EphemeralStore(timeoutMs) as Store;
    this.store.subscribe(() => this.notify());
  }

  setLocal(state: PresenceState): void {
    this.store.set(this.peerId, state);
  }

  getLocal(): PresenceState | undefined {
    return this.store.get(this.peerId);
  }

  /** Bytes of our own entry for the wire (empty when we have none). */
  encodeLocal(): Uint8Array {
    if (!this.store.keys().includes(this.peerId)) return new Uint8Array();
    return this.store.encode(this.peerId);
  }

  applyRemote(bytes: Uint8Array): void {
    if (bytes.length === 0) return;
    this.store.apply(bytes);
  }

  peers(): Map<string, PresenceState> {
    const out = new Map<string, PresenceState>();
    const all = this.store.getAllStates() as Record<string, PresenceState | undefined>;
    for (const [k, v] of Object.entries(all)) {
      if (k !== this.peerId && v) out.set(k, v);
    }
    return out;
  }

  subscribe(listener: (peers: Map<string, PresenceState>) => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  /** Called by the client when it learns a peer left. */
  drop(peer: string): void {
    if (this.store.keys().includes(peer)) this.store.delete(peer);
  }

  destroy(): void {
    this.listeners.clear();
    this.store.destroy();
  }

  private notify(): void {
    const peers = this.peers();
    for (const l of this.listeners) l(peers);
  }
}
