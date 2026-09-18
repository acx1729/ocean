import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, errorMessage } from "../../lib/api";
import { auth } from "../../lib/auth";
import { Banner, Spinner } from "../components";
import { useAuth } from "../hooks";
import { SignIn } from "./SignIn";

export function AcceptInvite() {
  const { wsId = "", inviteId = "" } = useParams();
  const state = useAuth();
  const navigate = useNavigate();
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (state.status !== "signed_in") return;
    let cancelled = false;
    (async () => {
      try {
        await api.members.acceptInvite({ workspaceId: wsId, inviteId });
        await auth.reloadMe();
        if (!cancelled) navigate(`/w/${wsId}`, { replace: true });
      } catch (err) {
        if (!cancelled) setError(errorMessage(err));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [state.status, wsId, inviteId, navigate]);

  if (state.status === "loading") {
    return (
      <div className="center">
        <Spinner />
      </div>
    );
  }
  if (state.status === "signed_out")
    return <SignIn note="Sign in with the invited identity to accept the invitation." />;
  return (
    <div className="center">
      <div className="card stack">
        {error ? (
          <>
            <Banner kind="error">{error}</Banner>
            <Link to="/">Back to workspaces</Link>
          </>
        ) : (
          <Spinner label="Accepting the invitation…" />
        )}
      </div>
    </div>
  );
}
