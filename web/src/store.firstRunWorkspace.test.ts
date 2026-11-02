import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, setApiPrincipalAccess, setBackend } from './api';
import { setCloudDualBackend } from './dualBackend';
import { useStore } from './store';
import type { PaneInfo, Workspace } from './types';

const pane = (id: string): PaneInfo => ({
  id, title: id, cwd: '/tmp', cmd: ['bash'], status: 'running',
  pid: 1, cols: 80, rows: 24,
} as PaneInfo);

const workspace = (name: string, over: Partial<Workspace> = {}): Workspace => ({
  name, layout: null, paneIds: [], activeAt: '2026-10-20T00:00:00Z', ...over,
});

async function bootstrapWith(panes: PaneInfo[], workspaces: Workspace[]) {
  vi.spyOn(api, 'listPanes').mockResolvedValue(panes);
  vi.spyOn(api, 'listWorkspaces').mockResolvedValue(workspaces);
  vi.spyOn(api, 'listPorts').mockResolvedValue([]);
  for (const method of ['refreshUpdateStatus', 'loadRecoverySettings', 'loadNativeSettings',
    'loadUpdateSettings', 'loadOrchestratorHosts', 'refreshSchedules'] as const) {
    vi.spyOn(useStore.getState(), method).mockResolvedValue(undefined);
  }
  await useStore.getState().bootstrap();
}

beforeEach(() => {
  localStorage.clear();
  setCloudDualBackend(false);
  setBackend(null);
  setApiPrincipalAccess(true);
  useStore.setState({ panes: {}, layout: null, activeWorkspace: '_current', activeBackend: null,
    parkedPaneIds: [], mainPresentation: { kind: 'workspace' }, navigationGeneration: 0,
    managedOwnerBindings: new Map() });
});
afterEach(() => vi.restoreAllMocks());

describe('first-run workspace landing', () => {
  it('lands in the seeded empty Default so its row counts the open panes', async () => {
    await bootstrapWith([pane('a'), pane('b')], [workspace('Default', { protected: true })]);
    const state = useStore.getState();
    expect(state.activeWorkspace).toBe('Default');
    expect(state.layout).not.toBeNull();
  });

  it('keeps an existing _current layout as the landing place', async () => {
    await bootstrapWith([pane('a')], [
      workspace('Default', { protected: true }),
      workspace('_current', { layout: { type: 'pane', paneId: 'a' }, paneIds: ['a'], activeAt: '2026-10-19T00:00:00Z' }),
    ]);
    expect(useStore.getState().activeWorkspace).toBe('_current');
  });
});
