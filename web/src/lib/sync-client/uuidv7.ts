// UUIDv7 (RFC 9562): 48-bit Unix millisecond timestamp, 12 random bits, 62 random
// bits. Ids generated in the same millisecond stay monotonic through a counter in
// the rand_a field, which keeps client ids and block ids sortable by creation.

let lastMs = 0;
let counter = 0;

function randomBytes(n: number): Uint8Array {
  const b = new Uint8Array(n);
  globalThis.crypto.getRandomValues(b);
  return b;
}

export function uuidv7(now: number = Date.now()): string {
  let ms = Math.max(now, lastMs);
  if (ms === lastMs) {
    counter += 1;
    if (counter > 0xfff) {
      // Counter overflow within one millisecond: borrow the next millisecond.
      ms += 1;
      counter = 0;
    }
  } else {
    counter = randomBytes(2)[0]! & 0x7ff; // random start, leaves headroom
  }
  lastMs = ms;

  const b = new Uint8Array(16);
  b[0] = (ms / 2 ** 40) & 0xff;
  b[1] = (ms / 2 ** 32) & 0xff;
  b[2] = (ms / 2 ** 24) & 0xff;
  b[3] = (ms / 2 ** 16) & 0xff;
  b[4] = (ms / 2 ** 8) & 0xff;
  b[5] = ms & 0xff;
  b[6] = 0x70 | ((counter >> 8) & 0x0f);
  b[7] = counter & 0xff;
  const rand = randomBytes(8);
  b.set(rand, 8);
  b[8] = (b[8]! & 0x3f) | 0x80; // RFC 4122 variant
  return formatUuid(b);
}

function formatUuid(b: Uint8Array): string {
  const hex: string[] = [];
  for (let i = 0; i < 16; i++) hex.push(b[i]!.toString(16).padStart(2, "0"));
  const h = hex.join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export function isUuidv7(s: string): boolean {
  return UUID_RE.test(s);
}
