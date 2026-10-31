import './previewBoot';
import React from 'react';
import ReactDOM from 'react-dom/client';
import { isPreviewBuild } from './previewBoot';
import { isPreviewAcceptanceHost } from './previewAcceptanceShell';
import { loadEnterGapConfig, onRoutingScopeChange, setBackend } from './api';
import { isRouterMode } from './dualBackend';
import { loadRouterPreviewOrigin, ROUTER_RESTART_NOTICE } from './lib/portBrowser';
import { useStore } from './store';
import { installLifecycleDebugGlobal, lifecycleLog } from './debug';
import { installEmbedBridge, isEmbedded } from './embed';
import { captureArrival } from './lib/arrival';
import { standalonePageFor } from './standalonePages';
import { RootErrorBoundary } from './components/RootErrorBoundary';
import './prefs'; // activate cloud-synced user-preferences sync (see prefs.ts)
import './styles.css';

const App = React.lazy(() => import('./App'));
const CloudGate = React.lazy(() =>
  import('./components/CloudGate').then((m) => ({ default: m.CloudGate })),
);
const PopoutBrowser = React.lazy(() =>
  import('./components/PopoutBrowser').then((m) => ({ default: m.PopoutBrowser })),
);


// Popout-window mode. Two signals, in order of preference:
//   (1) URL hash — openExternalWindow puts the popout config in the
//       navigate URL's fragment, which survives every redirect (auth
//       gate, router mode) because browsers re-attach the fragment to
//       the Location target. This is the primary detection path.
//   (2) window.__SOLU_POPOUT__ global — Rust init_script injection.
//       Kept as a fallback for debugging, but some WebView2 configs
//       don't run initialization_script reliably for child windows;
//       relying on it alone leaves the popout stuck showing the raw
//       target URL full-screen with no chrome (the 2026-04-25 bug).
type SoluPopoutConfig = { url: string; title?: string; parentLabel?: string };

function readPopoutFromHash(): SoluPopoutConfig | null {
  const raw = window.location.hash.startsWith('#')
    ? window.location.hash.slice(1)
    : window.location.hash;
  if (!raw) return null;
  const params = new URLSearchParams(raw);
  if (params.get('popout') !== '1') return null;
  const url = params.get('popout_url');
  if (!url) return null;
  return {
    url,
    title: params.get('popout_title') ?? undefined,
    parentLabel: params.get('popout_label') ?? undefined,
  };
}

function readViewFromHash(): 'system' | null {
  const raw = window.location.hash.startsWith('#') ? window.location.hash.slice(1) : window.location.hash;
  if (!raw) return null;
  const view = new URLSearchParams(raw).get('view');
  return view === 'system' ? view : null;
}

const popoutConfig: SoluPopoutConfig | null =
  readPopoutFromHash() ??
  (() => {
    const g = (window as unknown as { __SOLU_POPOUT__?: SoluPopoutConfig }).__SOLU_POPOUT__;
    return g && typeof g.url === 'string' && g.url.length > 0 ? g : null;
  })();
const isPopout = !!popoutConfig;
// A page that replaces the whole app at its own path (standalonePages.ts),
// such as the owner-only /admin/metrics. It does its own sign-in check.
const standalonePage = isPopout ? null : standalonePageFor(window.location.pathname);
const StandalonePage = standalonePage ? React.lazy(standalonePage.load) : null;
// The website's "Get started" and "Sign in" open the hosted app with ?intent= and
// a first-touch tag. Keep them for this tab and clean the address bar before
// anything logs or shares the URL.
if (!isPopout) captureArrival();
const initialView = readViewFromHash();
if (initialView === 'system') useStore.getState().setSystemPageOpen(true);

// Expose the resolved config on window so PopoutBrowser reads one
// source regardless of whether it came from hash or init_script.
if (popoutConfig) {
  (window as unknown as { __SOLU_POPOUT__?: SoluPopoutConfig }).__SOLU_POPOUT__ = popoutConfig;
}

// In dual-backend router mode the webview is served by `solud router`
// and expects API calls under /b/wsl/* or /b/win/*. The `b=<backend>`
// URL param (set by switchActiveBackend when the user clicks the L/W
// toggle) picks which side is active for this page load. Defaults to
// WSL so fresh loads feel the same as before.
if (isRouterMode()) {
  const params = new URLSearchParams(window.location.search);
  const b = params.get('b') === 'win' ? 'win' : 'wsl';
  setBackend(b);
  // A preview opened before this resolves still lands there: the router
  // redirects app-origin preview routes to the preview origin.
  void loadRouterPreviewOrigin().then((status) => {
    if (status === 'old-router') {
      useStore.getState().toast(ROUTER_RESTART_NOTICE, { kind: 'warn', icon: '↻', ttlMs: 0 });
    }
  });
}

