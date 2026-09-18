import { create } from "@bufbuild/protobuf";
import { LoroDoc } from "loro-crdt";
import { describe, expect, it } from "vitest";
import {
  AwarenessSchema,
  CloseSchema,
  OpenResponseSchema,
  PullResponseSchema,
  PushAckSchema,
  SyncErrorSchema,
  UpdateSchema,
  WelcomeSchema,
  type SyncFrame,
} from "../../gen/kb/v1/sync_pb";
import { SyncClient } from "./client";
import { decodeFrame, encodeFrame } from "./frames";
import { MemoryPersistence } from "./persistence";
import type { ConnectOptions, PersistenceAdapter, TokenReason, WebSocketLike } from "./types";

// --- an in-memory server speaking the /ws/sync protocol -------------------------

interface ServerDoc {
  doc: LoroDoc;
  log: { seq: number; update: Uint8Array; actor: string }[];
  seen: Map<string, number>;
}

interface OpenRecord {
  docId: string;
  sinceSeq: number;
  mode: "snapshot" | "tail";
}

class FakeServer {
  readonly docs = new Map<string, ServerDoc>();
  readonly conns = new Set<FakeConn>();
  readonly rooms = new Map<string, Set<FakeConn>>();
  readonly awareness = new Map<string, Map<string, Uint8Array>>();
  readonly opens: OpenRecord[] = [];
  validTokens = new Set(["tok"]);
  dropNextFanout = 0;
  pushCount = 0;

  createDoc(docId: string): ServerDoc {
    const d: ServerDoc = { doc: new LoroDoc(), log: [], seen: new Map() };
    d.doc.getTree("blocks").enableFractionalIndex(0);
    this.docs.set(docId, d);
    return d;
  }

  room(docId: string): Set<FakeConn> {
    let r = this.rooms.get(docId);
    if (!r) {
      r = new Set();
      this.rooms.set(docId, r);
    }
    return r;
  }

  /** Appends an update committed outside the protocol (like an API write) and fans it out to everyone. */
  broadcast(docId: string, update: Uint8Array): number {
    const d = this.docs.get(docId)!;
    d.doc.import(update);
    const seq = d.log.length + 1;
    d.log.push({ seq, update, actor: "api" });
    this.fanout(docId, null, {
      case: "update",
      value: create(UpdateSchema, { docId, seq: BigInt(seq), update, actor: "api" }),
    });
    return seq;
  }

  closeDoc(docId: string, reason: string): void {
    for (const c of this.room(docId)) {
      c.opened.delete(docId);
      c.send({ case: "close", value: create(CloseSchema, { docId, reason }) });
    }
    this.room(docId).clear();
  }

  disconnectAll(): void {
    for (const c of [...this.conns]) c.socket.serverClose(1001, "restart");
  }

  fanout(docId: string, except: FakeConn | null, kind: SyncFrame["kind"]): void {
    for (const c of this.room(docId)) {
      if (c === except) continue;
      if (kind.case === "update" && this.dropNextFanout > 0) {
        this.dropNextFanout--;
        continue;
      }
      c.send(kind);
    }
  }
}

class FakeConn {
  authed = false;
  clientId = "";
  readonly opened = new Set<string>();

  constructor(
    readonly server: FakeServer,
    readonly socket: FakeSocket,
  ) {
    server.conns.add(this);
  }

  send(kind: SyncFrame["kind"]): void {
    this.socket.serverSend(encodeFrame(kind));
  }

  error(docId: string, code: string, message: string): void {
    this.send({ case: "error", value: create(SyncErrorSchema, { docId, code, message }) });
  }

  leaveAll(): void {
    for (const docId of this.opened) {
      this.server.room(docId).delete(this);
      this.server.awareness.get(docId)?.delete(this.clientId);
    }
    this.opened.clear();
    this.server.conns.delete(this);
  }

