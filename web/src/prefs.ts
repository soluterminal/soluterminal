import { create } from 'zustand';
import { api, currentRoutingScope, onRoutingScopeChange, type RoutingScope } from './api';
import { useCloud } from './cloud';
import { useEntitlements } from './entitlements';

// The viewed daemon is authoritative. localStorage is an offline cache and a
// one-time migration source, never the identity of the layout's owner.
export interface SidebarPrefs {
  order?: string[];
  hidden?: string[];
  open?: string[];
  workspacesCollapsed?: boolean;
}
export interface UserPrefs { sidebar?: SidebarPrefs }

const LEGACY_KEY = 'solu.userPrefs.v1';
const CACHE_KEY = 'solu.daemonUIPrefs.v1:';
const MIGRATION_KEY = 'solu.daemonUIPrefs.legacyTarget';
const PUSH_DEBOUNCE_MS = 800;

interface StoredPrefs {
  prefs: UserPrefs;
  updatedAt: string | null;
  userId: string | null;
  pending: boolean; // edits not yet acknowledged by this daemon
}
interface PrefsState extends StoredPrefs {
  hydrated: boolean;
  setPrefs: (updater: (prev: UserPrefs) => UserPrefs) => void;
  hydrateFromDaemon: (userId?: string | null) => Promise<void>;
}
interface PrefsClient {
  scope: () => RoutingScope;
  supported?: (scope: RoutingScope) => Promise<boolean>;
  get: (scope: RoutingScope) => Promise<{ prefs: UserPrefs | null }>;
  put: (prefs: UserPrefs, scope: RoutingScope) => Promise<unknown>;
  paused: () => boolean;
  legacy?: () => Promise<{ prefs: UserPrefs }>;
}

// Browser caches and the legacy cloud row predate the daemon schema. Ignore
// malformed fields so corrupt local storage cannot break sidebar rendering.
function cleanPrefs(value: unknown): UserPrefs {
  if (!value || typeof value !== 'object' || !('sidebar' in value)) return {};
  const raw = value.sidebar;
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return {};
  const sidebar: SidebarPrefs = {};
  for (const key of ['order', 'hidden', 'open'] as const) {
    const list = (raw as SidebarPrefs)[key];
    if (Array.isArray(list) && list.length <= 64
      && list.every((item) => typeof item === 'string' && /^[a-zA-Z0-9_-]{1,64}$/.test(item))) {
      sidebar[key] = list;
    }
  }
  const collapsed = (raw as SidebarPrefs).workspacesCollapsed;
  if (typeof collapsed === 'boolean') sidebar.workspacesCollapsed = collapsed;
  return { sidebar };
}

