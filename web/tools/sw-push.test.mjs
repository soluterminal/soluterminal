// @vitest-environment node
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it, vi } from 'vitest';

// The service worker is the rendering half of every daemon push kind: the
// daemon composes title + summary, sw.js decides what actually reaches a lock
// screen. A kind the daemon fires but sw.js has no branch for falls into the
// generic tail — title "soluterminal update", body = pane location, the
// summary DISCARDED, and no re-buzz — so adding a push kind daemon-side
// without wiring it here silently downgrades the notification instead of
// improving it. These tests pin the workflow lifecycle kinds against that.
//
// Authored as .mjs under web/tools/ (the repo's wired .mjs runner, see
// steward-equivalence.test.mjs) because sw.js is a classic service-worker
// script, not an ES module the TS project can import: it is loaded here by
// evaluating the source against an injected `self`.

// Runs under the node environment (like capture-screens.test.mjs and
// e2e-smoke.test.mjs, the other tools tests that read repo files): the default
// jsdom environment resolves import.meta.url to an http URL, which readFileSync
// rejects outright. sw.js needs no DOM — it is evaluated against a stub `self`.
const swSource = fileURLToPath(new URL('../src/sw.js', import.meta.url));

// sw.js is a classic service-worker script: no imports, no exports, every
// entry point registered on `self`. Injecting a stub `self` is how a test
// reaches the push renderer without a browser.
function loadServiceWorker() {
  const source = readFileSync(swSource, 'utf8');
  const listeners = new Map();
  const showNotification = vi.fn(() => Promise.resolve());
  const stubSelf = {
    addEventListener: (type, listener) => { listeners.set(type, listener); },
    registration: { showNotification },
    skipWaiting: () => {},
    clients: { claim: () => Promise.resolve(), matchAll: () => Promise.resolve([]) },
  };
  new Function('self', source)(stubSelf);
  return { listeners, showNotification };
}

async function renderPush(payload) {
  const { listeners, showNotification } = loadServiceWorker();
  const push = listeners.get('push');
  if (!push) throw new Error('sw.js registered no push listener');
  const pending = [];
  push({ data: { json: () => payload }, waitUntil: (promise) => { pending.push(promise); } });
  await Promise.all(pending);
  const call = showNotification.mock.calls[0];
  if (!call) throw new Error('push showed no notification');
  return { title: call[0], body: call[1].body ?? '', renotify: call[1].renotify === true };
}

// Taps: what notificationclick does with the windows that are open.
async function tapNotification(data, clients) {
  const source = readFileSync(swSource, 'utf8');
  const listeners = new Map();
  const openWindow = vi.fn(() => Promise.resolve());
  const stubSelf = {
    addEventListener: (type, listener) => { listeners.set(type, listener); },
    registration: { showNotification: () => Promise.resolve() },
    skipWaiting: () => {},
    clients: { claim: () => Promise.resolve(), matchAll: () => Promise.resolve(clients), openWindow },
  };
  new Function('self', source)(stubSelf);
  const pending = [];
  listeners.get('notificationclick')({
    notification: { close: () => {}, data },
    waitUntil: (promise) => { pending.push(promise); },
  });
  await Promise.all(pending);
  return { openWindow };
}

// A window as the worker sees it. `app: true` is a window where the app is
// mounted: it answers the worker's "are you listening?" on the port it is sent
// (lib/notificationTap.ts). Any other page never answers.
function windowClient(url, { app = false, ...over } = {}) {
  return {
    url,
    focused: false,
    visibilityState: 'hidden',
    postMessage: vi.fn((message, transfer) => {
      if (app && message.type === 'solu:notification-tap-ready') transfer[0].postMessage('ready');
    }),
    focus: vi.fn(() => Promise.resolve()),
    navigate: vi.fn(() => Promise.resolve()),
    ...over,
  };
}

const tapsSentTo = (client) => client.postMessage.mock.calls.map(([message]) => message).filter((m) => m.type === 'solu:notification-tap');

