// The access token holder shared by the RPC transport, the sync client and the
// auth flows. Nothing here persists: the refresh token lives in an HttpOnly
// cookie that the browser sends to AuthService.Refresh, and the access token
// stays in memory.

type Listener = () => void;

export interface Tokens {
  accessToken: string;
  /** Unix milliseconds. */
  accessExpiresAt: number;
}

let tokens: Tokens | null = null;
let refresher: (() => Promise<boolean>) | null = null;
let refreshing: Promise<boolean> | null = null;
const listeners = new Set<Listener>();

const SESSION_FLAG = "kb.session";

export const session = {
  token(): string | null {
    return tokens?.accessToken ?? null;
  },
  tokens(): Tokens | null {
    return tokens;
  },
  set(next: Tokens | null): void {
    tokens = next;
    try {
      if (next) localStorage.setItem(SESSION_FLAG, "1");
      else localStorage.removeItem(SESSION_FLAG);
    } catch {
      // storage may be unavailable (private mode); the session is then per page load
    }
    for (const l of [...listeners]) l();
  },
  /** Whether a previous page load left a session behind (a refresh may revive it). */
  remembered(): boolean {
    try {
      return localStorage.getItem(SESSION_FLAG) === "1";
    } catch {
      return false;
    }
  },
  /** Seconds until the access token expires (negative when expired or absent). */
  ttl(): number {
    if (!tokens) return -1;
    return (tokens.accessExpiresAt - Date.now()) / 1000;
  },
  onRefresh(fn: () => Promise<boolean>): void {
    refresher = fn;
  },
  /** Runs one refresh at a time; concurrent callers share the result. */
  refresh(): Promise<boolean> {
    if (!refresher) return Promise.resolve(false);
    if (!refreshing) {
      refreshing = refresher().finally(() => {
        refreshing = null;
      });
    }
    return refreshing;
  },
  subscribe(l: Listener): () => void {
    listeners.add(l);
    return () => {
      listeners.delete(l);
    };
  },
};
