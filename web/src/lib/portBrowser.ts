import {
  currentApiPrefix,
  currentPrefix,
  currentRoutingScope,
  mintRelayTicket,
  relayNeedsTicket,
} from '../api';
import { isDesktopRouterMode } from '../dualBackend';
import { getShellVersion, isDesktopApp } from '../tauri-browser';
import type { BrowserServiceTarget, Port } from '../types';
import { isWebService, portUrl } from './ports';
import { withQueryParam } from './ticketedEventSource';

// The desktop router serves previews on an origin of its own
// (/router/preview-origin), so a pane's page neither runs as the app nor needs
// the sandbox that would strip its scripts and data. Null until known, and on
// a router too old to have one (previews then stay on the app's origin).
let routerPreviewOrigin: string | null = null;

// Without a router, the daemon serves previews on an origin of its own too
// (daemon/preview_listener.go, GET /api/preview-origin): its origin and the
// /b/<id> route there. The desktop app frames previews only there.
let daemonPreview: { origin: string; prefix: string } | null = null;

const LOOPBACK_ORIGIN = /^http:\/\/(127\.0\.0\.1|localhost|\[::1\]):\d+$/;

// Tiles re-render when a preview origin becomes known (usePreviewOrigins).
const previewOriginListeners = new Set<() => void>();
let previewOriginVersion = 0;
function previewOriginsChanged() {
  previewOriginVersion += 1;
  for (const listener of previewOriginListeners) listener();
}
export function subscribePreviewOrigins(listener: () => void): () => void {
  previewOriginListeners.add(listener);
  return () => previewOriginListeners.delete(listener);
}
export function previewOriginsVersion(): number {
  return previewOriginVersion;
}

/** Ask this daemon for its preview origin (desktop app, no router, no relay). */
export async function loadDaemonPreviewOrigin(): Promise<boolean> {
  if (!isDesktopApp() || isDesktopRouterMode() || currentPrefix()) return false;
  try {
    const r = await fetch(`${currentApiPrefix()}/api/preview-origin`, { cache: 'no-store' });
    if (!r.ok) return false;
    const { origin, prefix } = (await r.json()) as { origin?: unknown; prefix?: unknown };
    if (typeof origin !== 'string' || !LOOPBACK_ORIGIN.test(origin) || origin === location.origin) return false;
    if (typeof prefix !== 'string' || !/^\/b\/(wsl|win|host)$/.test(prefix)) return false;
    if (daemonPreview?.origin !== origin || daemonPreview.prefix !== prefix) {
      daemonPreview = { origin, prefix };
      previewOriginsChanged();
    }
    return true;
  } catch {
    return false;
  }
}

/** Test hook. */
export function setDaemonPreviewOrigin(value: { origin: string; prefix: string } | null): void {
  daemonPreview = value;
  previewOriginsChanged();
}

// The first desktop shell whose capabilities name only the app's exact
// origins. Older shells grant their commands to any 127.0.0.1 origin, which
// would include a framed preview's own origin, so they never frame one.
const PREVIEW_SHELL_MIN = [0, 0, 1241] as const;

// Whether this desktop shell may frame previews: null until its version is read.
let shellFramesPreviews: boolean | null = null;

/** Whether a shell version string is PREVIEW_SHELL_MIN or newer (unreadable: no). */
export function shellVersionFramesPreviews(version: string | null | undefined): boolean {
  const m = /^(\d+)\.(\d+)\.(\d+)/.exec(version?.trim() ?? '');
  if (!m) return false;
  for (let i = 0; i < 3; i++) {
    const part = Number(m[i + 1]);
    if (part !== PREVIEW_SHELL_MIN[i]) return part > PREVIEW_SHELL_MIN[i];
  }
  return true;
}

/** Read the desktop shell's version once (no-op outside the desktop app). */
export async function loadShellPreviewGate(): Promise<void> {
  if (!isDesktopApp()) return;
  const version = await getShellVersion().catch(() => null);
  shellFramesPreviews = shellVersionFramesPreviews(version);
  previewOriginsChanged();
}

/** Test hook. */
export function setShellPreviewGate(allowed: boolean | null): void {
  shellFramesPreviews = allowed;
  previewOriginsChanged();
}

/** In the desktop app, whether this shell is too old (or unread) to frame previews. */
export function desktopShellBlocksPreviews(): boolean {
  return isDesktopApp() && shellFramesPreviews !== true;
}

