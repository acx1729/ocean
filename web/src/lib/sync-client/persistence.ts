// Persistence adapters for offline docs: an in-memory one for tests and an
// IndexedDB one for browsers that encrypts every record with AES-256-GCM under
// a per-device non-extractable key (specification section 11, "Encryption on
// the device"). The key never leaves the device; sign-out clears the stores.

import type { DocMeta, PendingUpdate, PersistenceAdapter, StoredDoc } from "./types";

export class MemoryPersistence implements PersistenceAdapter {
  readonly docs = new Map<string, StoredDoc>();

  private get(docId: string): StoredDoc {
    let d = this.docs.get(docId);
    if (!d) {
      d = { meta: { lastSeq: 0, clientId: "", clientSeq: 0 }, snapshot: null, tail: [], pending: [] };
      this.docs.set(docId, d);
    }
    return d;
  }

  async load(docId: string): Promise<StoredDoc | null> {
    const d = this.docs.get(docId);
    return d ? structuredClone(d) : null;
  }
  async appendLocal(docId: string, entry: PendingUpdate, meta: DocMeta): Promise<void> {
    const d = this.get(docId);
    d.tail.push(entry.update);
    d.pending.push(entry);
    d.meta = { ...meta };
  }
  async appendTail(docId: string, update: Uint8Array, meta: DocMeta): Promise<void> {
    const d = this.get(docId);
    d.tail.push(update);
    d.meta = { ...meta };
  }
  async ackPending(docId: string, clientSeqs: number[], meta: DocMeta): Promise<void> {
    const d = this.get(docId);
    const gone = new Set(clientSeqs);
    d.pending = d.pending.filter((p) => !gone.has(p.clientSeq));
    d.meta = { ...meta };
  }
  async writeSnapshot(docId: string, snapshot: Uint8Array, meta: DocMeta): Promise<void> {
    const d = this.get(docId);
    d.snapshot = snapshot;
    d.tail = [];
    d.meta = { ...meta };
  }
  async remove(docId: string): Promise<void> {
    this.docs.delete(docId);
  }
  async clear(): Promise<void> {
    this.docs.clear();
  }
}

// --- binary record format -----------------------------------------------------
// u8 version=1 | u32 metaLen | meta JSON | u8 hasSnapshot | [u32 len | bytes] |
// u32 tailCount | (u32 len | bytes)* | u32 pendingCount | (u32 clientSeq | u32 len | bytes)*

const encoder = new TextEncoder();

/** A standalone ArrayBuffer copy (WebCrypto rejects views over shared buffers in the type system). */
function arrayBuffer(u: Uint8Array): ArrayBuffer {
  return u.buffer.slice(u.byteOffset, u.byteOffset + u.byteLength) as ArrayBuffer;
}
const decoder = new TextDecoder();

export function encodeStoredDoc(d: StoredDoc): Uint8Array {
  const meta = encoder.encode(JSON.stringify(d.meta));
  let size = 1 + 4 + meta.length + 1 + (d.snapshot ? 4 + d.snapshot.length : 0) + 4 + 4;
  for (const t of d.tail) size += 4 + t.length;
  for (const p of d.pending) size += 8 + p.update.length;
  const out = new Uint8Array(size);
  const view = new DataView(out.buffer);
  let o = 0;
  out[o++] = 1;
  view.setUint32(o, meta.length);
  o += 4;
  out.set(meta, o);
  o += meta.length;
  out[o++] = d.snapshot ? 1 : 0;
  if (d.snapshot) {
    view.setUint32(o, d.snapshot.length);
    o += 4;
    out.set(d.snapshot, o);
    o += d.snapshot.length;
  }
  view.setUint32(o, d.tail.length);
  o += 4;
  for (const t of d.tail) {
    view.setUint32(o, t.length);
    o += 4;
    out.set(t, o);
    o += t.length;
  }
  view.setUint32(o, d.pending.length);
  o += 4;
  for (const p of d.pending) {
    view.setUint32(o, p.clientSeq);
    o += 4;
    view.setUint32(o, p.update.length);
    o += 4;
    out.set(p.update, o);
    o += p.update.length;
  }
  return out;
}

export function decodeStoredDoc(b: Uint8Array): StoredDoc {
  const view = new DataView(b.buffer, b.byteOffset, b.byteLength);
  let o = 0;
  if (b[o++] !== 1) throw new Error("unknown stored doc version");
  const metaLen = view.getUint32(o);
  o += 4;
  const meta = JSON.parse(decoder.decode(b.subarray(o, o + metaLen))) as DocMeta;
  o += metaLen;
  let snapshot: Uint8Array | null = null;
  if (b[o++] === 1) {
    const n = view.getUint32(o);
    o += 4;
    snapshot = b.slice(o, o + n);
    o += n;
  }
  const tailCount = view.getUint32(o);
  o += 4;
  const tail: Uint8Array[] = [];
  for (let i = 0; i < tailCount; i++) {
    const n = view.getUint32(o);
    o += 4;
    tail.push(b.slice(o, o + n));
    o += n;
  }
  const pendingCount = view.getUint32(o);
  o += 4;
  const pending: PendingUpdate[] = [];
  for (let i = 0; i < pendingCount; i++) {
    const clientSeq = view.getUint32(o);
    o += 4;
    const n = view.getUint32(o);
    o += 4;
    pending.push({ clientSeq, update: b.slice(o, o + n) });
    o += n;
  }
  return { meta, snapshot, tail, pending };
}

// --- IndexedDB ------------------------------------------------------------------

const DOCS_DB = "kb-docs";
const KEY_DB = "kb-device-key";