// A factory also lets tests model independent browser origins against one
// daemon without carrying module state or localStorage between them.
export function createPrefsStore(client: PrefsClient, storage?: Storage) {
  const empty = (userId: string | null): StoredPrefs => ({ prefs: {}, updatedAt: null, userId, pending: false });
  const targetKey = (scope: RoutingScope, userId: string | null) => JSON.stringify([scope.prefix, scope.backendPrefix, userId]);
  const read = (scope: RoutingScope, userId: string | null): StoredPrefs => {
    try {
      const cached = storage?.getItem(CACHE_KEY + targetKey(scope, userId));
      if (cached) {
        const parsed = JSON.parse(cached);
        return { ...empty(userId), prefs: cleanPrefs(parsed.prefs),
          updatedAt: typeof parsed.updatedAt === 'string' ? parsed.updatedAt : null, pending: parsed.pending === true };
      }
      // A legacy origin-wide cache may seed just one backend. Never seed a
      // second machine with the first machine's layout, or a different user's.
      const bound = storage?.getItem(MIGRATION_KEY + JSON.stringify(userId));
      if (bound && bound !== targetKey(scope, userId)) return empty(userId);
      const raw = storage?.getItem(LEGACY_KEY);
      const legacy: StoredPrefs = raw ? JSON.parse(raw) : empty(userId);
      if ((legacy.userId ?? null) !== userId) return empty(userId);
      const sidebar: SidebarPrefs = { ...legacy.prefs?.sidebar };
      // Collapse state used to live outside the prefs store.
      const open = storage?.getItem('solu.sidebarOpenMiddle.v2');
      const oldOpen = storage?.getItem('solu.sidebarOpenMiddle.v1');
      if (open) sidebar.open = JSON.parse(open);
      else if (oldOpen) sidebar.open = oldOpen === 'null' ? [] : [oldOpen];
      if (sidebar.open && !storage?.getItem('solu.sidebar.agentPanesIntroduced')) {
        sidebar.open = [...new Set([...sidebar.open, 'agentPanes'])];
      }
      const collapsed = storage?.getItem('solu.sidebarWorkspacesCollapsed.v1');
      if (collapsed != null) sidebar.workspacesCollapsed = collapsed === '1';
      return { ...empty(userId), prefs: Object.keys(sidebar).length ? cleanPrefs({ sidebar }) : {} };
    } catch { return empty(userId); }
  };
  const save = (scope: RoutingScope, value: StoredPrefs) => {
    try {
      storage?.setItem(CACHE_KEY + targetKey(scope, value.userId), JSON.stringify(value));
      const migrationKey = MIGRATION_KEY + JSON.stringify(value.userId);
      if (!storage?.getItem(migrationKey)) storage?.setItem(migrationKey, targetKey(scope, value.userId));
    } catch { /* Daemon persistence still works without browser storage. */ }
  };
  let scope = client.scope();
  let timer: ReturnType<typeof setTimeout> | undefined;
  let generation = 0;
  let revision = 0;
  let loadedFromDaemon = false;
  let canPush = !client.supported;
  let inFlight: Promise<void> | undefined;
  let edits: Array<(prev: UserPrefs) => UserPrefs> = [];

  return create<PrefsState>((set, get) => {
    const current = (captured: RoutingScope, gen: number) => gen === generation && captured.key === client.scope().key;
    const saveState = () => save(scope, { prefs: get().prefs, updatedAt: get().updatedAt, userId: get().userId, pending: get().pending });
    // Serialize writes so a slow older PUT can never overwrite a newer one.
    let writes: Promise<unknown> = Promise.resolve();
    const schedulePush = () => {
      clearTimeout(timer);
      const captured = scope;
      const gen = generation;
      timer = setTimeout(() => {
        timer = undefined;
        const prefs = get().prefs;
        const sentRevision = revision;
        writes = writes.catch(() => {}).then(async () => {
          if (!current(captured, gen) || client.paused() || !get().hydrated || !canPush) return;
          await client.put(prefs, captured);
          if (current(captured, gen) && revision === sentRevision) {
            set({ pending: false });
            saveState();
          }
        }).catch(() => { /* Offline: keep the scoped cache. */ });
      }, PUSH_DEBOUNCE_MS);
    };
    return {
      ...read(scope, null), hydrated: false,
      setPrefs: (updater) => {
        if (client.paused()) return;
        const prefs = updater(get().prefs);
        revision++;
        if (inFlight || !get().hydrated) edits.push(updater);
        set({ prefs, updatedAt: new Date().toISOString(), pending: true });
        saveState();
        if (get().hydrated) schedulePush();
      },
      hydrateFromDaemon: (userId = null) => {
        const nextScope = client.scope();
        if (client.paused()) return Promise.resolve();
        const changed = nextScope.key !== scope.key || userId !== get().userId;
        if (changed) {
          generation++;
          loadedFromDaemon = false;
          canPush = !client.supported;
          clearTimeout(timer);
          inFlight = undefined;
          edits = [];
          scope = nextScope;
          set({ ...read(scope, userId), hydrated: false });
        }
        if (inFlight) return inFlight;
        if (loadedFromDaemon && !changed) return Promise.resolve();
        const captured = scope;
        const gen = generation;
        const rev = revision;
        const localBeforeLoad = get().prefs;
        const pendingBeforeLoad = get().pending;
        edits = [];
        // Defer so inFlight is assigned even if a mock throws synchronously.
        inFlight = Promise.resolve().then(async () => {
          let remote: { prefs: UserPrefs | null };
          try {
            if (client.supported) {
              const supported = await client.supported(captured);
              if (!current(captured, gen) || client.paused()) return;
              canPush = supported;
              if (!supported) {
                // A failed HTTP probe still appears in the browser console.
                // Old daemons/static previews omit the capability in health:
                // keep their scoped local cache without touching the endpoint.
                loadedFromDaemon = true;
                set({ hydrated: true });
                saveState();
                return;
              }
            }
            remote = await client.get(captured);
          }
          catch {
            if (current(captured, gen) && !client.paused()) {
              set({ hydrated: true });
              if (get().pending) schedulePush();
            }
            return;
          }
          if (!current(captured, gen) || client.paused()) return;
          // A one-time account-cloud import can recover the old layout when
          // this app address has no order/visibility cache at all. Subsequent
          // reads and writes stay on the selected backend.
          let base = pendingBeforeLoad ? localBeforeLoad : remote.prefs ?? localBeforeLoad;
          if (remote.prefs === null && !pendingBeforeLoad && userId && client.legacy
            && localBeforeLoad.sidebar?.order === undefined && localBeforeLoad.sidebar?.hidden === undefined) {
            try {
              const legacy = await client.legacy();
              base = { ...base, sidebar: { ...cleanPrefs(legacy.prefs).sidebar, ...base.sidebar } };
              if (Object.keys(base.sidebar ?? {}).length === 0) base = {};
            } catch { /* The browser fallback still works without the cloud. */ }
          }
          if (!current(captured, gen) || client.paused()) return;
          loadedFromDaemon = true;
          // Replay against the pre-GET snapshot, not the already-edited state:
          // a toggle must run exactly once even when no daemon copy exists.
          let prefs = base;
          for (const edit of edits) prefs = edit(prefs);
          const pending = pendingBeforeLoad || rev !== revision;
          set({ prefs, hydrated: true, pending });
          saveState();
          if ((remote.prefs === null && Object.keys(prefs).length > 0) || pending) schedulePush();
        }).finally(() => {
          if (current(captured, gen)) { inFlight = undefined; edits = []; }
        });
        return inFlight;
      },
    };
  });
}