/**
 * Whether the desktop app may frame src: only on a preview origin of its own
 * (the router's or the daemon's), never the app's or a dev server's, and only
 * in a shell new enough that such an origin is outside its capabilities.
 * Browser tabs and phones frame as before.
 */
export function framesInThisWindow(src: string): boolean {
  if (!isDesktopApp()) return true;
  if (shellFramesPreviews !== true) return false;
  let origin: string;
  try { origin = new URL(src, location.href).origin; } catch { return false; }
  if (origin === location.origin) return false;
  return origin === routerPreviewOrigin || origin === daemonPreview?.origin;
}

/**
 * What /router/preview-origin said: 'ready' (previews have their own origin),
 * 'old-router' (a router from before 0.0.1224, still running after an in-app
 * update until the app restarts: its app shell answers with the page), or
 * 'unknown' (not router mode, router unreachable, or no preview port).
 */
export type RouterPreviewStatus = 'ready' | 'old-router' | 'unknown';

export async function loadRouterPreviewOrigin(): Promise<RouterPreviewStatus> {
  if (!isDesktopRouterMode()) return 'unknown';
  let r: Response;
  try {
    r = await fetch('/router/preview-origin', { cache: 'no-store' });
  } catch {
    return 'unknown'; // router down: not a restart problem
  }
  if (!r.ok) return 'unknown';
  if (!(r.headers.get('content-type') ?? '').includes('application/json')) return 'old-router';
  try {
    const origin = ((await r.json()) as { origin?: unknown }).origin;
    if (typeof origin === 'string' && LOOPBACK_ORIGIN.test(origin)) {
      routerPreviewOrigin = origin;
      previewOriginsChanged();
      return 'ready';
    }
  } catch {
    // unreadable answer
  }
  return 'unknown';
}

/** Shown while a router older than this web app keeps running after an update. */
export const ROUTER_RESTART_NOTICE =
  'Restart SoluTerminal to finish this update. Right-click the SoluTerminal icon in the taskbar tray (it may be under '
  + 'the ^ arrow), choose Quit, then open SoluTerminal again. Your agents keep running, and their panes come back when '
  + 'you reopen it.';

/** Test hook. */
export function setRouterPreviewOrigin(origin: string | null): void {
  routerPreviewOrigin = origin;
  previewOriginsChanged();
}

export function serviceTarget(p: Port): BrowserServiceTarget {
  return {
    url: portUrl(p),
    previewPath: p.paneId ? p.previewPath : undefined,
    backend: p.backend,
    machinePrefix: currentPrefix(),
    paneId: p.paneId,
    port: p.port,
    addr: p.addr,
    proto: p.proto,
  };
}

// Keep credentials in the proxy URL only. Callers retain the friendly URL in
// workspace data and build a fresh access URL whenever a browser is opened.
export function browserAccessPath(raw: string, service?: BrowserServiceTarget): string | null {
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return raw;
  }
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password) return raw;
  const local = ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname);
  let matchesService = false;
  try {
    matchesService = !!service && url.origin === new URL(service.url).origin;
  } catch {
    return null;
  }
  if (!matchesService && !local) return raw;
  if (service && service.machinePrefix !== currentPrefix()) return null;
  // A pane's service on a desktop without a router: the daemon's preview origin.
  const ownOrigin = matchesService && !currentPrefix() && !service?.backend && isDesktopApp() ? daemonPreview : null;
  const prefix = service?.backend ? `${currentPrefix()}/b/${service.backend}` : currentApiPrefix();
  if (!currentPrefix() && !service?.backend && !ownOrigin) return raw;
  const path =
    matchesService &&
    service?.previewPath &&
    /^\/api\/proxy\/\d+~[A-Za-z0-9_-]+\/$/.test(service.previewPath)
      ? service.previewPath.replace(/\/$/, '')
      : `/api/proxy/${url.port || (url.protocol === 'https:' ? '443' : '80')}`;
  const params = new URLSearchParams(url.search);
  // Never carry a pasted sign-in token or an old proxy pass into a new URL.
  params.delete('access_token');
  const query = params.toString();
  if (ownOrigin) return `${ownOrigin.origin}${ownOrigin.prefix}${path}${url.pathname}${query ? `?${query}` : ''}${url.hash}`;
  const origin = !currentPrefix() && service?.backend && routerPreviewOrigin ? routerPreviewOrigin : '';
  return `${origin}${prefix}${path}${url.pathname}${query ? `?${query}` : ''}${url.hash}`;
}

