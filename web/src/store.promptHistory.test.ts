import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, currentRoutingScope, setBackend, setApiPrincipalAccess } from './api';
import { setCloudDualBackend } from './dualBackend';
import { terminalSessionKey, useStore } from './store';
import type { PaneInfo } from './types';

const pane = (id: string, prompts: string[] = []): PaneInfo => ({
  id, title: id, cwd: '/tmp', cmd: ['codex'], status: 'running',
  pid: 1, cols: 80, rows: 24, lastPrompts: prompts,
} as PaneInfo);

beforeEach(() => {
  setCloudDualBackend(false);
  setBackend(null);
  setApiPrincipalAccess(true);
  useStore.setState({ panes: {}, lastPrompts: {}, layout: null,
    activeBackend: null, parkedPaneIds: [], mainPresentation: { kind: 'workspace' },
    navigationGeneration: 0, managedOwnerBindings: new Map() });
});
afterEach(() => {
  vi.restoreAllMocks();
  setCloudDualBackend(false);
  setBackend(null);
});

describe('scoped pane prompt history', () => {
  it('feeds the scoped key selected by the terminal pane', () => {
    const key = terminalSessionKey(currentRoutingScope(), 'same');
    useStore.getState().setPaneLastPrompts('same', ['latest', 'earlier']);
    expect(useStore.getState().lastPrompts[key]).toEqual(['latest', 'earlier']);
    expect(useStore.getState().lastPrompts.same).toBeUndefined();
  });

  it('separates identical pane IDs across backends and rejects stale SSE updates', () => {
    setBackend('wsl');
    const wsl = currentRoutingScope();
    useStore.getState().setPaneLastPrompts('same', ['WSL prompt'], wsl);
    setBackend('win');
    const win = currentRoutingScope();
    useStore.getState().setPaneLastPrompts('same', ['Windows prompt'], win);
    useStore.getState().setPaneLastPrompts('same', ['late WSL event'], wsl);
    expect(useStore.getState().lastPrompts[terminalSessionKey(wsl, 'same')]).toEqual(['WSL prompt']);
    expect(useStore.getState().lastPrompts[terminalSessionKey(win, 'same')]).toEqual(['Windows prompt']);
  });

  it('hydrates history during initial bootstrap', async () => {
    vi.spyOn(api, 'listPanes').mockResolvedValue([pane('loaded', ['persisted prompt'])]);
    vi.spyOn(api, 'listWorkspaces').mockResolvedValue([]);
    vi.spyOn(api, 'listPorts').mockResolvedValue([]);
    for (const method of ['refreshUpdateStatus', 'loadRecoverySettings', 'loadNativeSettings',
      'loadUpdateSettings', 'loadOrchestratorHosts', 'refreshSchedules'] as const) {
      vi.spyOn(useStore.getState(), method).mockResolvedValue(undefined);
    }
    await useStore.getState().bootstrap();
    expect(useStore.getState().lastPrompts[terminalSessionKey(currentRoutingScope(), 'loaded')])
      .toEqual(['persisted prompt']);
  });

  it('heals lost history by polling even when pane metadata is unchanged', async () => {
    const saved = pane('same', ['persisted prompt']);
    useStore.setState({ panes: { same: saved }, lastPrompts: {} });
    vi.spyOn(api, 'listPanes').mockResolvedValue([{ ...saved }]);
    await useStore.getState().refreshPanes();
    const key = terminalSessionKey(currentRoutingScope(), 'same');
    expect(useStore.getState().lastPrompts[key]).toEqual(['persisted prompt']);
    expect(useStore.getState().panes.same).toBe(saved);
    const history = useStore.getState().lastPrompts;
    await useStore.getState().refreshPanes();
    expect(useStore.getState().lastPrompts).toBe(history);
  });

  it('clears an empty DTO history even when pane metadata is unchanged', async () => {
    const saved = pane('same');
    useStore.setState({ panes: { same: saved } });
    useStore.getState().setPaneLastPrompts('same', ['stale prompt']);
    vi.spyOn(api, 'listPanes').mockResolvedValue([saved]);
    await useStore.getState().refreshPanes();
    expect(useStore.getState().lastPrompts[terminalSessionKey(currentRoutingScope(), 'same')]).toBeUndefined();
  });

  it('prunes only the confirmed scope and preserves the other backend', async () => {
    setBackend('win');
    const winKey = terminalSessionKey(currentRoutingScope(), 'same');
    useStore.getState().setPaneLastPrompts('same', ['Windows prompt']);
    setBackend('wsl');
    const wsl = currentRoutingScope();
    useStore.getState().setPaneLastPrompts('gone', ['deleted prompt']);
    vi.spyOn(api, 'listPanes').mockResolvedValue([pane('same', ['WSL prompt'])]);
    await useStore.getState().refreshPanes();
    expect(useStore.getState().lastPrompts[winKey]).toEqual(['Windows prompt']);
    expect(useStore.getState().lastPrompts[terminalSessionKey(wsl, 'same')]).toEqual(['WSL prompt']);
    expect(useStore.getState().lastPrompts[terminalSessionKey(wsl, 'gone')]).toBeUndefined();
  });

  it('does not erase history when the active backend snapshot is incomplete', async () => {
    setCloudDualBackend(true);
    setBackend('wsl');
    useStore.setState({ activeBackend: 'wsl' });
    const key = terminalSessionKey(currentRoutingScope(), 'same');
    useStore.getState().setPaneLastPrompts('same', ['WSL prompt']);
    vi.spyOn(api, 'listPanesAllBackends').mockResolvedValue(Object.assign(
      [{ ...pane('same', ['Windows prompt']), backend: 'win' as const }],
      { completeBackends: ['win' as const] },
    ));
    await useStore.getState().refreshPanes();
    expect(useStore.getState().lastPrompts[key]).toEqual(['WSL prompt']);
  });

  it('discards delayed polling results after a backend switch', async () => {
    setBackend('wsl');
    let resolve!: (panes: PaneInfo[]) => void;
    vi.spyOn(api, 'listPanes').mockReturnValue(new Promise(done => { resolve = done; }));
    const pending = useStore.getState().refreshPanes();
    setBackend('win');
    const key = terminalSessionKey(currentRoutingScope(), 'same');
    useStore.getState().setPaneLastPrompts('same', ['Windows prompt']);
    resolve([pane('same', ['late WSL response'])]);
    await pending;
    expect(useStore.getState().lastPrompts[key]).toEqual(['Windows prompt']);
  });

  it('cleans up and restores the exact scoped history when deletion fails', async () => {
    const saved = pane('same', ['WSL prompt']);
    setBackend('win');
    const winKey = terminalSessionKey(currentRoutingScope(), 'same');
    useStore.getState().setPaneLastPrompts('same', ['Windows prompt']);
    setBackend('wsl');
    useStore.setState({ panes: { same: saved }, layout: { type: 'pane', paneId: 'same' } });
    useStore.getState().setPaneLastPrompts('same', saved.lastPrompts!);
    const wslKey = terminalSessionKey(currentRoutingScope(), 'same');
    let reject!: (error: Error) => void;
    vi.spyOn(api, 'deletePane').mockReturnValue(new Promise((_, fail) => { reject = fail; }));
    const pending = useStore.getState().killPane('same', { silent: true });
    expect(useStore.getState().lastPrompts[wslKey]).toBeUndefined();
    expect(useStore.getState().lastPrompts[winKey]).toEqual(['Windows prompt']);
    reject(new Error('delete failed'));
    expect(await pending).toBe(false);
    expect(useStore.getState().lastPrompts[wslKey]).toEqual(['WSL prompt']);
  });
});