let browserStorage: Storage | undefined;
try { browserStorage = localStorage; } catch { /* private mode */ }
export const usePrefs = createPrefsStore({
  scope: currentRoutingScope,
  supported: async (scope) => (await api.healthScoped(scope)).features?.includes('ui-prefs.v1') === true,
  get: api.getUIPrefs,
  put: api.putUIPrefs,
  legacy: async () => ({ prefs: (await api.getUserPrefs()).prefs as UserPrefs }),
  paused: () => useEntitlements.getState().viewPersonal,
}, browserStorage);

export function hydratePrefs(): void {
  void usePrefs.getState().hydrateFromDaemon(useCloud.getState().session?.user?.id ?? null);
}
// Defer until routing/token configuration has completed in the same call stack.
onRoutingScopeChange(() => queueMicrotask(hydratePrefs));
let lastToken: string | null = null;
useCloud.subscribe((s) => {
  const token = s.session?.access_token ?? null;
  if (token !== lastToken) { lastToken = token; queueMicrotask(hydratePrefs); }
});
let lastPersonal = useEntitlements.getState().viewPersonal;
useEntitlements.subscribe((s) => {
  const was = lastPersonal;
  lastPersonal = s.viewPersonal;
  if (was && !s.viewPersonal) hydratePrefs();
});