  handle(bytes: Uint8Array): void {
    const frame = decodeFrame(bytes);
    const k = frame.kind;
    if (!this.authed) {
      if (k.case !== "hello") {
        this.error("", "bad_frame", "first frame must be hello");
        return;
      }
      if (!this.server.validTokens.has(k.value.accessToken)) {
        this.error("", "unauthenticated", "bad token");
        this.socket.serverClose(1008, "unauthenticated");
        return;
      }
      this.authed = true;
      this.clientId = k.value.clientId;
      this.send({
        case: "welcome",
        value: create(WelcomeSchema, {
          principal: "did:key:z" + k.value.clientId.slice(0, 6),
          serverVersion: "test",
        }),
      });
      return;
    }
    switch (k.case) {
      case "open": {
        const req = k.value;
        const d = this.server.docs.get(req.docId);
        if (!d) {
          this.error(req.docId, "not_found", "no such doc");
          return;
        }
        this.server.room(req.docId).add(this);
        this.opened.add(req.docId);
        const since = Number(req.sinceSeq);
        const current = d.log.length;
        if (since > 0 && since <= current && current - since <= 2000) {
          this.server.opens.push({ docId: req.docId, sinceSeq: since, mode: "tail" });
          this.send({
            case: "opened",
            value: create(OpenResponseSchema, {
              docId: req.docId,
              snapshotSeq: BigInt(since),
              currentSeq: BigInt(current),
              tail: d.log
                .slice(since)
                .map((e) => ({ docId: req.docId, seq: BigInt(e.seq), update: e.update, actor: e.actor })),
            }),
          });
        } else {
          this.server.opens.push({ docId: req.docId, sinceSeq: since, mode: "snapshot" });
          this.send({
            case: "opened",
            value: create(OpenResponseSchema, {
              docId: req.docId,
              snapshot: d.doc.export({ mode: "snapshot" }),
              snapshotSeq: BigInt(current),
              currentSeq: BigInt(current),
            }),
          });
        }
        for (const [peer, state] of this.server.awareness.get(req.docId) ?? []) {
          if (peer !== this.clientId)
            this.send({
              case: "awareness",
              value: create(AwarenessSchema, { docId: req.docId, state, peer }),
            });
        }
        return;
      }
      case "push": {
        const req = k.value;
        this.server.pushCount++;
        if (!this.opened.has(req.docId)) {
          this.error(req.docId, "not_open", "open first");
          return;
        }
        if (req.clientId !== this.clientId) {
          this.error(req.docId, "bad_frame", "client id mismatch");
          return;
        }
        const d = this.server.docs.get(req.docId)!;
        const acked: { clientSeq: bigint; seq: bigint }[] = [];
        for (const u of req.updates) {
          const key = `${req.clientId}:${u.clientSeq}`;
          const prior = d.seen.get(key);
          if (prior !== undefined) {
            acked.push({ clientSeq: u.clientSeq, seq: BigInt(prior) });
            continue;
          }
          try {
            d.doc.import(u.update);
          } catch (err) {
            this.error(req.docId, "invalid_update", String(err));
            return;
          }
          const seq = d.log.length + 1;
          d.log.push({ seq, update: u.update, actor: this.clientId });
          d.seen.set(key, seq);
          acked.push({ clientSeq: u.clientSeq, seq: BigInt(seq) });
          this.server.fanout(req.docId, this, {
            case: "update",
            value: create(UpdateSchema, {
              docId: req.docId,
              seq: BigInt(seq),
              update: u.update,
              actor: this.clientId,
            }),
          });
        }
        this.send({ case: "ack", value: create(PushAckSchema, { docId: req.docId, acked }) });
        return;
      }
      case "pull": {
        const req = k.value;
        const d = this.server.docs.get(req.docId)!;
        const since = Number(req.sinceSeq);
        this.send({
          case: "pulled",
          value: create(PullResponseSchema, {
            docId: req.docId,
            currentSeq: BigInt(d.log.length),
            updates: d.log
              .filter((e) => e.seq > since)
              .map((e) => ({ docId: req.docId, seq: BigInt(e.seq), update: e.update, actor: e.actor })),
          }),
        });
        return;
      }
      case "awareness": {
        const a = k.value;
        if (!this.opened.has(a.docId)) return;
        let m = this.server.awareness.get(a.docId);
        if (!m) {
          m = new Map();
          this.server.awareness.set(a.docId, m);
        }
        if (a.state.length === 0) m.delete(this.clientId);
        else m.set(this.clientId, a.state);
        this.server.fanout(a.docId, this, {
          case: "awareness",
          value: create(AwarenessSchema, { docId: a.docId, state: a.state, peer: this.clientId }),
        });
        return;
      }
      case "close": {
        this.opened.delete(k.value.docId);
        this.server.room(k.value.docId).delete(this);
        this.server.awareness.get(k.value.docId)?.delete(this.clientId);
        return;
      }
      default:
        this.error("", "bad_frame", `unexpected ${k.case}`);
    }
  }
}

