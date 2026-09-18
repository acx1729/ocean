import type { EphemeralStore, LoroDoc, Value } from "loro-crdt";

export type SyncStatus = "idle" | "connecting" | "connected" | "offline" | "closed";

/** Cursor of a peer inside one block's Markdown source: encoded Loro cursors (stable across edits). */
export type PresenceCursor = {
  blockId: string;
  anchor: Uint8Array;
  head: Uint8Array;
};

/** State every peer publishes through awareness frames (never persisted). */
export type PresenceState = {
  name: string;
  color: string;
  cursor: PresenceCursor | null;
  [key: string]: Value;
};

export interface SyncErrorEvent {
  docId?: string;
  code: string;
  message: string;
}

export interface SyncClosedEvent {
  docId: string;
  reason: string;
}

export interface WelcomeEvent {
  principal: string;
  serverVersion: string;
}

export type SyncClientEvents = {
  status: (status: SyncStatus) => void;
  error: (err: SyncErrorEvent) => void;
  closed: (ev: SyncClosedEvent) => void;
  welcome: (ev: WelcomeEvent) => void;
};

/** Why the client asks for a token: the first hello, or a retry after `unauthenticated`. */
export type TokenReason = "connect" | "unauthenticated";

export interface ConnectOptions {
  /** Absolute WebSocket URL, e.g. wss://kb.example.com/ws/sync. */
  url: string;
  workspaceId: string;
  /** Returns a fresh access token; called right before every hello frame. */
  getAccessToken: (reason: TokenReason) => string | Promise<string>;
  /** Per-tab session client id (UUIDv7); generated when omitted. */
  clientId?: string;
}

// A structural subset of the DOM WebSocket so tests and other runtimes can
// supply their own implementation.
export interface WebSocketLike {
  binaryType: string;
  readonly readyState: number;
  onopen: ((ev: unknown) => void) | null;
  onmessage: ((ev: { data: ArrayBuffer | Uint8Array }) => void) | null;
  onclose: ((ev: { code?: number; reason?: string }) => void) | null;
  onerror: ((ev: unknown) => void) | null;
  send(data: Uint8Array): void;
  close(code?: number, reason?: string): void;
}

export type WebSocketConstructor = new (url: string) => WebSocketLike;

export interface SyncClientOptions {
  persistence?: PersistenceAdapter;
  WebSocket?: WebSocketConstructor;
  /** Local updates are coalesced for this long before a push (default 50 ms). */
  coalesceMs?: number;
  /** Awareness sends are throttled to one per interval (default 100 ms). */
  awarenessThrottleMs?: number;
  /** Awareness heartbeat interval (default 10 s). */
  awarenessHeartbeatMs?: number;
  /** Awareness TTL for remote peers (default 30 s). */
  awarenessTimeoutMs?: number;
  /** Write a snapshot to the cache after this many local commits (default 500). */
  snapshotEvery?: number;
  /** "snapshot" (default, complete history) or "shallow-snapshot" for the cache. */
  snapshotMode?: "snapshot" | "shallow-snapshot";
  backoff?: { minMs: number; maxMs: number };
  /** How long a seq gap may stay open before a pull is sent (default 200 ms). */
  gapGraceMs?: number;
  /** Open docs per connection (default 20, the server limit). */
  maxDocs?: number;
  /** Runs after a doc is created and after every snapshot import (default: enable the blocks tree's fractional index). */
  configureDoc?: (doc: LoroDoc) => void;
  log?: (message: string, ...args: unknown[]) => void;
}

export interface PendingUpdate {
  clientSeq: number;
  update: Uint8Array;
}

/** What the cache knows about a doc besides its bytes. */
export interface DocMeta {
  /** Highest contiguous server seq covered by snapshot + tail. */
  lastSeq: number;
  /** client_id that owns the pending queue's client_seq space. */
  clientId: string;
  /** Last assigned client_seq. */
  clientSeq: number;
}

export interface StoredDoc {
  meta: DocMeta;
  snapshot: Uint8Array | null;
  /** Updates (local and remote) applied after the snapshot, in order. */
  tail: Uint8Array[];
  /** Local updates not yet acknowledged, in client_seq order. */
  pending: PendingUpdate[];
}

/**
 * Storage for offline docs. Every method that writes also stores the given
 * meta, so a cache is consistent after any single completed call. Calls for one
 * doc are issued sequentially by the client.
 */
export interface PersistenceAdapter {
  load(docId: string): Promise<StoredDoc | null>;
  /** Append a local update to the tail and the pending queue in one step. */
  appendLocal(docId: string, entry: PendingUpdate, meta: DocMeta): Promise<void>;
  /** Append a remote (or re-appended pending) update to the tail. */
  appendTail(docId: string, update: Uint8Array, meta: DocMeta): Promise<void>;
  /** Remove acknowledged entries from the pending queue. */
  ackPending(docId: string, clientSeqs: number[], meta: DocMeta): Promise<void>;
  /** Replace the snapshot and clear the tail (the pending queue is untouched). */
  writeSnapshot(docId: string, snapshot: Uint8Array, meta: DocMeta): Promise<void>;
  remove(docId: string): Promise<void>;
  clear(): Promise<void>;
}

export interface DocAwareness {
  readonly store: EphemeralStore<Record<string, PresenceState>>;
  /** Our key in the store (the client id). */
  readonly peerId: string;
  setLocal(state: PresenceState): void;
  getLocal(): PresenceState | undefined;
  /** Remote peers' states keyed by peer id. */
  peers(): Map<string, PresenceState>;
  subscribe(listener: (peers: Map<string, PresenceState>) => void): () => void;
}

export interface DocHandle {
  readonly docId: string;
  readonly doc: LoroDoc;
  readonly awareness: DocAwareness;
  /** Highest contiguous server seq imported. */
  readonly lastSeq: number;
  /** Local updates waiting for an ack. */
  readonly pendingCount: number;
  /** Opened on the current connection and nothing pending. */
  readonly synced: boolean;
  /** Fires whenever lastSeq, pendingCount or synced changes. */
  onSync(listener: () => void): () => void;
  close(): Promise<void>;
}
