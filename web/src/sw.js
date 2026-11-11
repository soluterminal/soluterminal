// Minimal service worker for soluterminal.
//
// Purpose (v0):
//   1. Meets the "installable PWA" requirement on iOS/Android so the app can be
//      added to the home screen and use Notifications.
//   2. Caches the app shell (index.html + hashed JS/CSS) so a cold load works
//      even when the daemon is briefly unreachable — the UI still renders and
//      tells you the daemon is down, instead of a raw browser error page.
//
// What it intentionally does NOT do:
//   - Cache API responses. All /api/* traffic is always network-first, because
//     pane state must never be stale.
//   - Cache WebSockets. SWs can't, and we wouldn't want to.
//
// Bump this when the service worker logic itself changes, OR when the
// app bundle has a bug we need to force clients off of (the shell cache
// holds index.html and thus references to the hashed JS filename — if a
// client is stuck on an old index.html, bumping this invalidates their
// shell and forces a fresh pull).
// v29: force browser/PWA clients onto the terminal reader-mode autoscroll fix.
// v30: the pane-mark app icon; icons are served stale-while-revalidate from
//      this cache, so without a bump the old 2×2 grid lingers for a load.
// v31: a tapped feedback notification reaches an already-open window.
const SW_VERSION = 'solu-sw-v31';
const APP_SHELL_CACHE = 'solu-shell-' + SW_VERSION;
const NAVIGATION_NETWORK_TIMEOUT_MS = 10000;

// Deliberately omits `/index.html`. The cloud's Go file server 301-
// redirects /index.html to / (standard FileServer behavior). cache.addAll
// follows the redirect and gets a 200, but the resulting Response has
// `response.redirected === true`, and cache.put() throws on redirected
// responses. That made addAll reject → SW install failed → the previous
// SW stayed in control → Chrome's installability check failed → no
// install prompt. The 2026-04-21 PWA regression was exactly this.
// `/` alone is enough — it IS the canonical entry and is what start_url
// points to.
const BUILD_SHELL_URLS = self.__SOLU_BUILD_SHELL_URLS__ || [];
const SHELL_URLS = [
  '/',
  '/manifest.webmanifest',
  '/icon-192.png',
  '/icon-512.png',
  '/icon-mark-192.png',
  '/icon-maskable-192.png',
  '/icon-maskable-512.png',
  '/apple-touch-icon.png',
  '/favicon.svg',
  ...BUILD_SHELL_URLS,
];

function fetchWithTimeout(input, init, timeoutMs) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('network timeout')), timeoutMs);
    fetch(input, init).then(
      (res) => {
        clearTimeout(timer);
        resolve(res);
      },
      (err) => {
        clearTimeout(timer);
        reject(err);
      },
    );
  });
}

function isRuntimeCacheablePath(pathname) {
  return pathname === '/manifest.webmanifest' ||
    pathname === '/icon-192.png' ||
    pathname === '/icon-512.png' ||
    pathname === '/icon-mark-192.png' ||
    pathname === '/icon-maskable-192.png' ||
    pathname === '/icon-maskable-512.png' ||
    pathname === '/apple-touch-icon.png' ||
    pathname === '/favicon.svg' ||
    pathname.startsWith('/assets/');
}

