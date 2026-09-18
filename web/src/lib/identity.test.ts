import { describe, expect, it } from "vitest";
import { base58Decode, base58Encode } from "./base58";
import { didKeyFromPublicKey, publicKeyFromDidKey } from "./identity";

describe("did:key", () => {
  it("round-trips base58", () => {
    const bytes = new Uint8Array([0, 0, 1, 2, 3, 255, 128, 7]);
    expect(base58Decode(base58Encode(bytes))).toEqual(bytes);
    expect(base58Encode(new Uint8Array([0]))).toBe("1");
  });

  it("encodes Ed25519 keys with the z6Mk prefix and decodes them", () => {
    const pub = new Uint8Array(32);
    for (let i = 0; i < 32; i++) pub[i] = (i * 37 + 11) & 0xff;
    const did = didKeyFromPublicKey(pub);
    expect(did.startsWith("did:key:z6Mk")).toBe(true);
    expect(publicKeyFromDidKey(did)).toEqual(pub);
  });

  it("matches the W3C test vector", () => {
    // https://w3c-ccg.github.io/did-method-key/#ed25519-x25519
    const did = "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK";
    const pub = publicKeyFromDidKey(did);
    expect(pub).toHaveLength(32);
    expect(didKeyFromPublicKey(pub)).toBe(did);
  });
});