// A proxy pass is scoped to one machine, backend and exact listener. Sign-in
// tokens remain in the ticket-mint Authorization header, never in page URLs.
export async function browserAccessUrl(
  raw: string,
  service?: BrowserServiceTarget,
): Promise<string | null> {
  const scope = currentRoutingScope().key;
  const path = browserAccessPath(raw, service);
  if (path === null || !relayNeedsTicket(path)) return path;
  const match = path.match(/^(\/m\/[^/]+(?:\/b\/[^/]+)?\/api\/proxy\/[^/]+)\//);
  if (!match) return path;
  const pass = await mintRelayTicket(match[1] + '/', 'proxy');
  if (scope !== currentRoutingScope().key) return null;
  return withQueryParam(path, 'access_token', pass);
}

/**
 * The browser tile's iframe sandbox for src. A page on the app's own origin
 * (a pane's server through /api/proxy, or anything else the daemon or cloud
 * serves) must not keep that origin: it could read the app's storage and its
 * requests would pass as the person's. Other origins (a localhost dev server)
 * keep their own, so their cookies and storage work.
 */
export function browserTileSandbox(src: string): string {
  const base = 'allow-forms allow-modals allow-popups allow-scripts';
  let sameOrigin = true;
  try {
    const u = new URL(src, location.href);
    // The desktop router's /b/<backend> previews keep same-origin until they
    // get an origin of their own (the follow-up); sandboxed, they lose their
    // scripts and data.
    sameOrigin = u.origin === location.origin && !u.pathname.startsWith('/b/');
  } catch {
    // unparseable: treat as the app's own
  }
  return sameOrigin ? base : `${base} allow-same-origin`;
}

/** An absolute http(s) URL without credentials: one a system browser may open. */
export function isPlainWebUrl(raw: string): boolean {
  try {
    const url = new URL(raw);
    return (url.protocol === 'http:' || url.protocol === 'https:') && !url.username && !url.password;
  } catch {
    return false;
  }
}

/**
 * What "Open in browser" hands the person's own browser for raw. On this
 * computer that is the address itself (the browser reaches the dev server the
 * way the person would). Through the relay (a phone, another computer) a
 * localhost address means nothing, so it is the proxy URL with a fresh pass
 * scoped to that one service. Null when raw is not a plain web URL or the
 * service belongs to another machine.
 *
 * On a dual-backend desktop the person's browser reaches a port the Windows
 * side listens on before WSL's forwarding, so a WSL service whose port a
 * Windows listener also uses (ports, the current list) can't be opened by its
 * address: that throws with the reason instead of opening the other app.
 */
export async function systemBrowserUrl(
  raw: string,
  service?: BrowserServiceTarget,
  ports: readonly Port[] = [],
): Promise<string | null> {
  if (!isPlainWebUrl(raw)) return null;
  if (!currentPrefix()) {
    if (service?.backend && service.backend !== 'win') {
      // Any Windows listener takes the port, a web service or not (discovery
      // never probes an unowned one, so it may read as unverified).
      const shadow = ports.find((p) => p.backend === 'win' && p.port === service.port);
      if (shadow) {
        throw new Error(
          `A Windows app also uses port ${service.port}, so your browser would open that one instead. ` +
          'Use Open preview, or stop one of them.',
        );
      }
    }
    return raw;
  }
  const access = await browserAccessUrl(raw, service);
  return access ? new URL(access, location.href).href : null;
}

/**
 * The service an agent's open-browser event names, or null. Current daemons
 * send the matched port (open_browser accepts only a pane's listed service);
 * an older daemon sends only a URL, which counts only when it is a pane's web
 * service known here on the event's backend.
 */
export function agentPreviewPort(
  url: string,
  sent: Port | undefined,
  ports: readonly Port[],
  backend: Port['backend'],
): Port | null {
  if (sent) return { ...sent, backend };
  let origin: string;
  try { origin = new URL(url).origin; } catch { return null; }
  return ports.find((p) => {
    try {
      return !!p.paneId && isWebService(p) && p.backend === backend && new URL(portUrl(p)).origin === origin;
    } catch {
      return false;
    }
  }) ?? null;
}
