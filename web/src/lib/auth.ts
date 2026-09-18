// Sign-in flows (did:key and did:pkh), session bootstrap and sign-out.

import { create } from "@bufbuild/protobuf";
import { clearDeviceStores } from "@kb/sync-client";
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { type MeResponse, VerifyRequestSchema } from "../gen/kb/v1/auth_pb";
import { api, errorMessage } from "./api";
import { getOrCreateIdentity, type LocalIdentity } from "./identity";
import { session } from "./session";
import { connectWallet } from "./wallet";

export type AuthStatus = "loading" | "signed_out" | "signed_in";

export interface AuthState {
  status: AuthStatus;
  me: MeResponse | null;
  error: string | null;
}

type Listener = (s: AuthState) => void;

let state: AuthState = { status: "loading", me: null, error: null };
const listeners = new Set<Listener>();

function setState(patch: Partial<AuthState>): void {
  state = { ...state, ...patch };
  for (const l of [...listeners]) l(state);
}

export const auth = {
  state(): AuthState {
    return state;
  },
  subscribe(l: Listener): () => void {
    listeners.add(l);
    return () => {
      listeners.delete(l);
    };
  },

  /** Revives a remembered session through the refresh cookie, then loads the profile. */
  async bootstrap(): Promise<void> {
    if (!session.remembered()) {
      setState({ status: "signed_out" });
      return;
    }
    const ok = await refresh();
    if (!ok) {
      setState({ status: "signed_out" });
      return;
    }
    await loadMe();
  },

  async signInWithLocalKey(): Promise<void> {
    const id: LocalIdentity = await getOrCreateIdentity();
    await signIn(id.did, async (message) => id.sign(new TextEncoder().encode(message)));
  },

  async signInWithWallet(): Promise<void> {
    const wallet = await connectWallet();
    await signIn(wallet.did, (message) => wallet.signMessage(message));
  },

  async signOut(): Promise<void> {
    try {
      await api.auth.revoke({});
    } catch {
      // the local state is cleared regardless
    }
    session.set(null);
    try {
      await clearDeviceStores();
    } catch {
      // best effort: the cache is encrypted with a key that is discarded here anyway
    }
    setState({ status: "signed_out", me: null, error: null });
  },

  /** Token provider for the sync client. */
  async accessToken(reason: "connect" | "unauthenticated"): Promise<string> {
    if (reason === "unauthenticated" || session.ttl() < 60) {
      const ok = await refresh();
      if (!ok) throw new Error("session expired");
    }
    const t = session.token();
    if (!t) throw new Error("signed out");
    return t;
  },

  async reloadMe(): Promise<void> {
    await loadMe();
  },
};

async function signIn(did: string, sign: (message: string) => Promise<Uint8Array>): Promise<void> {
  setState({ error: null });
  try {
    const ch = await api.auth.challenge({ did });
    const signature = await sign(ch.message);
    const res = await api.auth.verify(
      create(VerifyRequestSchema, { did, nonce: ch.nonce, signature, message: ch.message }),
    );
    session.set({
      accessToken: res.accessToken,
      accessExpiresAt: res.accessExpiresAt ? timestampMs(res.accessExpiresAt) : Date.now() + 10 * 60_000,
    });
    await loadMe();
  } catch (err) {
    setState({ status: "signed_out", error: errorMessage(err) });
    throw err;
  }
}

async function refresh(): Promise<boolean> {
  try {
    const res = await api.auth.refresh({});
    session.set({
      accessToken: res.accessToken,
      accessExpiresAt: res.accessExpiresAt ? timestampMs(res.accessExpiresAt) : Date.now() + 10 * 60_000,
    });
    return true;
  } catch {
    session.set(null);
    return false;
  }
}

async function loadMe(): Promise<void> {
  try {
    const me = await api.auth.me({});
    setState({ status: "signed_in", me, error: null });
  } catch (err) {
    setState({ status: "signed_out", me: null, error: errorMessage(err) });
  }
}

session.onRefresh(refresh);
