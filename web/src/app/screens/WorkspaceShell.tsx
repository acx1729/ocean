import type { SyncClient } from "@kb/sync-client";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createContext, useContext, useEffect, useMemo, useState } from "react";
import { Link, NavLink, Outlet, useParams } from "react-router-dom";
import type { Project, Workspace } from "../../gen/kb/v1/common_pb";
import { api, errorMessage } from "../../lib/api";
import { auth } from "../../lib/auth";
import { config } from "../../lib/config";
import { Banner, colorFor, displayName, Spinner } from "../components";
import { useAuth, useSyncStatus } from "../hooks";
import { syncClientFor } from "../sync";

export interface WorkspaceCtx {
  workspaceId: string;
  workspace: Workspace | null;
  role: string;
  projects: Project[];
  client: SyncClient;
  me: { did: string; name: string; color: string };
}

const Ctx = createContext<WorkspaceCtx | null>(null);

export function useWorkspace(): WorkspaceCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error("useWorkspace outside WorkspaceShell");
  return v;
}

export function WorkspaceShell() {
  const { wsId = "" } = useParams();
  const { me } = useAuth();
  const membership = me?.memberships.find((m) => m.workspace?.id === wsId);
  const projects = useQuery({
    queryKey: ["projects", wsId],
    queryFn: () => api.projects.list({ workspaceId: wsId, pageSize: 50 }),
    enabled: wsId !== "",
  });
  const client = useMemo(() => syncClientFor(wsId), [wsId]);
  const status = useSyncStatus(client);
  const [showMembers, setShowMembers] = useState(false);
  const did = me?.principal?.did ?? "";
  const ctx = useMemo<WorkspaceCtx>(
    () => ({
      workspaceId: wsId,
      workspace: membership?.workspace ?? null,
      role: membership?.role ?? "",
      projects: projects.data?.projects ?? [],
      client,
      me: { did, name: displayName(did, me?.principal?.displayName), color: colorFor(did) },
    }),
    [wsId, membership, projects.data, client, did, me?.principal?.displayName],
  );

  useEffect(() => {
    try {
      localStorage.setItem("kb.lastWorkspace", wsId);
    } catch {
      // ignore
    }
  }, [wsId]);

  if (!membership) {
    return (
      <div className="center">
        <div className="card stack">
          <Banner kind="warn">You are not a member of this workspace.</Banner>
          <Link to="/">Back to workspaces</Link>
        </div>
      </div>
    );
  }

  return (
    <Ctx.Provider value={ctx}>
      <div className="shell">
        <aside className="sidebar">
          <div className="row">
            <Link to="/" className="ghost small" title="All workspaces">
              ‹
            </Link>
            <span className="title grow" data-testid="workspace-name">
              {membership.workspace?.name}
            </span>
            <span
              className={`pill ${status === "connected" ? "ok" : status === "connecting" ? "" : "bad"}`}
              title={`sync: ${status}`}
            >
              <span className="dot" />
              {status === "connected" ? "online" : status}
            </span>
          </div>
          <nav className="nav">
            <div className="section">Projects</div>
            {projects.isLoading && <Spinner />}
            {projects.error && <Banner kind="error">{errorMessage(projects.error)}</Banner>}
            {ctx.projects.map((p) => (
              <NavLink
                key={p.id}
                to={`/w/${wsId}/p/${p.id}`}
                className={({ isActive }) => (isActive ? "active" : "")}
              >
                {p.name}
              </NavLink>
            ))}
          </nav>
          <div className="grow" />
          <div className="stack small">
            <button className="ghost" onClick={() => setShowMembers((s) => !s)} data-testid="toggle-members">
              {showMembers ? "Hide members" : "Members"}
            </button>
            {showMembers && <MembersPanel />}
            <div className="row">
              <span className="avatar" style={{ background: ctx.me.color }} title={did}>
                {ctx.me.name.slice(0, 2).toUpperCase()}
              </span>
              <span className="grow mono muted" title={did}>
                {ctx.me.name}
              </span>
              <button className="ghost small" onClick={() => void auth.signOut()}>
                Sign out
              </button>
            </div>
            <div className="muted">kb {config.version}</div>
          </div>
        </aside>
        <main className="main">
          <div className="main-inner">
            <Outlet />
          </div>
        </main>
      </div>
    </Ctx.Provider>
  );
}

function MembersPanel() {
  const { workspaceId } = useWorkspace();
  const qc = useQueryClient();
  const members = useQuery({
    queryKey: ["members", workspaceId],
    queryFn: () => api.members.list({ workspaceId, pageSize: 100 }),
  });
  const [did, setDid] = useState("");
  const [role, setRole] = useState("editor");
  const [link, setLink] = useState<string | null>(null);
  const invite = useMutation({
    mutationFn: () => api.members.invite({ workspaceId, did: did.trim(), role }),
    onSuccess: (res) => {
      setLink(`${config.publicUrl}/app/invite/${workspaceId}/${res.invite?.id}`);
      setDid("");
      void qc.invalidateQueries({ queryKey: ["members", workspaceId] });
    },
  });
  return (
    <div className="stack small" data-testid="members-panel">
      {members.data?.members.map((m) => (
        <div className="row" key={m.principal?.did}>
          <span className="grow mono" title={m.principal?.did}>
            {displayName(m.principal?.did ?? "", m.principal?.displayName)}
          </span>
          <span className="muted">{m.role}</span>
        </div>
      ))}
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          if (did.trim()) invite.mutate();
        }}
      >
        <input
          type="text"
          placeholder="did:key:… or did:pkh:…"
          value={did}
          onChange={(e) => setDid(e.target.value)}
          data-testid="invite-did"
        />
        <div className="row">
          <select value={role} onChange={(e) => setRole(e.target.value)}>
            <option value="viewer">viewer</option>
            <option value="editor">editor</option>
            <option value="admin">admin</option>
          </select>
          <button type="submit" disabled={invite.isPending || !did.trim()} data-testid="invite-submit">
            Invite
          </button>
        </div>
      </form>
      {invite.error && <Banner kind="error">{errorMessage(invite.error)}</Banner>}
      {link && (
        <div className="stack">
          <span className="muted">Send this link to the invitee (they must sign in with that DID):</span>
          <input
            type="text"
            readOnly
            value={link}
            onFocus={(e) => e.currentTarget.select()}
            data-testid="invite-link"
          />
        </div>
      )}
    </div>
  );
}