describe('service worker feedback notification tap', () => {
  const feedback = { machineId: null, paneId: 'feedback:42' };
  const tap = { type: 'solu:notification-tap', paneId: 'feedback:42' };

  it('hands the report to an open window instead of reloading it', async () => {
    const background = windowClient('https://app.example/m/m_1/', { app: true });
    const front = windowClient('https://app.example/m/m_2/', { app: true, focused: true, visibilityState: 'visible' });
    const { openWindow } = await tapNotification(feedback, [background, front]);
    expect(tapsSentTo(front)).toEqual([tap]);
    expect(front.focus).toHaveBeenCalledTimes(1);
    expect(front.navigate).not.toHaveBeenCalled();
    expect(tapsSentTo(background)).toEqual([]);
    expect(background.focus).not.toHaveBeenCalled();
    expect(openWindow).not.toHaveBeenCalled();
  });

  // A sign-in screen, the computer picker or a standalone page has no
  // listener: a message posted there is lost, so the target goes in the URL.
  it('loads the report from the URL in a window that is not running the app', async () => {
    const signIn = windowClient('https://app.example/', { focused: true, visibilityState: 'visible' });
    const { openWindow } = await tapNotification(feedback, [signIn]);
    expect(tapsSentTo(signIn)).toEqual([]);
    expect(signIn.navigate).toHaveBeenCalledWith('/?pane=feedback%3A42');
    expect(signIn.focus).toHaveBeenCalledTimes(1);
    expect(openWindow).not.toHaveBeenCalled();
  });

  it('picks the window running the app over a focused page that is not', async () => {
    const page = windowClient('https://app.example/privacy', { focused: true, visibilityState: 'visible' });
    const app = windowClient('https://app.example/m/m_1/', { app: true });
    await tapNotification(feedback, [page, app]);
    expect(tapsSentTo(app)).toEqual([tap]);
    expect(app.focus).toHaveBeenCalledTimes(1);
    expect(page.navigate).not.toHaveBeenCalled();
    expect(page.focus).not.toHaveBeenCalled();
  });

  it('opens the app on the report when no window is open', async () => {
    const { openWindow } = await tapNotification(feedback, []);
    expect(openWindow).toHaveBeenCalledWith('/?pane=feedback%3A42');
  });

  it('still navigates a window on another machine for a pane push', async () => {
    const other = windowClient('https://app.example/m/m_2/');
    await tapNotification({ machineId: 'm_1', paneId: 'pane_9' }, [other]);
    expect(other.navigate).toHaveBeenCalledWith('/m/m_1/?pane=pane_9');
    expect(other.postMessage).not.toHaveBeenCalled();
  });
});

describe('service worker workflow push rendering', () => {
  it('renders a post-approval landing failure with its blocker-led summary', async () => {
    const shown = await renderPush({
      paneId: 'workflow:wft_abc123',
      state: 'workflow-land-failed',
      title: 'Landing failed after approval: Ship the release',
      summary: 'assertion_failed · approved · merge reported landed 1a2b3c4d (unconfirmed) · landed assertion failed',
      machineName: 'devbox',
    });
    expect(shown.title).toBe('Landing failed after approval: Ship the release');
    // The blocker token surviving to the lock screen is the whole point of the
    // land-specific kind.
    expect(shown.body).toContain('assertion_failed');
    expect(shown.body).toContain('landed assertion failed');
    expect(shown.body).toContain('on devbox');
    // Approved work sitting outside the repository re-buzzes, exactly like the
    // generic failure push this kind replaces.
    expect(shown.renotify).toBe(true);
  });

  it('falls back to a self-explanatory landing-failure line without a summary', async () => {
    const shown = await renderPush({
      paneId: 'workflow:wft_abc123',
      state: 'workflow-land-failed',
      title: 'Landing failed after approval: Ship the release',
    });
    expect(shown.title).toBe('Landing failed after approval: Ship the release');
    expect(shown.body).toContain('did not reach the repository');
  });

  // The kind this one replaces for land failures must keep rendering exactly
  // as before for every other failure.
  it('still renders the generic task failure', async () => {
    const shown = await renderPush({
      paneId: 'workflow:wft_abc123',
      state: 'workflow-failed',
      title: 'Task failed: Ship the release',
      summary: 'verification failed: go test ./daemon/',
    });
    expect(shown.title).toBe('Task failed: Ship the release');
    expect(shown.body).toContain('verification failed: go test ./daemon/');
    expect(shown.renotify).toBe(true);
  });
});

describe('service worker managed-agent push rendering', () => {
  it.each([
    {
      state: 'agent-needs-you', title: 'Board Steward needs you',
      body: 'Open Tasks to review the decision',
    },
    {
      state: 'agent-error', title: 'Board Steward failed',
      body: 'Open Agents to inspect the run',
    },
    {
      state: 'agent-report', title: 'Board Steward',
      body: 'A managed-agent report is ready',
    },
  ])('renders and re-notifies $state', async ({ state, title, body }) => {
    const shown = await renderPush({ paneId: 'agent:agt_steward', state, title: 'Board Steward' });
    expect(shown.title).toBe(title);
    expect(shown.body).toBe(body);
    expect(shown.renotify).toBe(true);
  });
});
