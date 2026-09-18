import type { SyncStatus } from "@kb/sync-client";
import type { ReactNode } from "react";
import { shortDid } from "../lib/identity";

export function Banner({
  kind = "info",
  children,
}: {
  kind?: "info" | "error" | "warn";
  children: ReactNode;
}) {
  return <div className={`banner ${kind === "info" ? "" : kind}`}>{children}</div>;
}

export function Spinner({ label = "Loading…" }: { label?: string }) {
  return <div className="muted small">{label}</div>;
}

export function colorFor(id: string): string {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  return `hsl(${h % 360} 65% 45%)`;
}

export function displayName(did: string, name?: string): string {
  if (name) return name;
  if (did.startsWith("did:pkh:")) {
    const addr = did.slice(did.lastIndexOf(":") + 1);
    return `${addr.slice(0, 6)}…${addr.slice(-4)}`;
  }
  return shortDid(did);
}

export function initials(name: string): string {
  const words = name
    .replace(/^did:key:/, "")
    .split(/\s+/)
    .filter(Boolean);
  if (words.length >= 2) return (words[0]![0]! + words[1]![0]!).toUpperCase();
  return name.slice(0, 2).toUpperCase();
}

export function Avatar({ name, color, title }: { name: string; color: string; title?: string }) {
  return (
    <span className="avatar" style={{ background: color }} title={title ?? name}>
      {initials(name)}
    </span>
  );
}

export function SyncPill({
  status,
  pending,
  synced,
}: {
  status: SyncStatus;
  pending: number;
  synced: boolean;
}) {
  let cls = "pill";
  let text: string;
  if (status === "connected") {
    if (pending > 0 || !synced) {
      cls += " warn";
      text = pending > 0 ? `Saving ${pending}…` : "Syncing…";
    } else {
      cls += " ok";
      text = "Saved";
    }
  } else if (status === "offline" || status === "closed") {
    cls += " bad";
    text = pending > 0 ? `Offline · ${pending} pending` : "Offline";
  } else {
    text = status === "connecting" ? "Connecting…" : "Idle";
  }
  return (
    <span className={cls} data-testid="sync-pill">
      <span className="dot" /> {text}
    </span>
  );
}
