// The Focus view (the native renderer) was removed. It kept per-browser state
// that nothing reads any more: preferences, a slash-command cache, per-pane
// vitals snapshots, and per-tab unsent drafts. Clear it on every load (it is
// idempotent and cheap) so it does not linger in every browser that ever used
// Focus. No other Focus key was ever written: there were no queued-message keys.

const LOCAL_KEYS = [
  'solu.focusFontOffset',
  'solu.focusHintDismissed',
  'solu.knownSlashCommands',
  'solu.knownSlashCommands.codex',
  'solu.knownSlashCommands.muse',
  'solu.nativeMetaBar',
  'solu.nativePerf',
];
const LOCAL_PREFIXES = ['solu.nativeVitals.'];
const SESSION_PREFIXES = ['solu.native.draft.'];

function clearFrom(storage: Storage | undefined, keys: string[], prefixes: string[]): number {
  if (!storage) return 0;
  let removed = 0;
  try {
    for (const key of keys) {
      if (storage.getItem(key) !== null) {
        storage.removeItem(key);
        removed++;
      }
    }
    const matching: string[] = [];
    for (let i = 0; i < storage.length; i++) {
      const key = storage.key(i);
      if (key && prefixes.some((prefix) => key.startsWith(prefix))) matching.push(key);
    }
    for (const key of matching) {
      storage.removeItem(key);
      removed++;
    }
  } catch {
    // Storage unavailable (privacy mode, blocked site data): nothing to clear.
  }
  return removed;
}

function storageOrUndefined(get: () => Storage): Storage | undefined {
  try {
    return get();
  } catch {
    return undefined;
  }
}

/** Removes the retired Focus view's keys; returns how many it removed. */
export function clearRetiredFocusStorage(
  local: Storage | undefined = storageOrUndefined(() => window.localStorage),
  session: Storage | undefined = storageOrUndefined(() => window.sessionStorage),
): number {
  return clearFrom(local, LOCAL_KEYS, LOCAL_PREFIXES) + clearFrom(session, [], SESSION_PREFIXES);
}
