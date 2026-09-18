// Connect clients for the kb.v1 services, same-origin under /rpc. The
// interceptor attaches the access token and the web-client marker (which makes
// AuthService manage the refresh cookie) and retries a unary call once after a
// successful refresh when the server answers Unauthenticated.

import { Code, ConnectError, createClient, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { AgentsService, AuthService } from "../gen/kb/v1/auth_pb";
import { BlocksService } from "../gen/kb/v1/blocks_pb";
import { PagesService } from "../gen/kb/v1/pages_pb";
import { SchemaService } from "../gen/kb/v1/schema_pb";
import { MembersService, ProjectsService, WorkspacesService } from "../gen/kb/v1/workspaces_pb";
import { session } from "./session";

const authInterceptor: Interceptor = (next) => async (req) => {
  req.header.set("X-KB-Client", "web");
  const token = session.token();
  if (token) req.header.set("Authorization", `Bearer ${token}`);
  try {
    return await next(req);
  } catch (err) {
    const isAuthRPC = req.service.typeName === AuthService.typeName;
    if (
      err instanceof ConnectError &&
      err.code === Code.Unauthenticated &&
      !req.stream &&
      !isAuthRPC &&
      token
    ) {
      if (await session.refresh()) {
        const fresh = session.token();
        if (fresh) req.header.set("Authorization", `Bearer ${fresh}`);
        return await next(req);
      }
    }
    throw err;
  }
};

export const transport = createConnectTransport({
  baseUrl: "/rpc",
  useBinaryFormat: true,
  interceptors: [authInterceptor],
});

export const api = {
  auth: createClient(AuthService, transport),
  agents: createClient(AgentsService, transport),
  workspaces: createClient(WorkspacesService, transport),
  projects: createClient(ProjectsService, transport),
  members: createClient(MembersService, transport),
  pages: createClient(PagesService, transport),
  blocks: createClient(BlocksService, transport),
  schema: createClient(SchemaService, transport),
};

/** A short, user-facing message for any thrown value. */
export function errorMessage(err: unknown): string {
  if (err instanceof ConnectError) {
    const detail = err.rawMessage || err.message;
    switch (err.code) {
      case Code.Unauthenticated:
        return "You are signed out. Sign in again.";
      case Code.PermissionDenied:
        return `Not allowed: ${detail}`;
      case Code.NotFound:
        return `Not found: ${detail}`;
      case Code.Unavailable:
        return "The server is unreachable.";
      default:
        return detail;
    }
  }
  if (err instanceof Error) return err.message;
  return String(err);
}

export function errorCode(err: unknown): Code | null {
  return err instanceof ConnectError ? err.code : null;
}