self.addEventListener('install', (event) => {
  // Per-URL fetch + cache.put with individual try/catch so a single
  // redirect / 404 / server hiccup can't kill the whole install. Before
  // this, we used cache.addAll which is all-or-nothing: one bad URL
  // meant the new SW never activated. If EVERY URL fails we still want
  // the install to fail loudly (via the rejected waitUntil), hence the
  // counter.
  event.waitUntil((async () => {
    const cache = await caches.open(APP_SHELL_CACHE);
    let stored = 0;
    for (const u of SHELL_URLS) {
      try {
        const res = await fetch(u, { cache: 'reload', credentials: 'same-origin' });
        // Reject redirects explicitly so a future misconfiguration
        // shows up as a console warning we can diagnose, rather than
        // silently retaining a broken shell.
        if (!res.ok || res.redirected) {
          console.warn('[sw] skipping shell entry', u, 'status=', res.status, 'redirected=', res.redirected);
          continue;
        }
        await cache.put(u, res);
        stored++;
      } catch (err) {
        console.warn('[sw] failed to cache shell entry', u, err);
      }
    }
    if (stored === 0) {
      throw new Error('sw install: no shell entries cached — all fetches failed');
    }
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    // Drop stale app-shell caches from prior SW versions.
    const keys = await caches.keys();
    await Promise.all(
      keys
        .filter((k) => k.startsWith('solu-shell-') && k !== APP_SHELL_CACHE)
        .map((k) => caches.delete(k))
    );
    await self.clients.claim();

    // Notify every live client that a new SW version just activated.
    // When the SW version changes, the browser can invalidate push
    // subscriptions the prior SW held; the client listens for this
    // message and re-POSTs its subscription to the cloud so push
    // delivery survives frontend deploys without a manual bell toggle.
    const clients = await self.clients.matchAll({ includeUncontrolled: true });
    for (const c of clients) {
      try { c.postMessage({ type: 'solu:sw-activated', version: SW_VERSION }); } catch { /* best effort */ }
    }
  })());
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  const url = new URL(req.url);

  // Only handle same-origin GET requests.
  if (req.method !== 'GET' || url.origin !== self.location.origin) return;

  // Never cache API or WS upgrades — must be live.
  if (url.pathname.startsWith('/api/')) return;

  // Never intercept machine-proxy traffic. `/m/<id>/...` is the cloud
  // relay path (both our own /api/* and the /api/proxy/<port>/* reverse
  // proxy into dev servers). Letting the SW wrap these triggered two
  // bugs: (a) the navigate-mode catch returned undefined when no
  // cached index existed → "Failed to convert value to 'Response'",
  // and (b) credentials:'include' mangled auth in a way that made
  // /api/proxy/ 401. Bypassing fixes both.
  if (url.pathname.startsWith('/m/')) return;

  // Never intercept dual-backend router traffic. `/b/<bk>/...` is the
  // Tauri router's path-prefix routing into per-backend daemons — all
  // API/WS/SSE calls the UI makes in router mode start with this.
  // Same two failure modes as /m/: the stale-while-revalidate path
  // returned undefined on cache-miss + fetch-fail, producing a flood
  // of "Failed to convert value to 'Response'" errors, and the
  // SSE endpoint /b/win/api/ui/events got stuck in a tight
  // reconnect loop that exhausted the browser connection pool and
  // starved unrelated requests (mic transcription, workspace fetches)
  // with ERR_INSUFFICIENT_RESOURCES.
  if (url.pathname.startsWith('/b/')) return;

  // Navigation requests → try the network first, fall back to the
  // cached shell only when the network actually fails. This is the
  // condition Android Chrome's PWA installability heuristic requires
  // for the "Install app" prompt to appear: the SW must be able to
  // produce a valid response for a navigation when offline. Bypassing
  // navigations entirely (as v8 did) flunks the check and the install
  // prompt never surfaces.
  //
  // History: v7 intercepted via `fetch(url.href, { credentials: 'include' })`
  // to serve the cached index on daemon-down. That tripped a WebView2
  // bug when our auth-gate redirect fired on the initial nav ("a
  // redirected response was used for a request whose redirect mode is
  // not 'follow'"). v8 removed the intercept entirely — which fixed
  // WebView2 but killed Android installability.
  //
  // v10 compromise: register this SW only in non-Tauri browsers (see
  // main.tsx) so WebView2 never runs it, but keep the network-first
  // navigation path for mobile PWAs. We use a fresh `fetch(url.href)`
  // (not the original Request) so the default `redirect: 'follow'`
  // applies — avoids the manual-redirect "response was used" error on
  // any Chromium that DOES end up running us.
  if (req.mode === 'navigate') {
    event.respondWith((async () => {
      try {
        return await fetchWithTimeout(
          url.href,
          { credentials: 'include' },
          NAVIGATION_NETWORK_TIMEOUT_MS,
        );
      } catch {
        const cached = await caches.match('/');
        if (cached) return cached;
        return new Response(
          '<!doctype html><meta charset="utf-8"><title>SoluTerminal offline</title>' +
          '<body style="font-family:system-ui;padding:24px">' +
          '<h1>SoluTerminal is offline</h1>' +
          '<p>Couldn\'t reach the daemon, and no cached shell is available. Retry once the daemon is back.</p>' +
          '</body>',
          { status: 503, headers: { 'Content-Type': 'text/html; charset=utf-8' } },
        );
      }
    })());
    return;
  }

  if (!isRuntimeCacheablePath(url.pathname)) return;

  // Static shell assets (JS/CSS/icons/manifest) → stale-while-revalidate.
  //
  // Defensive: `.catch` must always return a real Response, never
  // `cached` (which is undefined on cache-miss). Returning undefined
  // to respondWith throws "Failed to convert value to 'Response'" and
  // the caller sees a network error — for SSE/EventSource that turns
  // into a reconnect-storm, starving the whole connection pool.
  event.respondWith(
    caches.match(req).then((cached) => {
      const fetchPromise = fetch(req).then((res) => {
        if (res && res.status === 200 && res.type === 'basic') {
          const clone = res.clone();
          caches.open(APP_SHELL_CACHE)
            .then((cache) => cache.put(req, clone))
            .catch((err) => console.warn('[sw] runtime cache put failed', url.pathname, err));
        }
        return res;
      }).catch(() => cached || new Response('offline', { status: 503, statusText: 'Service Unavailable' }));
      return cached || fetchPromise;
    })
  );
});

