// Exponential backoff with jitter for reconnects: min * 2^attempt capped at max,
// scaled by a random factor in [0.8, 1.2) so that many tabs do not reconnect in
// lockstep after a server restart.

export interface BackoffOptions {
  minMs: number;
  maxMs: number;
}

export function backoffDelay(
  attempt: number,
  opts: BackoffOptions,
  random: () => number = Math.random,
): number {
  const base = Math.min(opts.maxMs, opts.minMs * 2 ** Math.max(0, attempt));
  const jitter = 0.8 + random() * 0.4;
  return Math.round(Math.min(opts.maxMs, base * jitter));
}