// Start daemon timing reads only after preview transport and initial backend
// routing are ready. Reload when a cloud machine or router backend changes.
onRoutingScopeChange(() => { void loadEnterGapConfig(); });
void loadEnterGapConfig();

// Register the service worker once the window has finished loading so it
// doesn't delay first paint. Production only — SW in dev causes stale loads.
//
// Skipped inside Tauri (window.__SOLU_TAURI__ set via Rust init_script —
// see `feedback_tauri_detection.md`). Reasons:
//   * Tauri doesn't need PWA installability — it IS the installed shell.
//   * Tauri doesn't need offline caching — the daemon runs on the same
//     machine, so reachability is effectively 100%.
//   * WebView2 breaks on redirected navigation responses when a SW
//     intercepts them ("a redirected response was used for a request
//     whose redirect mode is not 'follow'"). Our auth gate always
//     redirects on first load to bank the ?token=. Keeping the SW out
//     of Tauri avoids the whole class of WebView2 redirect bugs while
//     letting mobile browsers keep the install-prompt-capable SW.
// If a prior build DID register an SW here, unregister the stale one so
// WebView2 stops intercepting navigations after the upgrade.
const isTauri = !isEmbedded() && !!(window as unknown as { __SOLU_TAURI__?: boolean }).__SOLU_TAURI__;
installEmbedBridge((theme) => {
  const light = theme === 'light';
  useStore.getState().setAppThemeLight(light);
  if (light) document.documentElement.setAttribute('data-theme', 'light');
  else document.documentElement.removeAttribute('data-theme');
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', light ? '#f5f7fa' : '#0b0d10');
});
installLifecycleDebugGlobal();
lifecycleLog('main.boot', {
  href: window.location.href,
  isTauri,
  visibility: document.visibilityState,
  focused: typeof document.hasFocus === 'function' ? document.hasFocus() : null,
});
window.addEventListener('pageshow', (event) => {
  lifecycleLog('window.pageshow', {
    persisted: event.persisted,
    visibility: document.visibilityState,
  });
});
window.addEventListener('pagehide', (event) => {
  lifecycleLog('window.pagehide', {
    persisted: event.persisted,
    visibility: document.visibilityState,
  });
});
window.addEventListener('beforeunload', () => {
  lifecycleLog('window.beforeunload', { href: window.location.href });
});
if ('serviceWorker' in navigator && import.meta.env.PROD && !isTauri && !isEmbedded() && import.meta.env.BASE_URL === '/') {
  window.addEventListener('load', () => {
    navigator.serviceWorker
      .register('/sw.js', { scope: '/' })
      .catch((err) => console.warn('service worker registration failed', err));
  });
  // Auto-apply updates. The SW calls skipWaiting()+clients.claim(), so a
  // newly-published version takes control of this already-open tab —
  // firing `controllerchange`. Without this listener the tab keeps
  // running the OLD bundle until a manual reload, which is exactly how a
  // shipped feature (e.g. a new dialog) appears "missing". Reload once so
  // the fresh bundle is actually used. Guards: skip the FIRST handoff on a
  // first-ever visit (no prior controller → not an update), and a
  // `reloading` flag so we never loop.
  let hadController = !!navigator.serviceWorker.controller;
  let reloading = false;
  // The in-memory `reloading` flag resets on every navigation, so a churning
  // service worker (skipWaiting + clients.claim on each activate) can reload
  // the tab forever — the 2026-09-20 reload storm (~200 req/s to `/`). Bound it
  // ACROSS navigations with a session timestamp: at most one SW-driven reload
  // per minute, so a legitimate update still applies but a loop cannot spin.
  const SW_RELOAD_THROTTLE_MS = 60_000;
  navigator.serviceWorker.addEventListener('controllerchange', () => {
    if (!hadController) { hadController = true; return; }
    if (reloading) return;
    let last = 0;
    try { last = Number(sessionStorage.getItem('solu.swReloadAt') ?? '0'); } catch { /* private mode */ }
    if (Number.isFinite(last) && Date.now() - last < SW_RELOAD_THROTTLE_MS) {
      console.warn('service worker changed again within the reload throttle — not reloading');
      return;
    }
    reloading = true;
    try { sessionStorage.setItem('solu.swReloadAt', String(Date.now())); } catch { /* private mode */ }
    console.info('service worker updated — reloading for new version');
    window.location.reload();
  });
} else if ('serviceWorker' in navigator && isTauri) {
  // One-shot cleanup for installs that registered the SW before we
  // moved to "never in Tauri". Safe to run on every launch — no-op when
  // nothing is registered.
  navigator.serviceWorker.getRegistrations().then((regs) => {
    for (const r of regs) r.unregister().catch(() => { /* ignore */ });
  }).catch(() => { /* ignore */ });
}

