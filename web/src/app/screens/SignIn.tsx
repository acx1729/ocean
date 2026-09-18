import { useEffect, useState } from "react";
import { auth } from "../../lib/auth";
import { config } from "../../lib/config";
import { loadIdentity, shortDid } from "../../lib/identity";
import { hasInjectedWallet } from "../../lib/wallet";
import { Banner } from "../components";
import { useAuth } from "../hooks";

export function SignIn({ note }: { note?: string }) {
  const state = useAuth();
  const [busy, setBusy] = useState<"key" | "wallet" | null>(null);
  const [did, setDid] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    void loadIdentity().then((id) => setDid(id?.did ?? null));
  }, []);

  const run = async (kind: "key" | "wallet") => {
    setBusy(kind);
    setError(null);
    try {
      if (kind === "key") await auth.signInWithLocalKey();
      else await auth.signInWithWallet();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="center">
      <div className="card stack">
        <div>
          <h1>Sign in</h1>
          <p className="muted small">
            This node authenticates identities, not passwords. A local key lives only in this browser; a
            wallet signs a standard message.
          </p>
        </div>
        {note && <Banner kind="warn">{note}</Banner>}
        {(error || state.error) && <Banner kind="error">{error ?? state.error}</Banner>}
        <button
          className="primary"
          disabled={busy !== null}
          onClick={() => void run("key")}
          data-testid="signin-key"
        >
          {busy === "key"
            ? "Signing in…"
            : did
              ? "Continue with this browser's key"
              : "Create a key in this browser"}
        </button>
        {did && (
          <div className="small muted mono" title={did} data-testid="local-did">
            {shortDid(did)}
          </div>
        )}
        {config.wallets && hasInjectedWallet() && (
          <button disabled={busy !== null} onClick={() => void run("wallet")}>
            {busy === "wallet" ? "Waiting for the wallet…" : "Connect a wallet"}
          </button>
        )}
        <p className="small muted">
          To join someone else's workspace, send them your DID; they invite it from the workspace's Members
          panel.
        </p>
      </div>
    </div>
  );
}
