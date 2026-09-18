import type { DocHandle } from "@kb/sync-client";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { PageFormat } from "../../gen/kb/v1/common_pb";
import { api, errorMessage } from "../../lib/api";
import { readMeta, setMetaTitle } from "../../lib/blocks";
import { Avatar, Banner, Spinner, SyncPill } from "../components";
import { Editor } from "../editor/Editor";
import { useDocVersion, useHandleSync, useSyncStatus } from "../hooks";
import { useWorkspace } from "./WorkspaceShell";

export function PageScreen() {
  const { workspaceId, client, me } = useWorkspace();
  const { projectId = "", pageId = "" } = useParams();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const page = useQuery({
    queryKey: ["page", workspaceId, pageId],
    queryFn: () => api.pages.get({ workspaceId, ref: { case: "pageId", value: pageId } }),
  });
  const docId = page.data?.page?.docId ?? "";
  const [handle, setHandle] = useState<DocHandle | null>(null);
  const [openError, setOpenError] = useState<string | null>(null);
  const [closedReason, setClosedReason] = useState<string | null>(null);

  useEffect(() => {
    if (!docId) return;
    let cancelled = false;
    let opened: DocHandle | null = null;
    client
      .openDoc(docId)
      .then((h) => {
        if (cancelled) {
          void h.close();
          return;
        }
        opened = h;
        setOpenError(null);
        setHandle(h);
      })
      .catch((err: unknown) => setOpenError(err instanceof Error ? err.message : String(err)));
    const offClosed = client.on("closed", (ev) => {
      if (ev.docId === docId) setClosedReason(ev.reason);
    });
    return () => {
      cancelled = true;
      offClosed();
      setHandle(null);
      if (opened) void opened.close();
    };
  }, [client, docId]);

  const trash = useMutation({
    mutationFn: () => api.pages.trash({ workspaceId, pageId }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["pages", workspaceId, projectId] });
      navigate(`/w/${workspaceId}/p/${projectId}`);
    },
  });

  const onNavigate = useCallback(
    async (target: { kind: "page" | "tag"; value: string }) => {
      if (target.kind === "tag") {
        navigate(`/w/${workspaceId}/p/${projectId}?q=${encodeURIComponent(target.value)}`);
        return;
      }
      try {
        const res = await api.pages.list({ workspaceId, projectId, title: target.value, pageSize: 1 });
        let id = res.pages[0]?.id;
        if (!id) {
          const created = await api.pages.create({
            workspaceId,
            projectId,
            title: target.value,
            format: PageFormat.OUTLINER,
          });
          id = created.page?.id;
          void qc.invalidateQueries({ queryKey: ["pages", workspaceId, projectId] });
        }
        if (id) navigate(`/w/${workspaceId}/p/${projectId}/page/${id}`);
      } catch (err) {
        setOpenError(errorMessage(err));
      }
    },
    [navigate, projectId, qc, workspaceId],
  );

  if (page.isLoading) return <Spinner />;
  if (page.error) return <Banner kind="error">{errorMessage(page.error)}</Banner>;
  const p = page.data?.page;
  if (!p) return <Banner kind="error">Page not found.</Banner>;

  return (
    <div className="stack">
      <div className="row small muted">
        <Link to={`/w/${workspaceId}/p/${projectId}${p.key ? "?tab=work" : ""}`}>
          ‹ {p.key ? "Work" : "Documents"}
        </Link>
        <span className="grow" />
        {handle && <HeaderStatus handle={handle} />}
        <button
          className="ghost danger small"
          onClick={() => trash.mutate()}
          disabled={trash.isPending}
          data-testid="trash-page"
        >
          Move to trash
        </button>
      </div>
      {openError && <Banner kind="error">{openError}</Banner>}
      {closedReason && <Banner kind="warn">This page is no longer available here ({closedReason}).</Banner>}
      {trash.error && <Banner kind="error">{errorMessage(trash.error)}</Banner>}
      {handle ? (
        <>
          <div className="page-head">
            {p.key && <span className="key">{p.key}</span>}
            <TitleEditor handle={handle} fallback={p.title} />
          </div>
          <Editor handle={handle} me={me} onNavigate={(t) => void onNavigate(t)} />
        </>
      ) : (
        <Spinner label="Opening the document…" />
      )}
    </div>
  );
}

function HeaderStatus({ handle }: { handle: DocHandle }) {
  const { client } = useWorkspace();
  const status = useSyncStatus(client);
  useHandleSync(handle);
  const [peers, setPeers] = useState(() => [...handle.awareness.peers().entries()]);
  useEffect(() => handle.awareness.subscribe((m) => setPeers([...m.entries()])), [handle]);
  return (
    <>
      <span className="avatars" data-testid="presence">
        {peers.map(([peer, s]) => (
          <Avatar key={peer} name={s.name} color={s.color} />
        ))}
      </span>
      <SyncPill status={status} pending={handle.pendingCount} synced={handle.synced} />
    </>
  );
}

function TitleEditor({ handle, fallback }: { handle: DocHandle; fallback: string }) {
  useDocVersion(handle.doc);
  const docTitle = readMeta(handle.doc).title || fallback;
  const [draft, setDraft] = useState<string | null>(null);
  return (
    <input
      className="page-title"
      value={draft ?? docTitle}
      placeholder="Untitled"
      data-testid="page-title"
      onFocus={() => setDraft(docTitle)}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={() => {
        if (draft !== null) setMetaTitle(handle.doc, draft.trim());
        setDraft(null);
      }}
      onKeyDown={(e) => {
        if (e.key === "Enter") (e.target as HTMLInputElement).blur();
      }}
    />
  );
}