// Pane title values that give zero disambiguation. For these we'd
// rather show the cwd instead — seven panes all titled "shell" is
// the motivating example. A user who has renamed their pane
// ("soluterminal (claude)" etc.) gets the title; a default-titled
// pane gets its cwd.
const GENERIC_TITLES = new Set([
  'shell', 'bash', 'zsh', 'sh', 'fish', 'pwsh', 'cmd', 'cmd.exe',
  '/bin/bash', '/bin/sh', '/bin/zsh', '/usr/bin/fish',
]);

function abbreviateCwd(cwd) {
  if (!cwd) return '';
  // ~ substitution for common Linux/macOS homes + WSL paths.
  return cwd.replace(/^\/home\/[^/]+/, '~').replace(/^\/Users\/[^/]+/, '~');
}

// Web Push — shown when Claude transitions to a state that needs
// user attention. Payload shape (from cloud/push.go handleNotify):
//   { machineId, machineName, paneId, state, title, workspaceName, cwd,
//     toolName, summary, reason }
// workspaceName/cwd added in sw v14 so the body can disambiguate
// between many panes sharing the default "shell" title.
//
// Format (sw v26, the shared cross-surface grammar — mirrored by the
// cloud's FCM composition in cloud/fcm.go fcmDisplay and the in-app
// toasts in App.tsx, so all surfaces speak the same way):
//   title: "<workspace> · <pane>"  — WHERE. The scannable key for a user
//                                    with many panes; workspace omitted
//                                    when the pane isn't in a named one.
//   body:  "<event>[ — <detail>]"  — WHAT. Event phrases are shared
//                                    ("Approval needed", "Claude is
//                                    waiting for you", "Claude finished").
//                                    detail = the approval's tool+args
//                                    ("Bash: git push …") or the daemon's
//                                    attention reason ("won't auto-approve
//                                    — touches secrets").
// Workflow lifecycle pushes use the schedule-like slot mapping: title is
// the daemon-composed heading and summary is the notification body.
//          "on <machine>"          — second body line, only when the
//                                    machine name is known (multi-machine
//                                    disambiguation without eating title
//                                    space).
self.addEventListener('push', (event) => {
  let payload = {};
  try { payload = event.data ? event.data.json() : {}; } catch { /* keep empty */ }

  const rawTitle = (payload.title || '').trim();
  const cwd = abbreviateCwd(payload.cwd || '');
  const workspace = (payload.workspaceName || '').trim();
  const machineLine = payload.machineName ? `\non ${payload.machineName}` : '';

  // Pane "display" string: prefer the user-set title, but fall back to
  // the cwd when the title is a generic shell name. Keeps the title
  // informative for "shell" panes while respecting custom names.
  let paneDisplay;
  if (rawTitle && !GENERIC_TITLES.has(rawTitle.toLowerCase())) {
    paneDisplay = rawTitle;
  } else if (cwd) {
    paneDisplay = cwd;
  } else {
    paneDisplay = rawTitle || 'a pane';
  }
  const location = workspace ? `${workspace} · ${paneDisplay}` : paneDisplay;

  // Approval detail: what is actually being asked. Prefer the concrete
  // tool call, then the daemon's attention reason.
  const toolName = (payload.toolName || '').trim();
  const summary = (payload.summary || '').trim();
  const reason = (payload.reason || '').trim();
  let approvalDetail = '';
  if (toolName && summary) approvalDetail = ` — ${toolName}: ${summary}`;
  else if (toolName) approvalDetail = ` — ${toolName}`;
  else if (reason) approvalDetail = ` — ${reason}`;

  let title = 'SoluTerminal';
  let body = '';
  if (payload.state === 'feedback') {
    // Reporter loop (cloud feedback_loop.go): a pipeline event on one of the
    // user's own feedback reports. title = the report's title, summary = the
    // event message ("Needs more info — …" / "Your report was fixed — …").
    // No pane/workspace grammar — this isn't pane activity.
    title = `Feedback: ${rawTitle || 'your report'}`;
    body = summary || 'Your report has an update';
  } else if (payload.state === 'choice-prompt') {
    title = location;
    body = `Approval needed${approvalDetail}${machineLine}`;
  } else if (payload.state === 'awaiting-input') {
    title = location;
    body = `Claude is waiting for you${machineLine}`;
  } else if (payload.state === 'reply-complete') {
    title = location;
    body = `Claude finished — tap to review${machineLine}`;
  } else if (payload.state === 'schedule-error') {
    // Schedule pushes (sw v16): payload.title = the schedule's name,
    // payload.summary = the run detail. paneId carries "sched:<id>" purely
    // for tag coalescing — the click handler opens the app as usual.
    title = '⚠️ Schedule failed';
    body = rawTitle + (summary ? ` — ${summary}` : '') + machineLine;
  } else if (payload.state === 'schedule-fired') {
    title = `⏰ ${rawTitle || 'Automation'}`;
    body = (summary || 'ran just now') + machineLine;
  } else if (payload.state === 'user-ask') {
    // New asks from agents (daemon asks_push.go): title = who is waiting
    // (the daemon's pane labels), summary = the ask or "N new asks".
    title = rawTitle ? `${rawTitle} · waiting on you` : 'Waiting on you';
    body = (summary || 'An agent is waiting on you') + machineLine;
  } else if (payload.state === 'agent-needs-you') {
    title = `${rawTitle || 'Agent'} needs you`;
    body = (summary || 'Open Tasks to review the decision') + machineLine;
  } else if (payload.state === 'agent-error') {
    title = `${rawTitle || 'Agent'} failed`;
    body = (summary || 'Open Agents to inspect the run') + machineLine;
  } else if (payload.state === 'agent-report') {
    title = rawTitle || 'Agent report';
    body = (summary || 'A managed-agent report is ready') + machineLine;
  } else if (payload.state === 'release-pending' || payload.state === 'workflow-completed') {
    title = rawTitle || (payload.state === 'release-pending' ? 'Release pending' : 'Workflow task merged');
    body = (summary || 'A versioned publish is required to activate merged work') + machineLine;
  } else if (payload.state === 'workflow-approval') {
    title = rawTitle || 'Workflow approval needed';
    body = (summary || 'Awaiting your review in Tasks') + machineLine;
  } else if (payload.state === 'workflow-failed') {
    title = rawTitle || 'Workflow task failed';
    body = (summary || 'The task failed — open Tasks to inspect') + machineLine;
  } else if (payload.state === 'workflow-land-failed') {
    // A landing that died AFTER a human approved it: the work is done,
    // reviewed, approved — and outside the repository. The daemon leads the
    // summary with the blocker (rebase_conflict / reverify_failed /
    // assertion_failed / …) so the lock screen names it without opening
    // the app; falling through to the generic tail would drop that entirely.
    title = rawTitle || 'Landing failed after approval';
    body = (summary || 'The approved work did not reach the repository — open Tasks to inspect') + machineLine;
  } else if (payload.state === 'workflow-decision') {
    title = rawTitle || 'Workflow decision needed';
    body = (summary || 'Recovery needs your decision — open Tasks') + machineLine;
  } else if (payload.state === 'workflow-auto-resumed') {
    // Inform, not ask: triage auto-apply already resumed the task.
    title = rawTitle || 'Workflow task auto-resumed';
    body = (summary || 'A transient failure was resumed automatically — open Tasks') + machineLine;
  } else if (payload.state === 'workflow-preauthorized') {
    title = rawTitle || 'Repo policy approved a task';
    body = (summary || 'A repository policy pre-approved this task after its grace window — inspect in Tasks') + machineLine;
  } else if (payload.state === 'workflow-autopilot') {
    // The window is still OPEN when this arrives — it is the veto
    // opportunity, not the receipt above.
    title = rawTitle || 'Autopilot will approve a task';
    body = (summary || 'A countdown started — veto in Tasks before it ends') + machineLine;
  } else if (payload.state === 'workflow-digest') {
    title = rawTitle || 'Workflow attention digest';
    body = (summary || 'Workflow tasks need your attention — open Tasks') + machineLine;
  } else if (payload.state === 'workflow-fallback') {
    title = rawTitle || 'Workflow fallback engaged';
    body = (summary || 'A task switched runtime — open Tasks') + machineLine;
  } else if (payload.state === 'workflow-budget') {
    title = rawTitle || 'Workflow task paused at cost ceiling';
    body = (summary || 'Raise the ceiling in Tasks to continue') + machineLine;
  } else {
    title = `soluterminal update`;
    body = location + machineLine;
  }

  event.waitUntil((async () => {
    await self.registration.showNotification(title, {
      body,
      icon: '/icon-192.png',
      badge: '/icon-192.png',
      tag: 'solu-pane-' + (payload.paneId || 'unknown'),  // coalesces per-pane
      // 2026-05-07: only re-buzz on choice-prompt (Claude is blocked asking
      // a question); awaiting-input updates silently to stop amplifying
      // any daemon-side repeat into multiple OS-level buzzes.
      renotify: payload.state === 'choice-prompt' || payload.state === 'reply-complete'
        || payload.state === 'schedule-error' || payload.state === 'schedule-fired'
        || payload.state === 'agent-needs-you' || payload.state === 'agent-error' || payload.state === 'agent-report'
        || payload.state === 'user-ask'
        || payload.state === 'release-pending' || payload.state === 'workflow-completed'
        || payload.state === 'workflow-approval' || payload.state === 'workflow-failed'
        || payload.state === 'workflow-land-failed'
        || payload.state === 'workflow-decision' || payload.state === 'workflow-auto-resumed'
        || payload.state === 'workflow-preauthorized' || payload.state === 'workflow-autopilot'
        || payload.state === 'workflow-digest' || payload.state === 'workflow-fallback'
        || payload.state === 'workflow-budget',
      // vibrate is the Web Push → Android signal for "elevate this to
      // the heads-up banner + lockscreen UI, not the silent shade".
      // Without it Chrome creates the WebAPK notification channel at
      // IMPORTANCE_DEFAULT (low), and Android just drops the notification
      // into the tray with no banner. Users who find it too aggressive
      // can dial the app's channel importance down from Android
      // Settings → Apps → soluterminal → Notifications.
      vibrate: [200, 100, 200],
      // silent:false is explicit; leaving it implicit also works but
      // some Chromium versions default silent to true for repeat tags.
      silent: false,
      // NOTE: requireInteraction was set true in v0.0.113 (v12). Removed
      // in v13: on Android WebAPK, Chrome maps the flag to a persistent/
      // ongoing-style notification that Android treats as status-bar-
      // only — no heads-up banner, ever. The flag was gaining us
      // "banner stays until tapped" at the cost of "banner never shows
      // in the first place on Android." Not worth it. Desktop users
      // lose the stickiness but the tray entry persists regardless.
      data: {
        machineId: payload.machineId || null,
        paneId: payload.paneId || null,
      },
    });

    // Silent-throttle detection: ack this delivery so the cloud knows
    // the endpoint is still alive. Introduced in sw v15. The cloud
    // tracks (last_send_at, last_ack_at); if last_send stays ahead of
    // last_ack for longer than the staleness window (3 min), the next
    // /api/push/subscribe liveness POST returns {stale: true} and the
    // client rotates automatically. Without this ack, Android silently
    // throttling our endpoint was invisible to the server — we were
    // relying on a fixed 1h rotation timer that didn't match throttle
    // onset (~25 min observed). Fire-and-forget: a failed ack just
    // means we miss one data point; the next successful push acks
    // again. No-op when there is no current subscription.
    try {
      const sub = await self.registration.pushManager.getSubscription();
      if (sub && sub.endpoint) {
        await fetch('/api/push/ack', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ endpoint: sub.endpoint }),
        });
      }
    } catch { /* best effort */ }
  })());
});

