import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { currentRoutingScope, relayNeedsTicket } from '../api';
import {
  browserAccessPath,
  browserAccessUrl,
  browserTileSandbox,
  desktopShellBlocksPreviews,
  framesInThisWindow,
  loadDaemonPreviewOrigin,
  previewOriginsVersion,
  subscribePreviewOrigins,
  systemBrowserUrl,
} from '../lib/portBrowser';
import { debugLog } from '../debug';
import { isEmbedded, openEmbedUrl } from '../embed';
import { useStore } from '../store';
import { openInSystemBrowser } from '../tauri-browser';
import type { BrowserInfo, BrowserServiceTarget } from '../types';
import { DRAG_MIME, computeZone, type DropZone } from '../lib/paneDnd';

// BrowserPane is the preview tile: one sandboxed <iframe> showing a service
// one of the person's panes is running, on every client (desktop, browser,
// phone). The src comes from browserAccessUrl, the same routing the Services
// list uses: the localhost address on this computer, the desktop router's
// preview origin for a backend, or the daemon's /api/proxy through the relay
// (which strips X-Frame-Options and CSP for the page).
//
// In the desktop app the page is framed only on a preview origin of its own
// (the router's or the daemon's, framesInThisWindow), and only in a shell new
// enough to keep that origin out of its capabilities; otherwise the tile
// offers the person's browser instead.
//
// The parent can't read a cross-origin frame, so it can't see a page that
// refuses framing. Instead: a reachability probe before showing it, a load
// timeout, and an "Open in browser" button that is always there. Anything that
// is not a pane's service (an old saved tile on a public site) opens in the
// system browser instead.

/** How long a page may take to fire load before the tile offers the browser. */
export const PREVIEW_LOAD_TIMEOUT_MS = 15_000;

function useServiceBrowserSrc(url: string, service: BrowserServiceTarget | undefined, nonce: number): string | null {
  const path = service ? browserAccessPath(url, service) : null;
  const proxied = path !== null && relayNeedsTicket(path);
  const key = `${currentRoutingScope().key}|${nonce}|${path}`;
  const [minted, setMinted] = useState<{ key: string; src: string | null } | null>(null);
  useEffect(() => {
    if (!proxied) return;
    let cancelled = false;
    browserAccessUrl(url, service).then(src => { if (!cancelled) setMinted({ key, src }); }, err => {
      debugLog('[browser-pane] proxy pass failed', String(err));
      if (!cancelled) setMinted({ key, src: null });
    });
    return () => { cancelled = true; };
  }, [key, proxied, url, service]);
  return proxied ? minted?.key === key ? minted.src : null : path;
}

type Failure = { kind: 'unreachable' | 'timeout'; detail: string };

/**
 * Whether src answers at all. Same-origin (the relay's proxy route) reads the
 * status: the proxy's own refusals (403 unowned listener, 502 dev server down,
 * 503/504 relay) fail; an app's own error page still shows. Cross-origin (a
 * localhost dev server, the router's preview origin) can only tell a network
 * failure from an answer.
 */
export async function probePreview(src: string): Promise<Failure | null> {
  let u: URL;
  try { u = new URL(src, location.href); } catch { return { kind: 'unreachable', detail: 'The preview address is not valid.' }; }
  const host = u.port ? `Port ${u.port}` : u.host;
  try {
    if (u.origin === location.origin) {
      const r = await fetch(u.href, { cache: 'no-store', credentials: 'same-origin' });
      if (r.ok || ![403, 502, 503, 504].includes(r.status)) return null;
      const body = (await r.text().catch(() => '')).split('\n', 1)[0].trim();
      return { kind: 'unreachable', detail: body.slice(0, 200) || `The preview answered ${r.status}.` };
    }
    // Opaque either way; HEAD keeps the second request to the dev server cheap.
    await fetch(u.href, { method: 'HEAD', mode: 'no-cors', cache: 'no-store', credentials: 'omit' });
    return null;
  } catch {
    const label = u.origin === location.origin ? 'The preview' : host;
    return { kind: 'unreachable', detail: `${label} isn’t answering. Is the dev server still running?` };
  }
}

