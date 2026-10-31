import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
vi.mock('./cloud', () => ({ useCloud: { subscribe: vi.fn(), getState: () => ({ session: null }) } }));

import { createPrefsStore, type UserPrefs } from './prefs';
import type { RoutingScope } from './api';
import { effectiveMiddleOrder } from './components/sidebar/sidebarSections';

function memoryStorage(): Storage {
  const data = new Map<string, string>();
  return {
    get length() { return data.size; },
    clear: () => data.clear(), getItem: (key) => data.get(key) ?? null,
    setItem: (key, value) => { data.set(key, value); }, removeItem: (key) => { data.delete(key); },
    key: (i) => [...data.keys()][i] ?? null,
  };
}
const layout: UserPrefs = { sidebar: { order: ['mcp', 'credentials', 'files'], hidden: [], open: ['mcp'], workspacesCollapsed: true } };
const scopeA: RoutingScope = { prefix: '/m/A', backendPrefix: '/b/wsl', key: 'A' };
const scopeB: RoutingScope = { prefix: '/m/B', backendPrefix: '/b/win', key: 'B' };
function setup(storage = memoryStorage()) {
  let scope = scopeA;
  let paused = false;
  const get = vi.fn().mockResolvedValue({ prefs: null });
  const put = vi.fn().mockResolvedValue({});
  const supported = vi.fn().mockResolvedValue(true);
  const client = { scope: () => scope, supported, get, put, paused: () => paused };
  const store = createPrefsStore(client, storage);
  return { store, get, put, supported, client, storage, route: (s: RoutingScope) => { scope = s; }, pause: (p: boolean) => { paused = p; } };
}
function legacy(storage: Storage, prefs: UserPrefs = layout, userId: string | null = null) {
  storage.setItem('solu.sidebar.agentPanesIntroduced', '1');
  storage.setItem('solu.userPrefs.v1', JSON.stringify({ prefs, userId, updatedAt: '2099-01-01T00:00:00Z' }));
}