// How long a window gets to say the app is mounted there. A live page answers
// in a few milliseconds; the wait has to stay well inside the second or so
// browsers allow a notification click to focus or open a window.
const TAP_READY_WAIT_MS = 400;

// Resolves true when the window answers, i.e. the app (its listener in
// lib/notificationTap.ts) is mounted there. A sign-in screen, the computer
// picker and a standalone page never answer, and a tap posted to one is lost.
function tapReady(client) {
  return new Promise((resolve) => {
    let timer = 0;
    try {
      const channel = new MessageChannel();
      const done = (ok) => { clearTimeout(timer); channel.port1.close(); resolve(ok); };
      channel.port1.onmessage = () => done(true);
      timer = setTimeout(() => done(false), TAP_READY_WAIT_MS);
      client.postMessage({ type: 'solu:notification-tap-ready' }, [channel.port2]);
    } catch {
      clearTimeout(timer);
      resolve(false);
    }
  });
}

// The best window that answers: the focused one, then a visible one, then any.
// Every window is asked at once, so the wait never adds up.
function readyAppWindow(list) {
  const rank = (c) => (c.focused ? 0 : c.visibilityState === 'visible' ? 1 : 2);
  const windows = list.filter((c) => 'focus' in c).sort((a, b) => rank(a) - rank(b));
  const asked = windows.map((c) => tapReady(c).then((ok) => (ok ? c : null)));
  return asked.reduce((best, next) => best.then((found) => found || next), Promise.resolve(null));
}