class FakeSocket implements WebSocketLike {
  binaryType = "blob";
  readyState = 0;
  onopen: ((ev: unknown) => void) | null = null;
  onmessage: ((ev: { data: ArrayBuffer | Uint8Array }) => void) | null = null;
  onclose: ((ev: { code?: number; reason?: string }) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;
  private readonly conn: FakeConn;

  constructor(readonly server: FakeServer) {
    this.conn = new FakeConn(server, this);
    setTimeout(() => {
      if (this.readyState !== 0) return;
      this.readyState = 1;
      this.onopen?.({});
    }, 0);
  }

  send(data: Uint8Array): void {
    if (this.readyState !== 1) throw new Error("socket not open");
    const copy = new Uint8Array(data);
    setTimeout(() => {
      if (this.readyState === 1) this.conn.handle(copy);
    }, 0);
  }

  close(code?: number, reason?: string): void {
    this.finish(code ?? 1000, reason ?? "");
  }

  serverSend(bytes: Uint8Array): void {
    if (this.readyState !== 1) return;
    setTimeout(() => {
      if (this.readyState === 1) this.onmessage?.({ data: bytes });
    }, 0);
  }

  serverClose(code: number, reason: string): void {
    setTimeout(() => this.finish(code, reason), 0);
  }

  private finish(code: number, reason: string): void {
    if (this.readyState === 3) return;
    this.readyState = 3;
    this.conn.leaveAll();
    setTimeout(() => this.onclose?.({ code, reason }), 0);
  }
}

// --- helpers ------------------------------------------------------------------------

function wsFor(server: FakeServer) {
  return class extends FakeSocket {
    constructor(_url: string) {
      super(server);
    }
  };
}

function newClient(server: FakeServer, persistence?: PersistenceAdapter): SyncClient {
  return new SyncClient({
    WebSocket: wsFor(server),
    persistence,
    coalesceMs: 5,
    gapGraceMs: 20,
    awarenessThrottleMs: 5,
    awarenessHeartbeatMs: 60_000,
    backoff: { minMs: 10, maxMs: 40 },
  });
}

function connOpts(token = "tok", onToken?: (reason: TokenReason) => string): ConnectOptions {
  return {
    url: "ws://test/ws/sync",
    workspaceId: "ws-1",
    getAccessToken: (reason) => (onToken ? onToken(reason) : token),
  };
}

async function until(cond: () => boolean, what = "condition", timeoutMs = 5000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!cond()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 5));
  }
}

function text(doc: LoroDoc): string {
  return doc.getText("t").toString();
}

// --- tests ---------------------------------------------------------------------------

