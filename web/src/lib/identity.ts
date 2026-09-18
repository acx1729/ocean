// Local did:key identities. The private key is a WebCrypto Ed25519 key kept
// non-extractable in IndexedDB when the browser supports it; otherwise a
// software key (noble) whose seed is sealed with the per-device AES key that
// also protects the doc cache.

import { base58Decode, base58Encode } from "./base58";

const DB_NAME = "kb-identity";
const STORE = "keys";
const RECORD = "local";
const ED25519_MULTICODEC = new Uint8Array([0xed, 0x01]);

/** A standalone ArrayBuffer copy for WebCrypto (which rejects views over shared buffers in the type system). */
function ab(u: Uint8Array): ArrayBuffer {
  return u.buffer.slice(u.byteOffset, u.byteOffset + u.byteLength) as ArrayBuffer;
}

export interface LocalIdentity {
  did: string;
  publicKey: Uint8Array;
  kind: "webcrypto" | "software";
  sign(message: Uint8Array): Promise<Uint8Array>;
}

interface StoredKey {
  id: string;
  kind: "webcrypto" | "software";
  publicKey: Uint8Array;
  /** WebCrypto: the (non-extractable) private key object. */
  privateKey?: CryptoKey;
  /** Software: AES-GCM sealed seed, iv || ciphertext. */
  sealedSeed?: Uint8Array;
  createdAt: number;
}

export function didKeyFromPublicKey(publicKey: Uint8Array): string {
  if (publicKey.length !== 32) throw new Error("did:key: Ed25519 public keys are 32 bytes");
  const bytes = new Uint8Array(2 + 32);
  bytes.set(ED25519_MULTICODEC, 0);
  bytes.set(publicKey, 2);
  return `did:key:z${base58Encode(bytes)}`;
}

export function publicKeyFromDidKey(did: string): Uint8Array {
  if (!did.startsWith("did:key:z")) throw new Error("did:key: expected a base58btc multibase string");
  const bytes = base58Decode(did.slice("did:key:z".length));
  if (bytes.length !== 34 || bytes[0] !== 0xed || bytes[1] !== 0x01)
    throw new Error("did:key: not an Ed25519 key");
  return bytes.slice(2);
}

export function shortDid(did: string): string {
  if (did.length <= 24) return did;
  return `${did.slice(0, 14)}…${did.slice(-6)}`;
}

function openDB(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, 1);
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains(STORE)) db.createObjectStore(STORE, { keyPath: "id" });
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error ?? new Error("indexedDB open failed"));
  });
}

function idb<T>(req: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error ?? new Error("indexedDB request failed"));
  });
}

async function readStored(): Promise<StoredKey | null> {
  const db = await openDB();
  try {
    const rec = await idb(
      db.transaction(STORE, "readonly").objectStore(STORE).get(RECORD) as IDBRequest<StoredKey | undefined>,
    );
    return rec ?? null;
  } finally {
    db.close();
  }
}

async function writeStored(rec: StoredKey): Promise<void> {
  const db = await openDB();
  try {
    await idb(db.transaction(STORE, "readwrite").objectStore(STORE).put(rec));
  } finally {
    db.close();
  }
}

async function webCryptoSupported(): Promise<boolean> {
  try {
    const pair = (await crypto.subtle.generateKey({ name: "Ed25519" }, false, [
      "sign",
      "verify",
    ])) as CryptoKeyPair;
    return !!pair.privateKey;
  } catch {
    return false;
  }
}

async function deviceAesKey(): Promise<CryptoKey> {
  const { deviceKey } = await import("@kb/sync-client");
  return deviceKey();
}

async function toIdentity(rec: StoredKey): Promise<LocalIdentity> {
  const did = didKeyFromPublicKey(rec.publicKey);
  if (rec.kind === "webcrypto" && rec.privateKey) {
    const priv = rec.privateKey;
    return {
      did,
      publicKey: rec.publicKey,
      kind: "webcrypto",
      async sign(message) {
        return new Uint8Array(await crypto.subtle.sign("Ed25519", priv, ab(message)));
      },
    };
  }
  if (rec.kind === "software" && rec.sealedSeed) {
    const sealed = rec.sealedSeed;
    return {
      did,
      publicKey: rec.publicKey,
      kind: "software",
      async sign(message) {
        const key = await deviceAesKey();
        const seed = new Uint8Array(
          await crypto.subtle.decrypt(
            { name: "AES-GCM", iv: ab(sealed.subarray(0, 12)) },
            key,
            ab(sealed.subarray(12)),
          ),
        );
        const { ed25519 } = await import("@noble/curves/ed25519");
        try {
          return ed25519.sign(message, seed);
        } finally {
          seed.fill(0);
        }
      },
    };
  }
  throw new Error("identity record is unusable");
}

export async function loadIdentity(): Promise<LocalIdentity | null> {
  const rec = await readStored();
  if (!rec) return null;
  try {
    return await toIdentity(rec);
  } catch {
    return null;
  }
}

export async function createIdentity(): Promise<LocalIdentity> {
  let rec: StoredKey;
  if (await webCryptoSupported()) {
    const pair = (await crypto.subtle.generateKey({ name: "Ed25519" }, false, [
      "sign",
      "verify",
    ])) as CryptoKeyPair;
    const publicKey = new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey));
    rec = { id: RECORD, kind: "webcrypto", publicKey, privateKey: pair.privateKey, createdAt: Date.now() };
  } else {
    const { ed25519 } = await import("@noble/curves/ed25519");
    const seed = ed25519.utils.randomPrivateKey();
    const publicKey = ed25519.getPublicKey(seed);
    const key = await deviceAesKey();
    const iv = crypto.getRandomValues(new Uint8Array(12));
    const ct = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv }, key, ab(seed)));
    seed.fill(0);
    const sealedSeed = new Uint8Array(12 + ct.length);
    sealedSeed.set(iv, 0);
    sealedSeed.set(ct, 12);
    rec = { id: RECORD, kind: "software", publicKey, sealedSeed, createdAt: Date.now() };
  }
  await writeStored(rec);
  return toIdentity(rec);
}

export async function getOrCreateIdentity(): Promise<LocalIdentity> {
  return (await loadIdentity()) ?? createIdentity();
}

export async function forgetIdentity(): Promise<void> {
  const db = await openDB();
  try {
    await idb(db.transaction(STORE, "readwrite").objectStore(STORE).delete(RECORD));
  } finally {
    db.close();
  }
}