// Re-focus an existing window when the user taps a notification.
// If the notification carries machineId, route there first. Appends
// ?pane=<paneId> when known so the app can focus that specific pane
// on arrival — helpful when the workspace has many panes and the
// notification body alone wouldn't tell the user which one.
self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const data = event.notification.data || {};
  const paneQuery = data.paneId ? `?pane=${encodeURIComponent(data.paneId)}` : '';
  const machinePath = data.machineId ? `/m/${encodeURIComponent(data.machineId)}/` : '/';
  const targetPath = machinePath + paneQuery;

  // No window can take the target as a message: load it from the URL.
  const openAtTarget = (list) => {
    for (const client of list) {
      if ('focus' in client) {
        // Navigate focused window if it exists but isn't at target.
        if ('navigate' in client && targetPath !== '/') {
          return client.navigate(targetPath).then(() => client.focus());
        }
        return client.focus();
      }
    }
    if (self.clients.openWindow) return self.clients.openWindow(targetPath);
  };

  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((list) => {
      // Prefer a window already at the target, otherwise any window.
      for (const client of list) {
        try {
          const u = new URL(client.url);
          if (data.machineId && u.pathname.startsWith(`/m/${data.machineId}`)) {
            // Already at the right machine — the app is running old-page-
            // load state, so a URL param can't reach it. postMessage the
            // target instead (App.tsx listens); focus() alone silently
            // dropped the tap intent for the MOST common case, the app
            // sitting open in the background. (sw v24)
            if (data.paneId) {
              try { client.postMessage({ type: 'solu:notification-tap', paneId: data.paneId, machineId: data.machineId }); } catch { /* best effort */ }
            }
            return client.focus();
          }
        } catch { /* ignore malformed */ }
      }
      // A feedback update (cloud feedback_loop.go) is for the account, not one
      // machine, so it carries no machineId and any window running the app can
      // show it. Hand it the target the same way; navigating to
      // "/?pane=feedback:N" would reload the app. A window that does not
      // answer is not running the app, so the tap falls through to the URL,
      // which is still there once a computer is picked. (sw v31)
      if (!data.machineId && typeof data.paneId === 'string' && data.paneId.startsWith('feedback:')) {
        return readyAppWindow(list).then((open) => {
          if (!open) return openAtTarget(list);
          try { open.postMessage({ type: 'solu:notification-tap', paneId: data.paneId }); } catch { /* best effort */ }
          return open.focus();
        });
      }
      return openAtTarget(list);
    })
  );
});
