// SyncClient: one WebSocket per tab multiplexing Loro docs against /ws/sync.
//
// Per doc it keeps a LoroDoc, the highest contiguous server seq it has imported
// (lastSeq), a queue of local updates that still await an ack, and an
// awareness store for presence. Local commits are coalesced for a short window
// and exported as one update per push; pushes are acknowledged with server
// seqs; remote updates arrive as update frames, and any seq gap that stays open
// for longer than the grace period triggers a pull. Everything the doc knows is
// mirrored into the persistence adapter so a tab can reopen its docs offline
// and resume from lastSeq with a tail instead of a snapshot.

import { LoroDoc } from "loro-crdt";
import type {
  Awareness as AwarenessFrame,
  Close,
  OpenResponse,
  PullResponse,
  PushAck,
  SyncError,
  SyncFrame,
  Update,
  Welcome,
} from "../../gen/kb/v1/sync_pb";
import { Awareness } from "./awareness";
import { backoffDelay } from "./backoff";
import { Emitter } from "./emitter";
import { decodeFrame, frames, seq as toNum } from "./frames";
import { MemoryPersistence } from "./persistence";
import type {
  ConnectOptions,
  DocHandle,
  DocMeta,
  PendingUpdate,
  PersistenceAdapter,
  SyncClientEvents,
  SyncClientOptions,
  SyncStatus,
  TokenReason,
  WebSocketConstructor,
  WebSocketLike,
} from "./types";
import { uuidv7 } from "./uuidv7";

type Timer = ReturnType<typeof setTimeout>;

const WS_OPEN = 1;
const MAX_UPDATES_PER_PUSH = 50;
const PULL_PAGE = 5000; // the server's page size for pull
const CLOSE_DRAIN_MS = 3000;

interface DocState {
  readonly docId: string;
  readonly doc: LoroDoc;
  readonly awareness: Awareness;
  handle: DocHandle;
  refs: number;
  closing: boolean;
  /** Highest contiguous server seq imported (or acknowledged for our own pushes). */
  lastSeq: number;
  /** Seqs above lastSeq that we already have, waiting for the gap to close. */
  known: Set<number>;
  /** Last assigned client_seq. */
  clientSeq: number;
  pending: PendingUpdate[];
  inflight: Set<number>;
  /** Opened on the current connection. */
  opened: boolean;
  /** The server closed the doc (trash, access revoked, not found). */
  closedReason: string | null;
  /** Our peer's op counter up to which local ops have been queued. */
  flushedCounter: number;
  /** A local commit is waiting for the coalescing window to close. */
  dirty: boolean;
  coalesceTimer: Timer | null;
  gapTimer: Timer | null;
  pullInflight: boolean;
  pullNoProgress: number;
  retryTimer: Timer | null;
  awarenessTimer: Timer | null;
  lastAwarenessSend: number;
  /** Updates appended to the cache since its snapshot. */
  tailCount: number;
  persistChain: Promise<void>;
  unsubscribe: Array<() => void>;
  syncListeners: Set<() => void>;
}

export interface DocInfo {
  docId: string;
  opened: boolean;
  lastSeq: number;
  pending: number;
  inflight: number;
  gaps: number[];
  dirty: boolean;
  closedReason: string | null;
  refs: number;
}

interface ResolvedOptions {
  coalesceMs: number;
  awarenessThrottleMs: number;
  awarenessHeartbeatMs: number;
  awarenessTimeoutMs: number;
  snapshotEvery: number;
  snapshotMode: "snapshot" | "shallow-snapshot";
  backoff: { minMs: number; maxMs: number };
  gapGraceMs: number;
  maxDocs: number;
  configureDoc: (doc: LoroDoc) => void;
  log: (message: string, ...args: unknown[]) => void;
}

/** Default doc configuration: the blocks tree uses jitter-free fractional indexes, like the server. */
export function configureBlocksDoc(doc: LoroDoc): void {
  doc.getTree("blocks").enableFractionalIndex(0);
}

