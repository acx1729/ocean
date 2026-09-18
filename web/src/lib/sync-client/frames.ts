// Binary framing for /ws/sync: every WebSocket message is one protobuf-encoded
// kb.v1.SyncFrame. int64 fields are bigint on the wire types; the client works
// with numbers (seqs stay far below 2^53).

import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import {
  AwarenessSchema,
  CloseSchema,
  HelloSchema,
  OpenRequestSchema,
  PullRequestSchema,
  PushRequestSchema,
  SyncFrameSchema,
  type SyncFrame,
} from "../../gen/kb/v1/sync_pb";

export type FrameKind = SyncFrame["kind"];

export interface ClientUpdateInit {
  clientSeq: number;
  update: Uint8Array;
}

export function encodeFrame(kind: FrameKind): Uint8Array {
  return toBinary(SyncFrameSchema, create(SyncFrameSchema, { kind }));
}

export function decodeFrame(bytes: Uint8Array): SyncFrame {
  return fromBinary(SyncFrameSchema, bytes);
}

export const frames = {
  hello(accessToken: string, clientId: string, workspaceId: string): Uint8Array {
    return encodeFrame({ case: "hello", value: create(HelloSchema, { accessToken, clientId, workspaceId }) });
  },
  open(docId: string, sinceSeq: number, workspaceId: string): Uint8Array {
    return encodeFrame({
      case: "open",
      value: create(OpenRequestSchema, { docId, sinceSeq: BigInt(sinceSeq), workspaceId }),
    });
  },
  push(docId: string, clientId: string, workspaceId: string, updates: ClientUpdateInit[]): Uint8Array {
    return encodeFrame({
      case: "push",
      value: create(PushRequestSchema, {
        docId,
        clientId,
        workspaceId,
        updates: updates.map((u) => ({ clientSeq: BigInt(u.clientSeq), update: u.update })),
      }),
    });
  },
  pull(docId: string, sinceSeq: number, workspaceId: string): Uint8Array {
    return encodeFrame({
      case: "pull",
      value: create(PullRequestSchema, { docId, sinceSeq: BigInt(sinceSeq), workspaceId }),
    });
  },
  awareness(docId: string, state: Uint8Array, peer: string): Uint8Array {
    return encodeFrame({ case: "awareness", value: create(AwarenessSchema, { docId, state, peer }) });
  },
  close(docId: string, reason: string): Uint8Array {
    return encodeFrame({ case: "close", value: create(CloseSchema, { docId, reason }) });
  },
};

export function seq(v: bigint | number): number {
  return typeof v === "bigint" ? Number(v) : v;
}
