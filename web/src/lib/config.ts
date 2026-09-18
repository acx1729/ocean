// Runtime configuration. The Go handler (internal/web) replaces the
// <!--KB_CONFIG--> marker in index.html with a <meta name="kb-config"> tag whose
// content is a JSON object; the Vite dev server serves the marker untouched and
// everything falls back to same-origin defaults.

export interface AppConfig {
  /** Public URL of the node (no trailing slash). */
  publicUrl: string;
  /** Absolute WebSocket URL of the sync endpoint. */
  syncUrl: string;
  version: string;
  dev: boolean;
  /** Whether wallet sign-in (did:pkh) is offered. */
  wallets: boolean;
}

interface RawConfig {
  publicUrl?: string;
  syncUrl?: string;
  version?: string;
  dev?: boolean;
  wallets?: boolean;
}

function readRaw(): RawConfig {
  if (typeof document === "undefined") return {};
  const meta = document.querySelector('meta[name="kb-config"]');
  const content = meta?.getAttribute("content");
  if (!content) return {};
  try {
    const parsed: unknown = JSON.parse(content);
    return parsed && typeof parsed === "object" ? (parsed as RawConfig) : {};
  } catch {
    return {};
  }
}

function wsOrigin(httpOrigin: string): string {
  return httpOrigin.replace(/^http/, "ws");
}

export function loadConfig(
  raw: RawConfig = readRaw(),
  loc: Location | null = typeof location !== "undefined" ? location : null,
): AppConfig {
  const origin = loc?.origin ?? "http://localhost";
  const publicUrl = (raw.publicUrl || origin).replace(/\/+$/, "");
  const syncBase = (raw.syncUrl || publicUrl).replace(/\/+$/, "");
  // When the sync endpoint lives on the same host as the page, talk to the page's
  // own origin so dev-server proxies and port forwards keep working.
  let syncUrl: string;
  try {
    const su = new URL(syncBase);
    const pu = new URL(publicUrl);
    syncUrl =
      su.host === pu.host && loc ? `${wsOrigin(loc.origin)}/ws/sync` : `${wsOrigin(su.origin)}/ws/sync`;
  } catch {
    syncUrl = `${wsOrigin(origin)}/ws/sync`;
  }
  return {
    publicUrl,
    syncUrl,
    version: raw.version ?? "dev",
    dev: raw.dev ?? true,
    wallets: raw.wallets ?? true,
  };
}

export const config: AppConfig = loadConfig();
