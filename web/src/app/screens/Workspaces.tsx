import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api, errorMessage } from "../../lib/api";
import { auth } from "../../lib/auth";
import { Banner, displayName } from "../components";
import { useAuth } from "../hooks";

function slugify(name: string): string {
  return name
    .toLowerCase()
    .normalize("NFKD")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 40);
}

export function Workspaces() {
  const { me } = useAuth();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const create = useMutation({
    mutationFn: async () => api.workspaces.create({ name: name.trim(), slug: slugify(name) || "workspace" }),
    onSuccess: async (res) => {
      await auth.reloadMe();
      const ws = res.workspace?.id;
      const project = res.project?.id;
      if (ws) navigate(project ? `/w/${ws}/p/${project}` : `/w/${ws}`);
    },
  });
  const memberships = me?.memberships ?? [];
  const did = me?.principal?.did ?? "";

  return (
    <div className="center">
      <div className="card stack" style={{ width: "min(640px, 100%)" }}>
        <div className="row">
          <h1 className="grow">Workspaces</h1>
          <button className="ghost small" onClick={() => void auth.signOut()}>
            Sign out
          </button>
        </div>
        <div className="small muted">
          Signed in as{" "}
          <span className="mono" title={did} data-testid="me-did">
            {displayName(did, me?.principal?.displayName)}
          </span>
          <button
            className="ghost small"
            style={{ marginLeft: 8 }}
            onClick={() => void navigator.clipboard?.writeText(did)}
          >
            Copy DID
          </button>
        </div>
        {memberships.length === 0 ? (
          <div className="empty">
            You are not a member of any workspace yet. Create one below or ask for an invite.
          </div>
        ) : (
          <div className="list" data-testid="workspace-list">
            {memberships.map((m) => (
              <Link key={m.workspace?.id} className="item" to={`/w/${m.workspace?.id}`}>
                <span>{m.workspace?.name}</span>
                <span className="meta">{m.role}</span>
              </Link>
            ))}
          </div>
        )}
        <form
          className="row"
          onSubmit={(e) => {
            e.preventDefault();
            if (name.trim()) create.mutate();
          }}
        >
          <input
            type="text"
            placeholder="New workspace name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            data-testid="new-workspace-name"
          />
          <button
            className="primary"
            type="submit"
            disabled={create.isPending || !name.trim()}
            data-testid="new-workspace-submit"
          >
            Create
          </button>
        </form>
        {create.error && <Banner kind="error">{errorMessage(create.error)}</Banner>}
      </div>
    </div>
  );
}