function serviceLabel(raw: string): string {
  return raw.replace(/^https?:\/\//, '').replace(/\/$/, '');
}

export function BrowserPane({ info }: { info: BrowserInfo }) {
  const closeBrowser = useStore((s) => s.closeBrowserInLayout);
  const setFocus = useStore((s) => s.setFocus);
  const focused = useStore((s) => s.focusedPaneId === info.id);
  const movePane = useStore((s) => s.movePaneInLayout);
  const isDraggingPane = useStore((s) => s.isDraggingPane);
  const setDraggingPane = useStore((s) => s.setDraggingPane);

  const [nonce, setNonce] = useState(0); // iframe reload trigger
  const [dropZone, setDropZone] = useState<DropZone | null>(null);
  const [beingDragged, setBeingDragged] = useState(false);
  const [failure, setFailure] = useState<Failure | null>(null);
  const [waitedOut, setWaitedOut] = useState(false); // "Keep waiting" after a timeout
  // Back: the frame's own history is out of reach (cross-origin), but its
  // navigations join the window's session history. A load counts as a step
  // only when that history grew: a reload, an HMR full reload or a
  // location.replace adds no entry, and Back must never step the app itself.
  // A Back we started is answered by one load.
  const [depth, setDepth] = useState(0);
  const loads = useRef(0);
  const goingBack = useRef(false);
  const historyLength = useRef(0);

  // Re-render when a preview origin becomes known (it changes src).
  useSyncExternalStore(subscribePreviewOrigins, previewOriginsVersion);
  const service = info.service;
  const accessPath = service ? browserAccessPath(info.url, service) : null;
  const accessUrl = useServiceBrowserSrc(info.url, service, nonce);
  const frameKey = `${nonce}:${service?.previewPath ?? ''}:${info.url}`;

  // A new page or a reload starts over: no history, no verdict.
  useEffect(() => {
    loads.current = 0;
    goingBack.current = false;
    setDepth(0);
    setFailure(null);
    setWaitedOut(false);
  }, [frameKey]);

  const framable = accessUrl !== null && framesInThisWindow(accessUrl);

  useEffect(() => {
    if (!accessUrl || !framable) return;
    let cancelled = false;
    void probePreview(accessUrl).then((f) => { if (!cancelled && f) setFailure(f); });
    const timer = setTimeout(() => {
      if (!cancelled && loads.current === 0) {
        setFailure((prev) => prev ?? { kind: 'timeout', detail: 'The page has not finished loading. It may refuse to be shown inside SoluTerminal, or still be starting.' });
      }
    }, PREVIEW_LOAD_TIMEOUT_MS);
    return () => { cancelled = true; clearTimeout(timer); };
  }, [accessUrl, framable, frameKey]);

  function onFrameLoad() {
    loads.current += 1;
    const grew = history.length > historyLength.current;
    historyLength.current = history.length;
    if (loads.current === 1) {
      setFailure((prev) => (prev?.kind === 'timeout' ? null : prev));
      return;
    }
    if (goingBack.current) {
      goingBack.current = false;
      setDepth((d) => Math.max(0, d - 1));
    } else if (grew) {
      setDepth((d) => d + 1);
    }
  }

  function goBack() {
    if (depth === 0) return;
    const appUrl = location.href;
    goingBack.current = true;
    history.back();
    // If the step left the frame's history and moved the app instead, undo it.
    window.setTimeout(() => {
      if (location.href !== appUrl) {
        goingBack.current = false;
        setDepth(0);
        history.forward();
      }
    }, 400);
  }

  function reload() {
    if (service) void useStore.getState().refreshPorts();
    // A restarted daemon listens for previews on a new port: ask again.
    void loadDaemonPreviewOrigin();
    setNonce((n) => n + 1);
  }

  function openInBrowser(event: Event) {
    if (isEmbedded()) { openEmbedUrl(info.url, event); return; }
    void systemBrowserUrl(info.url, service, useStore.getState().ports)
      .then((url) => {
        if (url) return openInSystemBrowser(url);
        useStore.getState().toast('This address can’t be opened in a browser.', { kind: 'warn' });
      })
      .catch((err) => {
        debugLog('[browser-pane] open in browser failed', String(err));
        useStore.getState().toast(err instanceof Error ? err.message : 'Could not open the browser.', { kind: 'warn' });
      });
  }

  const label = serviceLabel(info.url);
  const blocked = !service
    ? { title: 'This page opens in your browser', detail: `The preview shows only services your panes are running. ${label}` }
    : accessUrl !== null && !framable
      ? {
          title: 'This preview opens in your browser',
          detail: desktopShellBlocksPreviews()
            ? `Update the SoluTerminal desktop app to preview here. ${label}`
            : `It can’t be shown inside this window. ${label}`,
        }
      : failure?.kind === 'unreachable'
        ? { title: 'Couldn’t load this preview', detail: failure.detail }
        : failure?.kind === 'timeout' && !waitedOut
          ? { title: 'This page is taking a while', detail: failure.detail }
          : null;
  // A refused or failed page is not left mounted behind the message; a slow
  // one keeps loading underneath in case it gets there.
  const showFrame = !!service && accessUrl !== null && framable && failure?.kind !== 'unreachable';

  return (
    <div
      className={`pane browser-pane ${focused ? 'focused' : ''} ${beingDragged ? 'dragging' : ''}`}
      onMouseDownCapture={() => setFocus(info.id)}
      onDragOver={(e) => {
        if (!e.dataTransfer.types.includes(DRAG_MIME)) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = 'move';
        setDropZone(computeZone(e));
      }}
      onDragLeave={(e) => {
        const related = e.relatedTarget as Node | null;
        if (!e.currentTarget.contains(related)) setDropZone(null);
      }}
      onDrop={(e) => {
        if (!e.dataTransfer.types.includes(DRAG_MIME)) return;
        e.preventDefault();
        const src = e.dataTransfer.getData(DRAG_MIME);
        const zone = computeZone(e);
        setDropZone(null);
        if (src && src !== info.id) movePane(src, info.id, zone);
      }}
    >
      <div
        className="pane-header browser-toolbar"
        draggable
        onDragStart={(e) => {
          e.dataTransfer.setData(DRAG_MIME, info.id);
          e.dataTransfer.effectAllowed = 'move';
          setBeingDragged(true);
          setDraggingPane(true);
        }}
        onDragEnd={() => {
          setBeingDragged(false);
          setDraggingPane(false);
        }}
      >
        <span className="activity-dot idle" title="Preview" />
        <button
          type="button"
          className="iconbtn"
          title="Back"
          aria-label="Back"
          disabled={depth === 0}
          onClick={(e) => { e.stopPropagation(); goBack(); }}
        >←</button>
        <button
          type="button"
          className="iconbtn"
          title="Reload"
          aria-label="Reload"
          onClick={(e) => { e.stopPropagation(); reload(); }}
        >⟳</button>
        <span className="browser-service" title={info.url}>{label}</span>
        <button
          type="button"
          className="iconbtn browser-open-external"
          title="Open in your browser"
          disabled={!isEmbedded() && accessPath === null && !!service}
          onClick={(e) => { e.stopPropagation(); openInBrowser(e.nativeEvent); }}
        >↗ Open in browser</button>
        <div className="pane-actions">
          <button
            type="button"
            className="iconbtn"
            title="Close preview"
            aria-label="Close preview"
            onClick={(e) => { e.stopPropagation(); closeBrowser(info.id); }}
          >×</button>
        </div>
      </div>
      <div className="pane-body browser-body">
        {service && accessPath === null && <p role="status" className="port-notice">This service belongs to a different machine. Open it again from Services on that machine.</p>}
        {showFrame && (
          <iframe
            key={frameKey}
            src={accessUrl}
            title={info.title || label}
            sandbox={browserTileSandbox(accessUrl)}
            onLoad={onFrameLoad}
            // A pane drag must reach the tile's drop zones, not the page.
            style={isDraggingPane ? { pointerEvents: 'none' } : undefined}
            hidden={!!blocked}
          />
        )}
        {blocked && (
          <div className="browser-fallback" role="status">
            <p className="browser-fallback-title">{blocked.title}</p>
            <p className="browser-fallback-detail">{blocked.detail}</p>
            <div className="browser-fallback-actions">
              <button type="button" className="port-action" onClick={(e) => openInBrowser(e.nativeEvent)}>Open in browser</button>
              {service && <button type="button" className="port-action" onClick={reload}>Reload</button>}
              {failure?.kind === 'timeout' && (
                <button type="button" className="port-action" onClick={() => setWaitedOut(true)}>Keep waiting</button>
              )}
            </div>
          </div>
        )}
        {dropZone && !beingDragged && (
          <div className={`drop-zone-overlay zone-${dropZone}`} aria-hidden>
            <span className="drop-zone-hint">
              {dropZone === 'center' ? 'swap' : `insert ${dropZone}`}
            </span>
          </div>
        )}
      </div>
    </div>
  );
}
