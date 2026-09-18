import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useEffect } from "react";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { auth } from "../lib/auth";
import { Spinner } from "./components";
import { useAuth } from "./hooks";
import { AcceptInvite } from "./screens/AcceptInvite";
import { PageScreen } from "./screens/PageScreen";
import { ProjectHome, ProjectRedirect } from "./screens/ProjectHome";
import { SignIn } from "./screens/SignIn";
import { Workspaces } from "./screens/Workspaces";
import { WorkspaceShell } from "./screens/WorkspaceShell";

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: 1, staleTime: 5_000, refetchOnWindowFocus: false } },
});

export function App() {
  useEffect(() => {
    void auth.bootstrap();
  }, []);
  return (
    <QueryClientProvider client={queryClient}>
      <BrowserRouter basename="/app">
        <Root />
      </BrowserRouter>
    </QueryClientProvider>
  );
}

function Root() {
  const state = useAuth();
  if (state.status === "loading") {
    return (
      <div className="center">
        <Spinner label="Signing in…" />
      </div>
    );
  }
  return (
    <Routes>
      <Route path="/invite/:wsId/:inviteId" element={<AcceptInvite />} />
      {state.status === "signed_out" ? (
        <Route path="*" element={<SignIn />} />
      ) : (
        <>
          <Route index element={<Workspaces />} />
          <Route path="w/:wsId" element={<WorkspaceShell />}>
            <Route index element={<ProjectRedirect />} />
            <Route path="p/:projectId" element={<ProjectHome />} />
            <Route path="p/:projectId/page/:pageId" element={<PageScreen />} />
          </Route>
          <Route path="*" element={<Navigate to="/" replace />} />
        </>
      )}
    </Routes>
  );
}