describe("SyncClient", () => {
  it("converges two clients through the server and tracks seqs", async () => {
    const server = new FakeServer();
    server.createDoc("d1");
    const a = newClient(server);
    const b = newClient(server);
    a.connect(connOpts());
    b.connect(connOpts());
    const ha = await a.openDoc("d1");
    const hb = await b.openDoc("d1");
    await until(() => ha.synced && hb.synced, "both opened");

    ha.doc.getText("t").insert(0, "hello");
    ha.doc.commit();
    await until(() => text(hb.doc) === "hello", "b sees hello");
    hb.doc.getText("t").insert(5, " world");
    hb.doc.commit();
    await until(() => text(ha.doc) === "hello world", "a sees world");
    await until(() => ha.synced && hb.synced, "both synced");

    expect(ha.lastSeq).toBe(2);
    expect(hb.lastSeq).toBe(2);
    expect(text(server.docs.get("d1")!.doc)).toBe("hello world");
    a.destroy();
    b.destroy();
  });

  it("reports unsynced right after a local commit and flushes on demand", async () => {
    const server = new FakeServer();
    server.createDoc("d1");
    const a = newClient(server);
    a.connect(connOpts());
    const ha = await a.openDoc("d1");
    await until(() => ha.synced, "opened");
    ha.doc.getText("t").insert(0, "x");
    ha.doc.commit();
    expect(ha.synced).toBe(false);
    expect(ha.pendingCount).toBe(0);
    a.flushAll();
    expect(ha.pendingCount).toBe(1);
    expect(server.pushCount).toBe(0); // the frame is in flight
    await until(() => ha.synced, "acked");
    expect(server.pushCount).toBe(1);
    a.destroy();
  });

  it("coalesces rapid local commits into one push", async () => {
    const server = new FakeServer();
    server.createDoc("d1");
    const a = newClient(server);
    a.connect(connOpts());
    const ha = await a.openDoc("d1");
    await until(() => ha.synced, "opened");
    for (let i = 0; i < 20; i++) {
      ha.doc.getText("t").insert(i, "x");
      ha.doc.commit();
    }
    await until(() => ha.lastSeq === 1 && ha.synced, "acked");
    expect(server.pushCount).toBe(1);
    expect(ha.lastSeq).toBe(1);
    expect(text(server.docs.get("d1")!.doc)).toBe("x".repeat(20));
    a.destroy();
  });

  it("queues edits while offline and pushes them after reconnecting", async () => {
    const server = new FakeServer();
    server.createDoc("d1");
    const a = newClient(server);
    const b = newClient(server);
    const statuses: string[] = [];
    a.on("status", (s) => statuses.push(s));
    a.connect(connOpts());
    b.connect(connOpts());
    const ha = await a.openDoc("d1");
    const hb = await b.openDoc("d1");
    await until(() => ha.synced && hb.synced, "opened");

    server.disconnectAll();
    await until(() => a.status === "offline", "offline");
    ha.doc.getText("t").insert(0, "offline edit");
    ha.doc.commit();
    await until(() => ha.pendingCount === 1, "queued");
    expect(ha.synced).toBe(false);

    await until(() => a.status === "connected" && ha.synced, "reconnected and drained");
    expect(text(server.docs.get("d1")!.doc)).toBe("offline edit");
    await until(() => text(hb.doc) === "offline edit", "b converged");
    expect(statuses).toEqual(["connecting", "connected", "offline", "connecting", "connected"]);
    a.destroy();
    b.destroy();
  });

  it("re-sends pending updates with the same client_seq so the server can dedupe", async () => {
    const server = new FakeServer();
    server.createDoc("d1");
    const a = newClient(server);
    a.connect(connOpts());
    const ha = await a.openDoc("d1");
    await until(() => ha.synced, "opened");
    // Break the connection right after the push leaves, before the ack lands.
    ha.doc.getText("t").insert(0, "once");
    ha.doc.commit();
    await until(() => server.pushCount === 1, "push received");
    server.disconnectAll();
    await until(() => a.status === "connected" && ha.synced, "resynced");
    expect(server.docs.get("d1")!.log.length).toBe(1);
    expect(text(server.docs.get("d1")!.doc)).toBe("once");
    a.destroy();
  });

  it("recovers a lost update through a pull", async () => {
    const server = new FakeServer();
    server.createDoc("d1");
    const a = newClient(server);
    const b = newClient(server);
    a.connect(connOpts());
    b.connect(connOpts());
    const ha = await a.openDoc("d1");
    const hb = await b.openDoc("d1");
    await until(() => ha.synced && hb.synced, "opened");

    server.dropNextFanout = 1; // a never receives seq 1
    hb.doc.getText("t").insert(0, "one");
    hb.doc.commit();
    await until(() => hb.synced && hb.lastSeq === 1, "b acked");
    expect(ha.lastSeq).toBe(0);
    hb.doc.getText("t").insert(3, " two");
    hb.doc.commit();
    await until(() => ha.lastSeq === 2 && ha.synced, "a pulled the gap");
    expect(text(ha.doc)).toBe("one two");
    a.destroy();
    b.destroy();
  });

  it("restores a doc from the cache and resumes with a tail", async () => {
    const server = new FakeServer();
    server.createDoc("d1");
    const cache = new MemoryPersistence();
    const a = newClient(server, cache);
    a.connect(connOpts());
    const ha = await a.openDoc("d1");
    await until(() => ha.synced, "opened");
    ha.doc.getText("t").insert(0, "one");
    ha.doc.commit();
    await until(() => ha.synced && ha.lastSeq === 1, "synced");
    await ha.close();
    a.destroy();

    // Someone else appends while we are away.
    const other = new LoroDoc();
    other.import(server.docs.get("d1")!.doc.export({ mode: "snapshot" }));
    other.getText("t").insert(3, " two");
    other.commit();
    server.broadcast("d1", other.export({ mode: "update", from: server.docs.get("d1")!.doc.version() }));

    const a2 = newClient(server, cache);
    const h2 = await a2.openDoc("d1"); // offline: served from the cache
    expect(text(h2.doc)).toBe("one");
    expect(h2.lastSeq).toBe(1);
    a2.connect(connOpts());
    await until(() => h2.synced && h2.lastSeq === 2, "resumed");
    expect(text(h2.doc)).toBe("one two");
    expect(server.opens.at(-1)).toEqual({ docId: "d1", sinceSeq: 1, mode: "tail" });
    a2.destroy();
  });

  it("pushes edits made offline from the cache after the first connection", async () => {
    const server = new FakeServer();
    server.createDoc("d1");
    const cache = new MemoryPersistence();
    const a = newClient(server, cache);
    const ha = await a.openDoc("d1");
    ha.doc.getText("t").insert(0, "draft");
    ha.doc.commit();
    await until(() => ha.pendingCount === 1, "queued");
    a.destroy();

    const a2 = newClient(server, cache);
    const h2 = await a2.openDoc("d1");
    expect(h2.pendingCount).toBe(1);
    expect(text(h2.doc)).toBe("draft");
    a2.connect(connOpts());
    await until(() => h2.synced, "pushed");
    expect(text(server.docs.get("d1")!.doc)).toBe("draft");
    a2.destroy();
  });

  it("asks for a fresh token after the server rejects the hello", async () => {
    const server = new FakeServer();
    server.validTokens = new Set(["good"]);
    const reasons: TokenReason[] = [];
    const a = newClient(server);
    const errors: string[] = [];
    a.on("error", (e) => errors.push(e.code));
    a.connect(
      connOpts("", (reason) => {
        reasons.push(reason);
        return reason === "unauthenticated" ? "good" : "stale";
      }),
    );
    await until(() => a.status === "connected", "connected");
    expect(reasons).toEqual(["connect", "unauthenticated"]);
    expect(errors).toEqual(["unauthenticated"]);
    a.destroy();
  });

  it("relays awareness and drops peers that leave", async () => {
    const server = new FakeServer();
    server.createDoc("d1");
    const a = newClient(server);
    const b = newClient(server);
    a.connect(connOpts());
    b.connect(connOpts());
    const ha = await a.openDoc("d1");
    const hb = await b.openDoc("d1");
    await until(() => ha.synced && hb.synced, "opened");
    const pos = new Uint8Array([1, 2, 3]);
    ha.awareness.setLocal({ name: "Ada", color: "#f00", cursor: { blockId: "blk", anchor: pos, head: pos } });
    await until(() => hb.awareness.peers().get(a.clientId)?.name === "Ada", "b sees Ada");
    expect(hb.awareness.peers().get(a.clientId)?.cursor).toEqual({ blockId: "blk", anchor: pos, head: pos });

    // A late joiner receives the current states on open.
    const c = newClient(server);
    c.connect(connOpts());
    const hc = await c.openDoc("d1");
    await until(() => hc.awareness.peers().get(a.clientId)?.name === "Ada", "c sees Ada");

    await ha.close();
    await until(() => !hb.awareness.peers().has(a.clientId), "b dropped Ada");
    a.destroy();
    b.destroy();
    c.destroy();
  });

  it("reports docs the server closes and refuses unknown docs", async () => {
    const server = new FakeServer();
    server.createDoc("d1");
    const a = newClient(server);
    const closed: { docId: string; reason: string }[] = [];
    const errors: { docId?: string; code: string }[] = [];
    a.on("closed", (e) => closed.push(e));
    a.on("error", (e) => errors.push({ docId: e.docId, code: e.code }));
    a.connect(connOpts());
    const ha = await a.openDoc("d1");
    await until(() => ha.synced, "opened");
    server.closeDoc("d1", "deleted");
    await until(() => closed.length === 1, "closed event");
    expect(closed[0]).toEqual({ docId: "d1", reason: "deleted" });
    expect(ha.synced).toBe(false);

    await a.openDoc("missing");
    await until(() => closed.length === 2, "not found");
    expect(closed[1]).toEqual({ docId: "missing", reason: "not_found" });
    expect(errors.some((e) => e.docId === "missing" && e.code === "not_found")).toBe(true);
    a.destroy();
  });

  it("shares one handle per doc and frees it on the last close", async () => {
    const server = new FakeServer();
    server.createDoc("d1");
    const a = newClient(server);
    a.connect(connOpts());
    const h1 = await a.openDoc("d1");
    const h2 = await a.openDoc("d1");
    expect(h1).toBe(h2);
    await h1.close();
    expect(a.openDocIds()).toEqual(["d1"]);
    await h2.close();
    expect(a.openDocIds()).toEqual([]);
    a.destroy();
  });
});