function resolveWebSocket(given?: WebSocketConstructor): WebSocketConstructor {
  if (given) return given;
  const g = globalThis as unknown as { WebSocket?: WebSocketConstructor };
  if (!g.WebSocket) throw new Error("sync-client: no WebSocket implementation available");
  return g.WebSocket;
}

function toBytes(data: ArrayBuffer | Uint8Array): Uint8Array {
  return data instanceof Uint8Array ? data : new Uint8Array(data);
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export class SyncClient extends Emitter<SyncClientEvents> {
  private readonly opts: ResolvedOptions;
  private readonly persistence: PersistenceAdapter;
  private readonly WS: WebSocketConstructor;
  private _clientId: string;
  private conn: ConnectOptions | null = null;
  private ws: WebSocketLike | null = null;
  private wanted = false;
  private welcomed = false;
  private attempt = 0;
  private generation = 0;
  private reconnectTimer: Timer | null = null;
  private heartbeat: Timer | null = null;
  private tokenReason: TokenReason = "connect";
  private _status: SyncStatus = "idle";
  private readonly docs = new Map<string, DocState>();
  private readonly loading = new Map<string, Promise<DocState>>();
  private readonly onOnline = (): void => {
    if (this.wanted && !this.ws) {
      this.clearReconnect();
      this.attempt = 0;
      void this.dial();
    }
  };

  constructor(options: SyncClientOptions = {}) {
    super();
    this.opts = {
      coalesceMs: options.coalesceMs ?? 50,
      awarenessThrottleMs: options.awarenessThrottleMs ?? 100,
      awarenessHeartbeatMs: options.awarenessHeartbeatMs ?? 10_000,
      awarenessTimeoutMs: options.awarenessTimeoutMs ?? 30_000,
      snapshotEvery: options.snapshotEvery ?? 500,
      snapshotMode: options.snapshotMode ?? "snapshot",
      backoff: options.backoff ?? { minMs: 500, maxMs: 30_000 },
      gapGraceMs: options.gapGraceMs ?? 200,
      maxDocs: options.maxDocs ?? 20,
      configureDoc: options.configureDoc ?? configureBlocksDoc,
      log: options.log ?? (() => {}),
    };
    this.persistence = options.persistence ?? new MemoryPersistence();
    this.WS = resolveWebSocket(options.WebSocket);
    this._clientId = uuidv7();
  }

  /** Per-tab client id used for hello and push frames. */
  get clientId(): string {
    return this._clientId;
  }

  get status(): SyncStatus {
    return this._status;
  }

  get connected(): boolean {
    return this.welcomed;
  }

  /** Ids of the docs currently held open. */
  openDocIds(): string[] {
    return [...this.docs.keys()];
  }

  /** A snapshot of every open doc's sync state, for diagnostics. */
  inspect(): DocInfo[] {
    return [...this.docs.values()].map((d) => ({
      docId: d.docId,
      opened: d.opened,
      lastSeq: d.lastSeq,
      pending: d.pending.length,
      inflight: d.inflight.size,
      gaps: [...d.known].sort((a, b) => a - b),
      dirty: d.dirty,
      closedReason: d.closedReason,
      refs: d.refs,
    }));
  }

  // --- connection -----------------------------------------------------------

  connect(opts: ConnectOptions): void {
    this.teardownSocket();
    this.conn = opts;
    if (opts.clientId) this._clientId = opts.clientId;
    this.wanted = true;
    this.attempt = 0;
    this.tokenReason = "connect";
    this.clearReconnect();
    const g = globalThis as unknown as { addEventListener?: (t: string, l: () => void) => void };
    g.addEventListener?.("online", this.onOnline);
    g.addEventListener?.("pagehide", this.onPageHide);
    void this.dial();
  }

  disconnect(): void {
    this.wanted = false;
    this.clearReconnect();
    const g = globalThis as unknown as { removeEventListener?: (t: string, l: () => void) => void };
    g.removeEventListener?.("online", this.onOnline);
    g.removeEventListener?.("pagehide", this.onPageHide);
    this.flushAll();
    this.teardownSocket();
    this.setStatus("closed");
  }

  /** Reconnect immediately, e.g. after the app refreshed its session. */
  reconnectNow(): void {
    if (!this.wanted) return;
    this.clearReconnect();
    this.attempt = 0;
    this.teardownSocket();
    void this.dial();
  }

  /** Disconnects and releases every doc; the client cannot be reused. */
  destroy(): void {
    this.disconnect();
    for (const d of [...this.docs.values()]) this.destroyDoc(d, false);
    this.clear();
  }

  private async dial(): Promise<void> {
    if (!this.wanted || !this.conn || this.ws) return;
    const gen = ++this.generation;
    this.setStatus("connecting");
    let token: string;
    try {
      token = await this.conn.getAccessToken(this.tokenReason);
    } catch (err) {
      if (gen !== this.generation || !this.wanted) return;
      this.emit("error", { code: "token", message: errorMessage(err) });
      this.setStatus("offline");
      this.scheduleReconnect();
      return;
    }
    if (gen !== this.generation || !this.wanted || this.ws) return;
    let ws: WebSocketLike;
    try {
      ws = new this.WS(this.conn.url);
    } catch (err) {
      this.emit("error", { code: "connect", message: errorMessage(err) });
      this.setStatus("offline");
      this.scheduleReconnect();
      return;
    }
    ws.binaryType = "arraybuffer";
    this.ws = ws;
    this.welcomed = false;
    const workspaceId = this.conn.workspaceId;
    ws.onopen = () => {
      if (this.ws !== ws) return;
      this.send(frames.hello(token, this._clientId, workspaceId));
    };
    ws.onmessage = (ev) => {
      if (this.ws !== ws) return;
      this.onMessage(toBytes(ev.data));
    };
    ws.onerror = () => {
      // The close event that follows carries the state change.
    };
    ws.onclose = (ev) => {
      if (this.ws !== ws) return;
      this.onSocketClosed(ev?.reason ?? "");
    };
  }

  private teardownSocket(): void {
    const ws = this.ws;
    this.ws = null;
    this.generation++;
    this.welcomed = false;
    this.stopHeartbeat();
    for (const d of this.docs.values()) this.markDisconnected(d);
    if (ws) {
      ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
      try {
        ws.close(1000, "bye");
      } catch {
        // already closed
      }
    }
  }

  private onSocketClosed(reason: string): void {
    this.ws = null;
    this.welcomed = false;
    this.stopHeartbeat();
    for (const d of this.docs.values()) this.markDisconnected(d);
    if (!this.wanted) {
      this.setStatus("closed");
      return;
    }
    this.opts.log("sync: socket closed", reason);
    this.setStatus("offline");
    this.scheduleReconnect();
  }

  private markDisconnected(d: DocState): void {
    const wasOpen = d.opened;
    d.opened = false;
    d.inflight.clear();
    d.pullInflight = false;
    if (d.gapTimer) {
      clearTimeout(d.gapTimer);
      d.gapTimer = null;
    }
    if (d.retryTimer) {
      clearTimeout(d.retryTimer);
      d.retryTimer = null;
    }
    if (wasOpen) this.notifySync(d);
  }

  private scheduleReconnect(): void {
    if (!this.wanted || this.reconnectTimer) return;
    const delay = backoffDelay(this.attempt++, this.opts.backoff);
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      void this.dial();
    }, delay);
  }

  private clearReconnect(): void {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  private setStatus(s: SyncStatus): void {
    if (this._status === s) return;
    this._status = s;
    this.emit("status", s);
  }

  private send(bytes: Uint8Array): boolean {
    const ws = this.ws;
    if (!ws || ws.readyState !== WS_OPEN) return false;
    try {
      ws.send(bytes);
      return true;
    } catch (err) {
      this.opts.log("sync: send failed", err);
      return false;
    }
  }

  private startHeartbeat(): void {
    this.stopHeartbeat();
    this.heartbeat = setInterval(() => {
      for (const d of this.docs.values()) {
        if (!d.opened) continue;
        // Re-setting our entry refreshes its timestamp locally and re-sends it.
        const local = d.awareness.getLocal();
        if (local) d.awareness.setLocal(local);
      }
    }, this.opts.awarenessHeartbeatMs);
  }

  private stopHeartbeat(): void {
    if (this.heartbeat) {
      clearInterval(this.heartbeat);
      this.heartbeat = null;
    }
  }

  // --- inbound frames ---------------------------------------------------------

  private onMessage(bytes: Uint8Array): void {
    let frame: SyncFrame;
    try {
      frame = decodeFrame(bytes);
    } catch (err) {
      this.emit("error", { code: "bad_frame", message: errorMessage(err) });
      return;
    }
    const kind = frame.kind;
    switch (kind.case) {
      case "welcome":
        this.onWelcome(kind.value);
        break;
      case "opened":
        this.onOpened(kind.value);
        break;
      case "ack":
        this.onAck(kind.value);
        break;
      case "update":
        this.onUpdate(kind.value);
        break;
      case "pulled":
        this.onPulled(kind.value);
        break;
      case "awareness":
        this.onAwareness(kind.value);
        break;
      case "error":
        this.onError(kind.value);
        break;
      case "close":
        this.onServerClose(kind.value);
        break;
      default:
        break;
    }
  }

  private onWelcome(w: Welcome): void {
    this.welcomed = true;
    this.attempt = 0;
    this.tokenReason = "connect";
    this.setStatus("connected");
    this.emit("welcome", { principal: w.principal, serverVersion: w.serverVersion });
    for (const d of this.docs.values()) {
      d.closedReason = null;
      this.sendOpen(d);
    }
    this.startHeartbeat();
  }

  private sendOpen(d: DocState): void {
    if (!this.welcomed || !this.conn || d.closedReason || d.closing) return;
    this.send(frames.open(d.docId, d.lastSeq, this.conn.workspaceId));
  }

  private onOpened(res: OpenResponse): void {
    const d = this.docs.get(res.docId);
    if (!d) return;
    d.opened = true;
    d.inflight.clear();
    try {
      if (res.snapshot.length > 0) {
        d.doc.import(res.snapshot);
        this.opts.configureDoc(d.doc);
        d.lastSeq = toNum(res.snapshotSeq);
        this.writeSnapshot(d);
      } else {
        const tail = res.tail;
        if (tail.length > 0) d.doc.importBatch(tail.map((u) => u.update));
        d.lastSeq = Math.max(d.lastSeq, toNum(res.currentSeq));
        for (const u of tail) this.appendTail(d, u.update);
      }
    } catch (err) {
      this.emit("error", { docId: d.docId, code: "import_failed", message: errorMessage(err) });
    }
    this.pruneKnown(d);
    this.sendPending(d);
    this.sendAwareness(d, false);
    this.notifySync(d);
  }

  private onAck(ack: PushAck): void {
    const d = this.docs.get(ack.docId);
    if (!d) return;
    const acked: number[] = [];
    for (const a of ack.acked) {
      const cs = toNum(a.clientSeq);
      acked.push(cs);
      d.inflight.delete(cs);
      this.markSeq(d, toNum(a.seq));
    }
    if (acked.length > 0) {
      d.pending = d.pending.filter((p) => !acked.includes(p.clientSeq));
      this.persist(d, (p, meta) => p.ackPending(d.docId, acked, meta));
    }
    if (d.inflight.size === 0) this.sendPending(d);
    this.notifySync(d);
  }

  private onUpdate(u: Update): void {
    const d = this.docs.get(u.docId);
    if (!d) return;
    const s = toNum(u.seq);
    if (s <= d.lastSeq || d.known.has(s)) return;
    try {
      d.doc.import(u.update);
    } catch (err) {
      this.emit("error", { docId: d.docId, code: "import_failed", message: errorMessage(err) });
    }
    this.appendTail(d, u.update);
    this.markSeq(d, s);
    this.notifySync(d);
  }

  private onPulled(res: PullResponse): void {
    const d = this.docs.get(res.docId);
    if (!d) return;
    d.pullInflight = false;
    const before = d.lastSeq;
    const fresh = res.updates.filter((u) => toNum(u.seq) > d.lastSeq && !d.known.has(toNum(u.seq)));
    try {
      if (fresh.length > 0) d.doc.importBatch(fresh.map((u) => u.update));
    } catch (err) {
      this.emit("error", { docId: d.docId, code: "import_failed", message: errorMessage(err) });
    }
    for (const u of fresh) this.appendTail(d, u.update);
    let top = d.lastSeq;
    for (const u of res.updates) top = Math.max(top, toNum(u.seq));
    const complete = res.updates.length < PULL_PAGE;
    const current = toNum(res.currentSeq);
    if (complete) top = Math.max(top, current);
    d.lastSeq = Math.max(d.lastSeq, top);
    this.pruneKnown(d);
    if (d.lastSeq === before && d.known.size > 0) {
      // No progress: seqs we hold beyond the server's current seq will never
      // be delivered (the log was restored); stop waiting for them.
      d.pullNoProgress++;
      if (d.pullNoProgress >= 3 && complete) {
        for (const k of [...d.known]) if (k > current) d.known.delete(k);
        d.pullNoProgress = 0;
      }
    } else {
      d.pullNoProgress = 0;
    }
    if (d.known.size > 0) this.armGap(d);
    this.notifySync(d);
  }

  private onAwareness(a: AwarenessFrame): void {
    const d = this.docs.get(a.docId);
    if (!d || a.peer === this._clientId) return;
    if (a.state.length === 0) {
      d.awareness.drop(a.peer);
      return;
    }
    try {
      d.awareness.applyRemote(a.state);
    } catch (err) {
      this.opts.log("sync: bad awareness frame", err);
    }
  }

  private onError(e: SyncError): void {
    this.emit("error", { docId: e.docId || undefined, code: e.code, message: e.message });
    if (!e.docId) {
      if (e.code === "unauthenticated") this.tokenReason = "unauthenticated";
      return;
    }
    const d = this.docs.get(e.docId);
    if (!d) return;
    switch (e.code) {
      case "not_found":
      case "permission_denied":
      case "deleted":
      case "too_many_docs":
        this.closeByServer(d, e.code);
        break;
      case "not_open":
        d.opened = false;
        d.inflight.clear();
        this.sendOpen(d);
        break;
      case "rate_limited":
        d.inflight.clear();
        if (!d.retryTimer) {
          d.retryTimer = setTimeout(() => {
            d.retryTimer = null;
            this.sendPending(d);
          }, 1000);
        }
        break;
      case "invalid_update":
      case "too_large": {
        // The server will never accept these; drop them so later work is not
        // stuck behind them (the error event tells the app what was lost).
        const dropped = [...d.inflight];
        d.inflight.clear();
        if (dropped.length > 0) {
          d.pending = d.pending.filter((p) => !dropped.includes(p.clientSeq));
          this.persist(d, (p, meta) => p.ackPending(d.docId, dropped, meta));
        }
        this.sendPending(d);
        this.notifySync(d);
        break;
      }
      default:
        d.inflight.clear();
        break;
    }
  }

  private onServerClose(c: Close): void {
    const d = this.docs.get(c.docId);
    if (!d) return;
    this.closeByServer(d, c.reason || "closed");
  }

  private closeByServer(d: DocState, reason: string): void {
    d.opened = false;
    d.inflight.clear();
    d.closedReason = reason;
    this.emit("closed", { docId: d.docId, reason });
    this.notifySync(d);
  }

  // --- seq bookkeeping ---------------------------------------------------------

  private markSeq(d: DocState, s: number): void {
    if (s <= d.lastSeq) return;
    d.known.add(s);
    this.pruneKnown(d);
    if (d.known.size > 0) this.armGap(d);
  }

  /** Advances lastSeq over contiguous known seqs and forgets anything at or below it. */
  private pruneKnown(d: DocState): void {
    while (d.known.has(d.lastSeq + 1)) {
      d.lastSeq++;
      d.known.delete(d.lastSeq);
    }
    for (const k of [...d.known]) if (k <= d.lastSeq) d.known.delete(k);
    if (d.known.size === 0 && d.gapTimer) {
      clearTimeout(d.gapTimer);
      d.gapTimer = null;
    }
  }

  private armGap(d: DocState): void {
    if (d.gapTimer || d.pullInflight) return;
    d.gapTimer = setTimeout(() => {
      d.gapTimer = null;
      if (d.known.size === 0 || d.pullInflight || !d.opened || !this.welcomed || !this.conn) return;
      d.pullInflight = true;
      this.send(frames.pull(d.docId, d.lastSeq, this.conn.workspaceId));
    }, this.opts.gapGraceMs);
  }

  // --- local changes ------------------------------------------------------------

  private scheduleFlush(d: DocState): void {
    if (!d.dirty) {
      d.dirty = true;
      this.notifySync(d);
    }
    if (d.coalesceTimer) return;
    d.coalesceTimer = setTimeout(() => {
      d.coalesceTimer = null;
      this.flushLocal(d);
    }, this.opts.coalesceMs);
  }

  /** Pushes every coalescing local change right away (used before the page unloads). */
  flushAll(): void {
    for (const d of this.docs.values()) {
      if (d.coalesceTimer) {
        clearTimeout(d.coalesceTimer);
        d.coalesceTimer = null;
      }
      if (d.dirty) this.flushLocal(d);
    }
  }

  private readonly onPageHide = (): void => {
    this.flushAll();
  };

  /** Exports the local ops committed since the last flush as one update. */
  private exportLocal(d: DocState): Uint8Array | null {
    const peer = d.doc.peerIdStr;
    const vv = d.doc.version();
    const end = vv.get(peer) ?? 0;
    if (end <= d.flushedCounter) return null;
    vv.setEnd({ peer, counter: d.flushedCounter });
    const bytes = d.doc.export({ mode: "update", from: vv });
    d.flushedCounter = end;
    return bytes;
  }

  private flushLocal(d: DocState): void {
    d.dirty = false;
    let bytes: Uint8Array | null;
    try {
      bytes = this.exportLocal(d);
    } catch (err) {
      this.emit("error", { docId: d.docId, code: "export_failed", message: errorMessage(err) });
      return;
    }
    if (!bytes || bytes.length === 0) {
      this.notifySync(d);
      return;
    }
    const entry: PendingUpdate = { clientSeq: ++d.clientSeq, update: bytes };
    d.pending.push(entry);
    d.tailCount++;
    if (d.tailCount >= this.opts.snapshotEvery) {
      this.writeSnapshot(d, entry);
    } else {
      this.persist(d, (p, meta) => p.appendLocal(d.docId, entry, meta));
    }
    this.sendPending(d);
    this.notifySync(d);
  }

  private sendPending(d: DocState): void {
    if (!d.opened || !this.welcomed || !this.conn || d.closedReason) return;
    if (d.inflight.size > 0 || d.pending.length === 0) return;
    const batch = d.pending.slice(0, MAX_UPDATES_PER_PUSH);
    for (const e of batch) d.inflight.add(e.clientSeq);
    if (!this.send(frames.push(d.docId, this._clientId, this.conn.workspaceId, batch))) d.inflight.clear();
  }

  // --- awareness ---------------------------------------------------------------

  private scheduleAwareness(d: DocState): void {
    if (d.awarenessTimer) return;
    const wait = Math.max(0, this.opts.awarenessThrottleMs - (Date.now() - d.lastAwarenessSend));
    d.awarenessTimer = setTimeout(() => {
      d.awarenessTimer = null;
      this.sendAwareness(d, true);
    }, wait);
  }

  private sendAwareness(d: DocState, allowEmpty: boolean): void {
    if (!d.opened || !this.welcomed) return;
    let bytes: Uint8Array;
    try {
      bytes = d.awareness.encodeLocal();
    } catch {
      return;
    }
    if (bytes.length === 0 && !allowEmpty) return;
    d.lastAwarenessSend = Date.now();
    this.send(frames.awareness(d.docId, bytes, this._clientId));
  }

  // --- persistence -------------------------------------------------------------

  private meta(d: DocState): DocMeta {
    return { lastSeq: d.lastSeq, clientId: this._clientId, clientSeq: d.clientSeq };
  }

  private persist(d: DocState, op: (p: PersistenceAdapter, meta: DocMeta) => Promise<void>): void {
    const meta = this.meta(d);
    d.persistChain = d.persistChain
      .then(() => op(this.persistence, meta))
      .catch((err: unknown) => this.opts.log("sync: persistence failed", d.docId, err));
  }

  private appendTail(d: DocState, update: Uint8Array): void {
    d.tailCount++;
    if (d.tailCount >= this.opts.snapshotEvery) {
      this.writeSnapshot(d);
    } else {
      this.persist(d, (p, meta) => p.appendTail(d.docId, update, meta));
    }
  }

  /** Replaces the cached snapshot with the doc's current state (and records a pending entry made at the same time). */
  private writeSnapshot(d: DocState, alsoPending?: PendingUpdate): void {
    let snapshot: Uint8Array;
    try {
      snapshot =
        this.opts.snapshotMode === "shallow-snapshot"
          ? d.doc.export({ mode: "shallow-snapshot", frontiers: d.doc.oplogFrontiers() })
          : d.doc.export({ mode: "snapshot" });
    } catch (err) {
      this.opts.log("sync: snapshot export failed", d.docId, err);
      return;
    }
    d.tailCount = 0;
    this.persist(d, async (p, meta) => {
      await p.writeSnapshot(d.docId, snapshot, meta);
      if (alsoPending) await p.appendLocal(d.docId, alsoPending, meta);
    });
  }

  // --- docs --------------------------------------------------------------------

  /** Opens a doc (from the cache first, then the server); handles are shared and reference counted. */
  async openDoc(docId: string): Promise<DocHandle> {
    const existing = this.docs.get(docId);
    if (existing) {
      existing.refs++;
      existing.closing = false;
      return existing.handle;
    }
    let loading = this.loading.get(docId);
    if (!loading) {
      loading = this.createDoc(docId).finally(() => this.loading.delete(docId));
      this.loading.set(docId, loading);
    }
    const d = await loading;
    d.refs++;
    return d.handle;
  }

  private async createDoc(docId: string): Promise<DocState> {
    if (this.docs.size >= this.opts.maxDocs) {
      throw new Error(`sync-client: too many open docs (max ${this.opts.maxDocs})`);
    }
    let doc = new LoroDoc();
    this.opts.configureDoc(doc);
    let lastSeq = 0;
    let clientSeq = 0;
    let pending: PendingUpdate[] = [];
    let tailCount = 0;
    let stored = null;
    try {
      stored = await this.persistence.load(docId);
    } catch (err) {
      this.opts.log("sync: cache load failed", docId, err);
    }
    if (stored) {
      try {
        if (stored.snapshot && stored.snapshot.length > 0) doc.import(stored.snapshot);
        if (stored.tail.length > 0) doc.importBatch(stored.tail);
        if (stored.pending.length > 0) doc.importBatch(stored.pending.map((p) => p.update));
        this.opts.configureDoc(doc);
        lastSeq = stored.meta.lastSeq;
        clientSeq = stored.meta.clientSeq;
        pending = [...stored.pending];
        tailCount = stored.tail.length;
      } catch (err) {
        this.opts.log("sync: cached doc is unreadable, starting over", docId, err);
        doc = new LoroDoc();
        this.opts.configureDoc(doc);
        lastSeq = 0;
        clientSeq = 0;
        pending = [];
        tailCount = 0;
        try {
          await this.persistence.remove(docId);
        } catch {
          // best effort
        }
      }
    }
    const awareness = new Awareness(this._clientId, this.opts.awarenessTimeoutMs);
    const d: DocState = {
      docId,
      doc,
      awareness,
      handle: undefined as unknown as DocHandle,
      refs: 0,
      closing: false,
      lastSeq,
      known: new Set(),
      clientSeq,
      pending,
      inflight: new Set(),
      opened: false,
      closedReason: null,
      flushedCounter: doc.version().get(doc.peerIdStr) ?? 0,
      dirty: false,
      coalesceTimer: null,
      gapTimer: null,
      pullInflight: false,
      pullNoProgress: 0,
      retryTimer: null,
      awarenessTimer: null,
      lastAwarenessSend: 0,
      tailCount,
      persistChain: Promise.resolve(),
      unsubscribe: [],
      syncListeners: new Set(),
    };
    d.handle = {
      docId,
      doc,
      awareness,
      get lastSeq() {
        return d.lastSeq;
      },
      get pendingCount() {
        return d.pending.length;
      },
      get synced() {
        return d.opened && !d.dirty && d.pending.length === 0 && d.known.size === 0;
      },
      onSync(listener: () => void) {
        d.syncListeners.add(listener);
        return () => {
          d.syncListeners.delete(listener);
        };
      },
      close: () => this.closeDoc(docId),
    };
    d.unsubscribe.push(doc.subscribeLocalUpdates(() => this.scheduleFlush(d)));
    d.unsubscribe.push(awareness.store.subscribeLocalUpdates(() => this.scheduleAwareness(d)));
    this.docs.set(docId, d);
    this.sendOpen(d);
    return d;
  }

  /** Releases one reference; the last release flushes pending work, tells the server and frees the doc. */
  async closeDoc(docId: string): Promise<void> {
    const d = this.docs.get(docId);
    if (!d) return;
    d.refs = Math.max(0, d.refs - 1);
    if (d.refs > 0) return;
    d.closing = true;
    if (d.coalesceTimer) {
      clearTimeout(d.coalesceTimer);
      d.coalesceTimer = null;
    }
    if (d.dirty) this.flushLocal(d);
    if (d.pending.length > 0 && d.opened && this.welcomed) await this.drain(d, CLOSE_DRAIN_MS);
    if (d.refs > 0 || !this.docs.has(docId)) {
      d.closing = false;
      return;
    }
    this.destroyDoc(d, true);
  }

  private drain(d: DocState, timeoutMs: number): Promise<void> {
    return new Promise((resolve) => {
      let done = false;
      const finish = (): void => {
        if (done) return;
        done = true;
        clearTimeout(timer);
        d.syncListeners.delete(check);
        resolve();
      };
      const check = (): void => {
        if (d.pending.length === 0 || !d.opened || d.refs > 0) finish();
      };
      const timer = setTimeout(finish, timeoutMs);
      d.syncListeners.add(check);
    });
  }

  private destroyDoc(d: DocState, notifyServer: boolean): void {
    this.docs.delete(d.docId);
    for (const t of [d.coalesceTimer, d.gapTimer, d.retryTimer, d.awarenessTimer]) if (t) clearTimeout(t);
    d.coalesceTimer = d.gapTimer = d.retryTimer = d.awarenessTimer = null;
    for (const u of d.unsubscribe) {
      try {
        u();
      } catch {
        // already gone
      }
    }
    d.unsubscribe = [];
    if (notifyServer && this.welcomed && d.opened && this.conn) {
      // An empty awareness state tells the peers we left before the room does.
      this.send(frames.awareness(d.docId, new Uint8Array(), this._clientId));
      this.send(frames.close(d.docId, "closed"));
    }
    d.opened = false;
    d.syncListeners.clear();
    d.awareness.destroy();
  }

  private notifySync(d: DocState): void {
    for (const l of [...d.syncListeners]) {
      try {
        l();
      } catch (err) {
        this.opts.log("sync: listener failed", err);
      }
    }
  }
}