function openDB(name: string, store: string): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(name, 1);
    req.onupgradeneeded = () => {
      if (!req.result.objectStoreNames.contains(store)) req.result.createObjectStore(store);
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

function idbRequest<T>(req: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

/** Loads or creates the per-device AES-GCM key (non-extractable). */
export async function deviceKey(): Promise<CryptoKey> {
  const db = await openDB(KEY_DB, "keys");
  try {
    const existing = await idbRequest(
      db.transaction("keys", "readonly").objectStore("keys").get("cache") as IDBRequest<
        CryptoKey | undefined
      >,
    );
    if (existing) return existing;
    const key = await crypto.subtle.generateKey({ name: "AES-GCM", length: 256 }, false, [
      "encrypt",
      "decrypt",
    ]);
    await idbRequest(db.transaction("keys", "readwrite").objectStore("keys").put(key, "cache"));
    return key;
  } finally {
    db.close();
  }
}

export async function clearDeviceStores(): Promise<void> {
  for (const name of [DOCS_DB, KEY_DB]) {
    await new Promise<void>((resolve) => {
      const req = indexedDB.deleteDatabase(name);
      req.onsuccess = () => resolve();
      req.onerror = () => resolve();
      req.onblocked = () => resolve();
    });
  }
}

/**
 * IndexedDBPersistence keeps one encrypted record per doc. Writes rewrite the
 * record (tails are bounded by the snapshot interval), which keeps the format
 * simple and every record self-consistent.
 */
export class IndexedDBPersistence implements PersistenceAdapter {
  private db: Promise<IDBDatabase> | null = null;
  private key: Promise<CryptoKey> | null = null;
  private cache = new Map<string, StoredDoc>();
  private chain = Promise.resolve();

  private open(): Promise<IDBDatabase> {
    if (!this.db) this.db = openDB(DOCS_DB, "docs");
    return this.db;
  }

  private getKey(): Promise<CryptoKey> {
    if (!this.key) this.key = deviceKey();
    return this.key;
  }

  private async read(docId: string): Promise<StoredDoc | null> {
    const cached = this.cache.get(docId);
    if (cached) return cached;
    const db = await this.open();
    const blob = await idbRequest(
      db.transaction("docs", "readonly").objectStore("docs").get(docId) as IDBRequest<Uint8Array | undefined>,
    );
    if (!blob) return null;
    const key = await this.getKey();
    const iv = arrayBuffer(blob.subarray(0, 12));
    const plain = new Uint8Array(
      await crypto.subtle.decrypt(
        { name: "AES-GCM", iv, additionalData: encoder.encode(docId) },
        key,
        arrayBuffer(blob.subarray(12)),
      ),
    );
    const d = decodeStoredDoc(plain);
    this.cache.set(docId, d);
    return d;
  }

  private async write(docId: string, d: StoredDoc): Promise<void> {
    this.cache.set(docId, d);
    const key = await this.getKey();
    const iv = crypto.getRandomValues(new Uint8Array(12));
    const ct = new Uint8Array(
      await crypto.subtle.encrypt(
        { name: "AES-GCM", iv, additionalData: encoder.encode(docId) },
        key,
        arrayBuffer(encodeStoredDoc(d)),
      ),
    );
    const blob = new Uint8Array(12 + ct.length);
    blob.set(iv, 0);
    blob.set(ct, 12);
    const db = await this.open();
    await idbRequest(db.transaction("docs", "readwrite").objectStore("docs").put(blob, docId));
  }

  /** Serializes writes per adapter so records never interleave. */
  private run<T>(fn: () => Promise<T>): Promise<T> {
    const p = this.chain.then(fn, fn);
    this.chain = p.then(
      () => undefined,
      () => undefined,
    );
    return p;
  }

  private async mutate(docId: string, meta: DocMeta, fn: (d: StoredDoc) => void): Promise<void> {
    return this.run(async () => {
      const d = (await this.read(docId)) ?? { meta: { ...meta }, snapshot: null, tail: [], pending: [] };
      fn(d);
      d.meta = { ...meta };
      await this.write(docId, d);
    });
  }

  load(docId: string): Promise<StoredDoc | null> {
    return this.run(() => this.read(docId));
  }
  appendLocal(docId: string, entry: PendingUpdate, meta: DocMeta): Promise<void> {
    return this.mutate(docId, meta, (d) => {
      d.tail.push(entry.update);
      d.pending.push(entry);
    });
  }
  appendTail(docId: string, update: Uint8Array, meta: DocMeta): Promise<void> {
    return this.mutate(docId, meta, (d) => {
      d.tail.push(update);
    });
  }
  ackPending(docId: string, clientSeqs: number[], meta: DocMeta): Promise<void> {
    const gone = new Set(clientSeqs);
    return this.mutate(docId, meta, (d) => {
      d.pending = d.pending.filter((p) => !gone.has(p.clientSeq));
    });
  }
  writeSnapshot(docId: string, snapshot: Uint8Array, meta: DocMeta): Promise<void> {
    return this.mutate(docId, meta, (d) => {
      d.snapshot = snapshot;
      d.tail = [];
    });
  }
  remove(docId: string): Promise<void> {
    return this.run(async () => {
      this.cache.delete(docId);
      const db = await this.open();
      await idbRequest(db.transaction("docs", "readwrite").objectStore("docs").delete(docId));
    });
  }
  clear(): Promise<void> {
    return this.run(async () => {
      this.cache.clear();
      const db = await this.open();
      await idbRequest(db.transaction("docs", "readwrite").objectStore("docs").clear());
    });
  }
}
