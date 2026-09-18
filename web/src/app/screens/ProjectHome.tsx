import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Link, Navigate, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { PageFormat, type Page } from "../../gen/kb/v1/common_pb";
import { api, errorMessage } from "../../lib/api";
import { Banner, Spinner } from "../components";
import { useWorkspace } from "./WorkspaceShell";

export function ProjectRedirect() {
  const { workspaceId, projects } = useWorkspace();
  if (projects.length === 0) return <div className="empty">This workspace has no project yet.</div>;
  return <Navigate to={`/w/${workspaceId}/p/${projects[0]!.id}`} replace />;
}

function today(): string {
  const d = new Date();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${d.getFullYear()}-${m}-${day}`;
}

export function ProjectHome() {
  const { workspaceId, projects } = useWorkspace();
  const { projectId = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") === "work" ? "work" : "docs";
  const q = params.get("q") ?? "";
  const navigate = useNavigate();
  const qc = useQueryClient();
  const project = projects.find((p) => p.id === projectId);
  const pages = useQuery({
    queryKey: ["pages", workspaceId, projectId],
    queryFn: () => api.pages.list({ workspaceId, projectId, pageSize: 200 }),
  });
  const types = useQuery({
    queryKey: ["types", workspaceId, projectId],
    queryFn: () => api.schema.listTypes({ workspaceId, projectId, pageSize: 100 }),
    enabled: tab === "work",
  });
  const [title, setTitle] = useState("");

  const create = useMutation({
    mutationFn: async (kind: "page" | "task") => {
      let typeId: string | undefined;
      if (kind === "task") {
        const list =
          types.data?.types ?? (await api.schema.listTypes({ workspaceId, projectId, pageSize: 100 })).types;
        typeId = (list.find((t) => t.name === "task") ?? list.find((t) => t.name === "work"))?.id;
        if (!typeId) throw new Error("this project has no work type");
      }
      return api.pages.create({
        workspaceId,
        projectId,
        title: title.trim() || "Untitled",
        typeId,
        format: PageFormat.OUTLINER,
      });
    },
    onSuccess: (res) => {
      setTitle("");
      void qc.invalidateQueries({ queryKey: ["pages", workspaceId, projectId] });
      if (res.page) navigate(`/w/${workspaceId}/p/${projectId}/page/${res.page.id}`);
    },
  });
  const journal = useMutation({
    mutationFn: () => api.pages.journal({ workspaceId, projectId, date: today() }),
    onSuccess: (res) => {
      void qc.invalidateQueries({ queryKey: ["pages", workspaceId, projectId] });
      if (res.page) navigate(`/w/${workspaceId}/p/${projectId}/page/${res.page.id}`);
    },
  });

  const shown = useMemo(() => {
    const all = pages.data?.pages ?? [];
    const filtered = all.filter((p) => (tab === "work" ? p.key !== "" : p.key === ""));
    const needle = q.trim().toLowerCase();
    const hits = needle
      ? filtered.filter((p) => `${p.key} ${p.title}`.toLowerCase().includes(needle))
      : filtered;
    return [...hits].sort((a, b) => Number(b.updatedAt?.seconds ?? 0n) - Number(a.updatedAt?.seconds ?? 0n));
  }, [pages.data, tab, q]);

  const setTab = (t: "docs" | "work") => {
    const next = new URLSearchParams(params);
    if (t === "work") next.set("tab", "work");
    else next.delete("tab");
    setParams(next, { replace: true });
  };

  return (
    <div className="stack">
      <div className="row">
        <h1 className="grow" data-testid="project-name">
          {project?.name ?? "Project"}
        </h1>
        <button onClick={() => journal.mutate()} disabled={journal.isPending} data-testid="journal-today">
          Today's journal
        </button>
      </div>
      <div className="tabs">
        <button
          className={tab === "docs" ? "active" : ""}
          onClick={() => setTab("docs")}
          data-testid="tab-docs"
        >
          Documents
        </button>
        <button
          className={tab === "work" ? "active" : ""}
          onClick={() => setTab("work")}
          data-testid="tab-work"
        >
          Work
        </button>
      </div>
      <form
        className="row"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate(tab === "work" ? "task" : "page");
        }}
      >
        <input
          type="text"
          placeholder={tab === "work" ? "New task title" : "New page title"}
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          data-testid="new-page-title"
        />
        <button className="primary" type="submit" disabled={create.isPending} data-testid="new-page-submit">
          {tab === "work" ? "New task" : "New page"}
        </button>
      </form>
      <input
        type="search"
        placeholder="Filter by title"
        value={q}
        onChange={(e) => {
          const next = new URLSearchParams(params);
          if (e.target.value) next.set("q", e.target.value);
          else next.delete("q");
          setParams(next, { replace: true });
        }}
      />
      {(create.error || journal.error) && (
        <Banner kind="error">{errorMessage(create.error ?? journal.error)}</Banner>
      )}
      {pages.isLoading && <Spinner />}
      {pages.error && <Banner kind="error">{errorMessage(pages.error)}</Banner>}
      {pages.data && shown.length === 0 && (
        <div className="empty">{tab === "work" ? "No work items yet." : "No pages yet."}</div>
      )}
      <div className="list" data-testid="page-list">
        {shown.map((p) => (
          <PageRow key={p.id} page={p} href={`/w/${workspaceId}/p/${projectId}/page/${p.id}`} />
        ))}
      </div>
    </div>
  );
}

function PageRow({ page, href }: { page: Page; href: string }) {
  return (
    <Link className="item" to={href} data-testid="page-row">
      {page.key && <span className="key">{page.key}</span>}
      {page.icon && <span>{page.icon}</span>}
      <span>{page.title || "Untitled"}</span>
      {page.journalDate && <span className="pill">journal</span>}
      <span className="meta">v{String(page.version)}</span>
    </Link>
  );
}
