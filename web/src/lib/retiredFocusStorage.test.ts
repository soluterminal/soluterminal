import { describe, expect, it } from 'vitest';
import { clearRetiredFocusStorage } from './retiredFocusStorage';

function memoryStorage(entries: Record<string, string>): Storage {
  const map = new Map(Object.entries(entries));
  return {
    get length() { return map.size; },
    key: (i: number) => Array.from(map.keys())[i] ?? null,
    getItem: (k: string) => (map.has(k) ? map.get(k)! : null),
    setItem: (k: string, v: string) => { map.set(k, v); },
    removeItem: (k: string) => { map.delete(k); },
    clear: () => map.clear(),
  };
}

describe('clearRetiredFocusStorage', () => {
  it('removes the Focus keys and keeps everything else', () => {
    const local = memoryStorage({
      'solu.focusFontOffset': '2',
      'solu.focusHintDismissed': '1',
      'solu.knownSlashCommands': '["/review"]',
      'solu.knownSlashCommands.codex': '[]',
      'solu.knownSlashCommands.muse': '[]',
      'solu.nativeMetaBar': '{}',
      'solu.nativePerf': '1',
      'solu.nativeVitals.pane-a': '{}',
      'solu.nativeVitals.pane-b': '{}',
      'solu.terminalTheme': 'dark',
      'solu.native': 'not a Focus key',
      'other.app': 'x',
    });
    const session = memoryStorage({
      'solu.native.draft.pane-a': 'half-typed',
      'solu.embedWindow': 'shinoque',
    });
    expect(clearRetiredFocusStorage(local, session)).toBe(10);
    expect([...Array(local.length).keys()].map((i) => local.key(i)).sort()).toEqual(['other.app', 'solu.native', 'solu.terminalTheme']);
    expect(session.getItem('solu.native.draft.pane-a')).toBeNull();
    expect(session.getItem('solu.embedWindow')).toBe('shinoque');
    expect(clearRetiredFocusStorage(local, session)).toBe(0);
  });

  it('tolerates missing or failing storage', () => {
    const throwing = memoryStorage({});
    throwing.getItem = () => { throw new Error('blocked'); };
    expect(clearRetiredFocusStorage(undefined, undefined)).toBe(0);
    expect(clearRetiredFocusStorage(throwing, undefined)).toBe(0);
  });
});