// Stash the PWA install prompt at module scope, BEFORE React mounts.
// Chrome fires `beforeinstallprompt` exactly once per page load when
// criteria are met, and if our listener isn't attached by then the
// event is lost. React mount happens milliseconds after this script
// evaluates; Chrome can (and sometimes does) fire the event inside
// that window. We stash it on window so InstallButton can pick it
// up on mount regardless of ordering, and we also dispatch our own
// custom event for any late-mounting listeners.
if (typeof window !== 'undefined' && !isTauri && !isEmbedded()) {
  window.addEventListener('beforeinstallprompt', (e) => {
    e.preventDefault();
    (window as unknown as { __SOLU_INSTALL_PROMPT__?: Event }).__SOLU_INSTALL_PROMPT__ = e;
    window.dispatchEvent(new CustomEvent('solu:install-prompt-ready'));
  });
  window.addEventListener('appinstalled', () => {
    delete (window as unknown as { __SOLU_INSTALL_PROMPT__?: Event }).__SOLU_INSTALL_PROMPT__;
    window.dispatchEvent(new CustomEvent('solu:install-completed'));
  });
}

if (typeof window !== 'undefined') {
  const reloadOnceForFreshShell = () => {
    const key = 'solu.preload-reload-attempted';
    try {
      if (sessionStorage.getItem(key) === '1') return;
      sessionStorage.setItem(key, '1');
    } catch {
      // If sessionStorage is unavailable, still try once for this page.
    }
    lifecycleLog('main.reload.fresh-shell', { href: window.location.href });
    window.location.reload();
  };

  window.addEventListener('vite:preloadError', (event) => {
    event.preventDefault();
    lifecycleLog('main.vite-preload-error', {});
    reloadOnceForFreshShell();
  });
  window.addEventListener('error', (event) => {
    const message = event.message || String(event.error ?? '');
    if (
      message.includes('Failed to fetch dynamically imported module') ||
      message.includes('Importing a module script failed')
    ) {
      event.preventDefault();
      lifecycleLog('main.dynamic-import-error', { message });
      reloadOnceForFreshShell();
    }
  });
}

const rootEl = document.getElementById('root')!;
if (typeof __SOLU_LOCAL_PREVIEW__ !== 'undefined' && __SOLU_LOCAL_PREVIEW__) {
  const notice = document.createElement('div');
  notice.setAttribute('role', 'status');
  notice.textContent = 'Local preview · separate workspace · live edits';
  notice.style.cssText = 'height:28px;box-sizing:border-box;padding:5px 12px;background:#20334d;color:#fff;font:12px system-ui;';
  document.body.insertBefore(notice, rootEl);
  rootEl.style.height = 'calc(100% - 28px)';
}
if (isPreviewBuild && !isPopout && isPreviewAcceptanceHost(window.location.hostname)) {
  // Exercise the candidate's actual UI. A placeholder with similarly named
  // headings cannot supply product acceptance evidence. previewBoot answers
  // reads from capture fixtures and rejects writes before any network call.
  document.documentElement.setAttribute('data-solu-preview-fixture', '1');
  const notice = document.createElement('div');
  notice.setAttribute('role', 'status');
  notice.textContent = 'Isolated preview · sample data · changes disabled';
  notice.style.cssText = 'height:28px;box-sizing:border-box;padding:5px 12px;background:#20334d;color:#fff;font:12px system-ui;';
  document.body.insertBefore(notice, rootEl);
  rootEl.style.height = 'calc(100% - 28px)';
}
// RootErrorBoundary sits outside Suspense so a render crash anywhere (or a
// lazy chunk that fails to load) shows "Something went wrong · Reload"
// instead of a blank window.
ReactDOM.createRoot(rootEl).render(
  <React.StrictMode>
    <RootErrorBoundary>
      <React.Suspense fallback={null}>
        {StandalonePage ? (
          <StandalonePage />
        ) : isPopout ? (
          <PopoutBrowser />

        ) : (
          <CloudGate>
            <App />
          </CloudGate>
        )}
      </React.Suspense>
    </RootErrorBoundary>
  </React.StrictMode>,
);