describe('daemon sidebar preferences', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('keeps old-daemon choices in localStorage without GET/PUT probes, and rechecks a new backend', async () => {
    const storage = memoryStorage();
    legacy(storage);
    const { store, get, put, supported, client, route } = setup(storage);
    supported.mockResolvedValue(false);
    await store.getState().hydrateFromDaemon();
    expect(supported).toHaveBeenCalledExactlyOnceWith(scopeA);
    expect(store.getState().prefs).toEqual(layout);
    expect(store.getState().hydrated).toBe(true);
    store.getState().setPrefs(() => ({ sidebar: { hidden: ['ports'] } }));
    await vi.advanceTimersByTimeAsync(800);
    await store.getState().hydrateFromDaemon();
    expect(supported).toHaveBeenCalledTimes(1);
    expect(get).not.toHaveBeenCalled();
    expect(put).not.toHaveBeenCalled();
    const reload = createPrefsStore(client, storage);
    await reload.getState().hydrateFromDaemon();
    expect(reload.getState().prefs.sidebar?.hidden).toEqual(['ports']);
    expect(reload.getState().pending).toBe(true);

    supported.mockResolvedValue(true);
    route(scopeB);
    await store.getState().hydrateFromDaemon();
    expect(get).toHaveBeenCalledExactlyOnceWith(scopeB);
    store.getState().setPrefs(() => layout);
    await vi.advanceTimersByTimeAsync(800);
    expect(put).toHaveBeenCalledExactlyOnceWith(layout, scopeB);
  });

  it('does not let a stale capability response disable the newly selected backend', async () => {
    const { store, get, put, supported, route } = setup();
    let resolve!: (supported: boolean) => void;
    supported.mockReturnValueOnce(new Promise<boolean>((r) => { resolve = r; }));
    const old = store.getState().hydrateFromDaemon();
    await vi.advanceTimersByTimeAsync(0);
    route(scopeB);
    await store.getState().hydrateFromDaemon();
    resolve(false);
    await old;
    expect(get).toHaveBeenCalledExactlyOnceWith(scopeB);
    store.getState().setPrefs(() => layout);
    await vi.advanceTimersByTimeAsync(800);
    expect(put).toHaveBeenCalledExactlyOnceWith(layout, scopeB);
  });

  it('keeps edits local when health is offline and retries capability detection on hydration', async () => {
    const { store, get, put, supported } = setup();
    supported.mockRejectedValueOnce(new Error('offline'));
    await store.getState().hydrateFromDaemon();
    store.getState().setPrefs(() => layout);
    await vi.advanceTimersByTimeAsync(800);
    expect(get).not.toHaveBeenCalled();
    expect(put).not.toHaveBeenCalled();
    await store.getState().hydrateFromDaemon();
    await vi.advanceTimersByTimeAsync(800);
    expect(put).toHaveBeenCalledExactlyOnceWith(layout, scopeA);
  });

  it('migrates existing localStorage only when the daemon has no copy, debounced', async () => {
    const storage = memoryStorage();
    legacy(storage);
    const { store, get, put } = setup(storage);
    await store.getState().hydrateFromDaemon();
    expect(get).toHaveBeenCalledWith(scopeA);
    expect(store.getState().prefs).toEqual(layout);
    expect(put).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(800);
    expect(put).toHaveBeenCalledExactlyOnceWith(layout, scopeA);
  });

  it('loads daemon first, even if the browser cache has a later timestamp', async () => {
    const storage = memoryStorage();
    legacy(storage);
    const { store, get, put } = setup(storage);
    get.mockResolvedValue({ prefs: { sidebar: { hidden: ['files'] } } });
    await store.getState().hydrateFromDaemon();
    expect(store.getState().prefs.sidebar?.hidden).toEqual(['files']);
    await vi.advanceTimersByTimeAsync(800);
    expect(put).not.toHaveBeenCalled();
  });

  it('keeps DEFAULT_HIDDEN for brand-new users and a saved reset', async () => {
    for (const prefs of [null, {}]) {
      const { store, get, put } = setup();
      get.mockResolvedValue({ prefs });
      await store.getState().hydrateFromDaemon();
      expect(store.getState().prefs).toEqual({});
      expect(effectiveMiddleOrder(undefined, undefined)).not.toEqual(expect.arrayContaining(['mcp', 'credentials']));
      await vi.advanceTimersByTimeAsync(800);
      expect(put).not.toHaveBeenCalled();
    }
  });

  it('falls back to localStorage when daemon loading fails, including old daemons', async () => {
    const storage = memoryStorage();
    legacy(storage);
    const { store, get, put } = setup(storage);
    get.mockRejectedValue(new Error('404 / offline'));
    await store.getState().hydrateFromDaemon();
    expect(store.getState().prefs).toEqual(layout);
    expect(store.getState().hydrated).toBe(true);
    expect(put).not.toHaveBeenCalled();
    put.mockRejectedValue(new Error('offline'));
    store.getState().setPrefs(() => ({ sidebar: { hidden: ['ports'] } }));
    await vi.advanceTimersByTimeAsync(800);
    const reload = createPrefsStore({ scope: () => scopeA, get, put, paused: () => false }, storage);
    await reload.getState().hydrateFromDaemon();
    expect(reload.getState().prefs.sidebar?.hidden).toEqual(['ports']);
  });

  it('debounces rapid changes to one PUT of the final layout', async () => {
    const { store, put } = setup();
    await store.getState().hydrateFromDaemon();
    store.getState().setPrefs(() => ({ sidebar: { hidden: ['files'] } }));
    await vi.advanceTimersByTimeAsync(500);
    store.getState().setPrefs(() => layout);
    await vi.advanceTimersByTimeAsync(799);
    expect(put).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(put).toHaveBeenCalledExactlyOnceWith(layout, scopeA);
  });

  it('preserves choices when the app origin changes and localStorage is empty', async () => {
    // Each store has independent origin storage; both use one durable daemon.
    let daemon: UserPrefs | null = null;
    const client = {
      scope: () => scopeA, paused: () => false,
      get: async () => ({ prefs: daemon }),
      put: async (prefs: UserPrefs) => { daemon = structuredClone(prefs); },
    };
    const routerOrigin = memoryStorage();
    legacy(routerOrigin);
    const oldApp = createPrefsStore(client, routerOrigin);
    await oldApp.getState().hydrateFromDaemon();
    await vi.advanceTimersByTimeAsync(800);
    const directOrigin = memoryStorage();
    const newApp = createPrefsStore(client, directOrigin);
    await newApp.getState().hydrateFromDaemon();
    expect(newApp.getState().prefs).toEqual(layout);
    const sidebar = newApp.getState().prefs.sidebar;
    expect(effectiveMiddleOrder(sidebar?.order, sidebar?.hidden).slice(0, 2)).toEqual(['mcp', 'credentials']);
    expect(sidebar?.open).toEqual(['mcp']);
    expect(sidebar?.workspacesCollapsed).toBe(true);
  });

  it('migrates the legacy collapse keys with the one-time agent section introduction', async () => {
    const storage = memoryStorage();
    storage.setItem('solu.sidebarOpenMiddle.v1', 'files');
    storage.setItem('solu.sidebarWorkspacesCollapsed.v1', '1');
    const { store, put } = setup(storage);
    await store.getState().hydrateFromDaemon();
    await vi.advanceTimersByTimeAsync(800);
    expect(put).toHaveBeenCalledWith({ sidebar: { open: ['files', 'agentPanes'], workspacesCollapsed: true } }, scopeA);
  });

  it('does not reopen an explicitly collapsed agent section on migration', async () => {
    const storage = memoryStorage();
    storage.setItem('solu.sidebarOpenMiddle.v2', '[]');
    storage.setItem('solu.sidebar.agentPanesIntroduced', '1');
    const { store } = setup(storage);
    await store.getState().hydrateFromDaemon();
    expect(store.getState().prefs.sidebar?.open).toEqual([]);
  });

  it('isolates backend caches, migration, pending debounces and stale GET responses', async () => {
    const storage = memoryStorage();
    legacy(storage);
    const { store, get, put, route } = setup(storage);
    await store.getState().hydrateFromDaemon();
    // A migration upload is still pending when the person picks machine B.
    route(scopeB);
    await store.getState().hydrateFromDaemon();
    expect(store.getState().prefs).toEqual({});
    await vi.advanceTimersByTimeAsync(800);
    expect(put).not.toHaveBeenCalled();
    // Read A while B is selected before the response from A arrives.
    route(scopeA);
    let resolve!: (v: { prefs: UserPrefs }) => void;
    get.mockReturnValueOnce(new Promise((r) => { resolve = r; }));
    const pending = store.getState().hydrateFromDaemon();
    await vi.advanceTimersByTimeAsync(0);
    route(scopeB);
    get.mockResolvedValue({ prefs: { sidebar: { hidden: ['mcp'] } } });
    await store.getState().hydrateFromDaemon();
    resolve({ prefs: layout });
    await pending;
    expect(store.getState().prefs.sidebar?.hidden).toEqual(['mcp']);
    // B's fallback remains B's, even when the network fails next visit.
    route(scopeA);
    await store.getState().hydrateFromDaemon();
    route(scopeB);
    get.mockRejectedValue(new Error('offline'));
    await store.getState().hydrateFromDaemon();
    expect(store.getState().prefs.sidebar?.hidden).toEqual(['mcp']);
  });

  it('never migrates another signed-in user’s browser layout', async () => {
    const storage = memoryStorage();
    legacy(storage, layout, 'A');
    const { store, put } = setup(storage);
    await store.getState().hydrateFromDaemon('B');
    expect(store.getState().prefs).toEqual({});
    await vi.advanceTimersByTimeAsync(800);
    expect(put).not.toHaveBeenCalled();
  });

  it('replays changes made during hydration over the daemon layout', async () => {
    const { store, get, put } = setup();
    let resolve!: (v: { prefs: UserPrefs }) => void;
    get.mockReturnValue(new Promise((r) => { resolve = r; }));
    const pending = store.getState().hydrateFromDaemon();
    await vi.advanceTimersByTimeAsync(0);
    store.getState().setPrefs((p) => ({ ...p, sidebar: { ...p.sidebar, workspacesCollapsed: true } }));
    resolve({ prefs: { sidebar: { order: ['mcp'], hidden: [] } } });
    await pending;
    await vi.advanceTimersByTimeAsync(800);
    expect(put).toHaveBeenCalledExactlyOnceWith({ sidebar: { order: ['mcp'], hidden: [], workspacesCollapsed: true } }, scopeA);
  });

  it('applies toggles exactly once while the first daemon GET has no saved copy', async () => {
    const { store, get, put } = setup();
    let resolve!: (v: { prefs: null }) => void;
    get.mockReturnValue(new Promise((r) => { resolve = r; }));
    const pending = store.getState().hydrateFromDaemon();
    await vi.advanceTimersByTimeAsync(0);
    store.getState().setPrefs((p) => ({ ...p, sidebar: { ...p.sidebar, workspacesCollapsed: !p.sidebar?.workspacesCollapsed } }));
    resolve({ prefs: null });
    await pending;
    await vi.advanceTimersByTimeAsync(800);
    expect(store.getState().prefs.sidebar?.workspacesCollapsed).toBe(true);
    expect(put).toHaveBeenCalledWith({ sidebar: { workspacesCollapsed: true } }, scopeA);
  });

  it('retains unsaved edits across backend switches and reloads', async () => {
    const { store, get, put, route, client, storage } = setup();
    get.mockResolvedValue({ prefs: { sidebar: { hidden: ['mcp'] } } });
    await store.getState().hydrateFromDaemon();
    store.getState().setPrefs(() => layout);
    // Switch before the debounce sends anything. It must not send A to B.
    route(scopeB);
    await store.getState().hydrateFromDaemon();
    await vi.advanceTimersByTimeAsync(800);
    expect(put).not.toHaveBeenCalled();
    route(scopeA);
    const reload = createPrefsStore(client, storage);
    await reload.getState().hydrateFromDaemon();
    expect(reload.getState().prefs).toEqual(layout);
    await vi.advanceTimersByTimeAsync(800);
    expect(put).toHaveBeenCalledExactlyOnceWith(layout, scopeA);
    expect(reload.getState().pending).toBe(false);
  });

  it('does not let anonymous startup consume a signed-in user’s migration', async () => {
    const storage = memoryStorage();
    legacy(storage, layout, 'A');
    const { store, put } = setup(storage);
    await store.getState().hydrateFromDaemon();
    expect(store.getState().prefs).toEqual({});
    await store.getState().hydrateFromDaemon('A');
    expect(store.getState().prefs).toEqual(layout);
    await vi.advanceTimersByTimeAsync(800);
    expect(put).toHaveBeenCalledWith(layout, scopeA);
  });

  it('imports the old account-cloud layout only when this daemon and browser have no layout', async () => {
    const { client, storage, get, put } = setup();
    const legacyCloud = vi.fn().mockResolvedValue({ prefs: layout });
    const store = createPrefsStore({ ...client, legacy: legacyCloud }, storage);
    await store.getState().hydrateFromDaemon('A');
    expect(store.getState().prefs).toEqual(layout);
    await vi.advanceTimersByTimeAsync(800);
    expect(put).toHaveBeenCalledWith(layout, scopeA);
    // A new app origin reads the daemon, without importing the cloud again.
    get.mockResolvedValue({ prefs: layout });
    legacyCloud.mockClear();
    const newOrigin = createPrefsStore({ ...client, legacy: legacyCloud }, memoryStorage());
    await newOrigin.getState().hydrateFromDaemon('A');
    expect(legacyCloud).not.toHaveBeenCalled();
    expect(newOrigin.getState().prefs).toEqual(layout);
  });

  it('keeps the browser fallback if the one-time cloud import is unavailable', async () => {
    const { client, storage } = setup();
    const store = createPrefsStore({ ...client, legacy: async () => { throw new Error('offline'); } }, storage);
    await store.getState().hydrateFromDaemon('A');
    expect(store.getState().hydrated).toBe(true);
    expect(store.getState().prefs).toEqual({});
  });

  it('ignores corrupt legacy fields', async () => {
    const storage = memoryStorage();
    storage.setItem('solu.userPrefs.v1', JSON.stringify({ prefs: { sidebar: { hidden: 42, order: null } } }));
    const { store } = setup(storage);
    await store.getState().hydrateFromDaemon();
    expect(store.getState().prefs).toEqual({ sidebar: {} });
  });

  it('serializes PUTs so an older save cannot land after a newer save', async () => {
    const { store, put } = setup();
    await store.getState().hydrateFromDaemon();
    let finish!: () => void;
    put.mockReturnValueOnce(new Promise<void>((r) => { finish = r; }));
    store.getState().setPrefs(() => ({ sidebar: { hidden: ['files'] } }));
    await vi.advanceTimersByTimeAsync(800);
    store.getState().setPrefs(() => layout);
    await vi.advanceTimersByTimeAsync(800);
    expect(put).toHaveBeenCalledTimes(1);
    finish();
    await vi.advanceTimersByTimeAsync(0);
    expect(put).toHaveBeenLastCalledWith(layout, scopeA);
  });

  it('pauses reads and writes in personal view', async () => {
    const { store, get, put, pause } = setup();
    pause(true);
    store.getState().setPrefs(() => layout);
    await store.getState().hydrateFromDaemon();
    await vi.advanceTimersByTimeAsync(800);
    expect(get).not.toHaveBeenCalled();
    expect(put).not.toHaveBeenCalled();
    expect(store.getState().prefs).toEqual({});
  });
});
