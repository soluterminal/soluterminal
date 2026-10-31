import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, setApiPrincipalAccess, setBackend } from './api';
import { setCloudDualBackend } from './dualBackend';
import { useStore } from './store';
import { collectPaneIds } from './lib/layout';
import type { PaneInfo, Workspace } from './types';

const pane = (id: string): PaneInfo => ({ id, pid: 1, status: 'running', cwd: '/repo', cmd: ['bash'], title: id, cols: 80, rows: 24, createdAt: '', lastActivityMs: 0, state: 'idle', hasClaude: false });
const starter = pane('starter');
const other = pane('other');
const worker = { ...pane('worker'), origin: { source: 'agent' as const, byPaneId: 'starter', at: 1 } };
const original: Workspace = {
  name: 'Desk', activeAt: '', paneIds: ['starter', 'other'], panes: [starter, other].map(p => ({ ...p, kind: undefined })),
  layout: { type: 'split', direction: 'vertical', ratio: 0.35, a: { type: 'pane', paneId: 'starter' }, b: { type: 'pane', paneId: 'other' } },
};
const updated: Workspace = {
  ...original, paneIds: ['starter', 'other', 'worker'], panes: [starter, worker, other].map(p => ({ ...p, kind: undefined })),
  layout: { type: 'split', direction: 'vertical', ratio: 0.35,
    a: { type: 'split', direction: 'horizontal', ratio: 0.5, a: { type: 'pane', paneId: 'starter' }, b: { type: 'pane', paneId: 'worker' } },
    b: { type: 'pane', paneId: 'other' } },
};

beforeEach(() => {
  vi.useFakeTimers();
  localStorage.clear(); setCloudDualBackend(false); setBackend(null); setApiPrincipalAccess(true);
  useStore.setState({
    activeWorkspace: 'Desk', activeWorkspaceMoveVersion: 0, activeWorkspaceKnownPaneIds: ['starter', 'other'], activeBackend: null,
    layout: original.layout, panes: { starter, other }, workspaces: [original],
    mainPresentation: { kind: 'workspace' }, managedOwnerBindings: new Map(), navigationGeneration: 0,
    focusedPaneId: 'starter', lastUserFocusId: null, zoomedPaneId: 'starter',
    browsers: {}, files: {}, parkedPaneIds: [], updateStatus: null, wsOrder: [],
  });
  vi.spyOn(api, 'listWorkspaces').mockResolvedValue([updated]);
  vi.spyOn(api, 'listWorkspaceSummaries').mockResolvedValue([updated]);
  vi.spyOn(api, 'saveWorkspace').mockImplementation(async (w) => w);
});
afterEach(async () => {
  await vi.advanceTimersByTimeAsync(500);
  vi.useRealTimers(); vi.restoreAllMocks();
});

describe('server spawned panes in the open workspace', () => {
  it('adds beside the starter live and preserves local edits and keyboard focus', async () => {
    // The user removed other locally, and has an unsaved resize.
    const local = { type: 'pane' as const, paneId: 'starter' };
    useStore.setState({ layout: local });
    await useStore.getState().refreshWorkspaces({ full: true });
    const s = useStore.getState();
    expect(collectPaneIds(s.layout)).toEqual(['starter', 'worker']);
    expect(s.panes.worker.origin?.byPaneId).toBe('starter');
    expect(s.activeWorkspaceKnownPaneIds).toEqual(['starter', 'other', 'worker']);
    expect(s.focusedPaneId).toBe('starter');
    expect(s.lastUserFocusId).toBeNull();
    expect(s.zoomedPaneId).toBe('starter');
    await s.refreshWorkspaces({ full: true });
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['starter', 'worker']);
  });

  it('refreshes a newly empty workspace when the daemon returns a null pane list', async () => {
    const empty: Workspace = { ...original, paneIds: null as unknown as string[], panes: [], layout: null, activeAt: 'new-empty-workspace' };
    useStore.setState({ layout: null, activeWorkspaceKnownPaneIds: [] });
    vi.mocked(api.listWorkspaces).mockResolvedValue([empty]);
    await useStore.getState().refreshWorkspaces({ full: true });
    expect(useStore.getState().workspaces).toEqual([empty]);
    expect(useStore.getState().layout).toBeNull();
  });

  it('keeps Clear layout empty when the old scratchpad is refreshed before its save', async () => {
    const scratch: Workspace = { ...updated, name: '_current' };
    useStore.setState({ workspaces: [original, scratch] });
    vi.mocked(api.listWorkspaces).mockResolvedValue([original, scratch]);
    vi.mocked(api.listWorkspaceSummaries).mockResolvedValue([original, scratch]);
    useStore.getState().clearLayout();
    await useStore.getState().refreshWorkspaces({ full: true });
    expect(useStore.getState().layout).toBeNull();
    await vi.advanceTimersByTimeAsync(401);
    expect(api.saveWorkspace).toHaveBeenCalledWith(expect.objectContaining({ name: '_current', layout: null, knownPaneIds: [], paneIds: [] }), expect.anything());
    expect(useStore.getState().layout).toBeNull();
  });

  it('does not import the old scratchpad when deleting the active named workspace', async () => {
    const scratch: Workspace = { ...updated, name: '_current' };
    vi.spyOn(api, 'deleteWorkspace').mockResolvedValue(undefined);
    vi.mocked(api.listWorkspaces).mockResolvedValue([scratch]);
    await useStore.getState().deleteWorkspace('Desk');
    expect(useStore.getState().activeWorkspace).toBe('_current');
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['starter', 'other']);
  });

  it('does not escalate every summary poll to a full fetch during a pane-move conflict', async () => {
    vi.mocked(api.listWorkspaceSummaries).mockResolvedValue([{ ...updated, paneMoveVersion: 1 }]);
    await useStore.getState().refreshWorkspaces();
    expect(api.listWorkspaces).not.toHaveBeenCalled();
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['starter', 'other']);
  });

  it('heals a missed event through the existing summary poll', async () => {
    useStore.setState({ paneLayoutMode: 'free' });
    await useStore.getState().refreshWorkspaces();
    expect(api.listWorkspaces).toHaveBeenCalledOnce();
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['starter', 'other', 'worker']);
    expect(useStore.getState().layout).toMatchObject({ direction: 'vertical', ratio: 0.35 });
  });

  it('sends the observed set on a stale autosave, then applies preserved additions from its response', async () => {
    vi.mocked(api.saveWorkspace).mockResolvedValue(updated);
    useStore.getState().setLayout(original.layout);
    await vi.advanceTimersByTimeAsync(401);
    expect(api.saveWorkspace).toHaveBeenCalledWith(expect.objectContaining({
      name: 'Desk', knownPaneIds: ['starter', 'other'], paneIds: ['starter', 'other'],
    }), expect.anything());
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['starter', 'other', 'worker']);
  });

  it('pins observations to the outgoing workspace when switching before a spawn event arrives', async () => {
    const elsewhere: Workspace = { name: 'Elsewhere', activeAt: '', paneIds: ['elsewhere'], panes: [{ ...pane('elsewhere'), kind: undefined }], layout: { type: 'pane', paneId: 'elsewhere' } };
    useStore.setState({ workspaces: [original, elsewhere] });
    vi.spyOn(api, 'listPanes').mockResolvedValue([starter, other, pane('elsewhere')]);
    vi.mocked(api.saveWorkspace).mockImplementation(async (w) => w.name === 'Desk' ? updated : w);
    vi.mocked(api.listWorkspaceSummaries).mockResolvedValue([updated, elsewhere]);
    useStore.getState().setLayout(original.layout);
    await useStore.getState().loadWorkspace('Elsewhere');
    await vi.advanceTimersByTimeAsync(1);
    expect(api.saveWorkspace).toHaveBeenCalledWith(expect.objectContaining({ name: 'Desk', knownPaneIds: ['starter', 'other'], paneIds: ['starter', 'other'] }), expect.anything());
    expect(useStore.getState().activeWorkspace).toBe('Elsewhere');
    expect(useStore.getState().workspaces.find(w => w.name === 'Desk')?.paneIds).toContain('worker');
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['elsewhere']);
  });

  it('never resurrects a pane the user already saw and removed', async () => {
    useStore.setState({ activeWorkspaceKnownPaneIds: ['starter', 'other', 'worker'] });
    await useStore.getState().refreshWorkspaces({ full: true });
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['starter', 'other']);
  });

  for (const source of ['poll', 'save', 'full'] as const) {
    it(`discards a worker closed by its starter on ${source}, including its recovery tile`, async () => {
      const closed: Workspace = { ...original, layoutVersion: 1, layoutMode: 'auto', removedPaneIds: ['worker'] };
      const recovery = { ...worker, restorePending: true, recoveryError: 'No launch recipe was saved' };
      useStore.setState({
        layout: updated.layout, panes: { starter, other, worker: recovery }, workspaces: [updated],
        activeWorkspaceLayoutVersion: 0, workspaceLayoutDirty: false, paneLayoutMode: 'auto', layoutColumns: 4, layoutAspect: 1.6,
        activeWorkspaceKnownPaneIds: ['starter', 'other', 'worker'],
        focusedPaneId: 'worker', lastUserFocusId: 'worker', zoomedPaneId: 'worker', parkedPaneIds: ['worker'],
      });
      vi.mocked(api.listWorkspaces).mockResolvedValue([closed]);
      vi.mocked(api.listWorkspaceSummaries).mockResolvedValue([closed]);
      vi.mocked(api.saveWorkspace).mockResolvedValue(closed);
      if (source === 'save') {
        useStore.getState().setLayout(updated.layout);
        await vi.advanceTimersByTimeAsync(401);
        expect(api.saveWorkspace).toHaveBeenCalledWith(expect.objectContaining({ paneIds: ['starter', 'worker', 'other'] }), expect.anything());
      } else {
        await useStore.getState().refreshWorkspaces(source === 'full' ? { full: true } : undefined);
      }
      const s = useStore.getState();
      expect(collectPaneIds(s.layout)).toEqual(['starter', 'other']);
      expect(s.panes.worker).toBeUndefined();
      expect(s.zoomedPaneId).toBeNull();
      expect(s.focusedPaneId).toBe('starter');
      expect(s.lastUserFocusId).toBeNull();
      expect(s.parkedPaneIds).toEqual([]);
      expect(s.workspaces.find(w => w.name === 'Desk')?.paneIds).not.toContain('worker');
    });
  }

  it('removes the final closed pane without importing old scratchpad panes', async () => {
    const closed: Workspace = { name: '_current', activeAt: '', paneIds: [], layout: null, removedPaneIds: ['worker'] };
    useStore.setState({ activeWorkspace: '_current', layout: { type: 'pane', paneId: 'worker' }, panes: { worker }, workspaces: [closed] });
    vi.mocked(api.listWorkspaceSummaries).mockResolvedValue([closed]);
    vi.mocked(api.listWorkspaces).mockResolvedValue([closed]);
    await useStore.getState().refreshWorkspaces();
    expect(useStore.getState().layout).toBeNull();
    expect(useStore.getState().panes.worker).toBeUndefined();
  });

  it('keeps an unsaved local pane when reconciling a server removal', async () => {
    const local = pane('local');
    useStore.setState({
      layout: { type: 'split', direction: 'horizontal', ratio: 0.5, a: updated.layout!, b: { type: 'pane', paneId: 'local' } },
      panes: { starter, other, worker, local }, activeWorkspaceKnownPaneIds: ['starter', 'other', 'worker', 'local'],
    });
    vi.mocked(api.listWorkspaces).mockResolvedValue([{ ...original, removedPaneIds: ['worker'] }]);
    await useStore.getState().refreshWorkspaces({ full: true });
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['starter', 'other', 'local']);
    expect(useStore.getState().panes.local).toBe(local);
  });

  it('does not apply additions to another workspace after navigation', async () => {
    vi.mocked(api.listWorkspaces).mockImplementation(async () => {
      useStore.setState({ activeWorkspace: 'Elsewhere', activeWorkspaceKnownPaneIds: ['other'], layout: { type: 'pane', paneId: 'other' } });
      return [updated];
    });
    await useStore.getState().refreshWorkspaces({ full: true });
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['other']);
  });

  it('keeps a conflicting pane-move generation for explicit reload', async () => {
    vi.mocked(api.listWorkspaces).mockResolvedValue([{ ...updated, paneMoveVersion: 1 }]);
    await useStore.getState().refreshWorkspaces({ full: true });
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['starter', 'other']);
    expect(useStore.getState().activeWorkspaceMoveVersion).toBe(0);
  });

  it('show scrolls into view and clears zoom without moving selection or the caret', async () => {
    await useStore.getState().refreshWorkspaces({ full: true });
    const element = document.createElement('div'); element.dataset.paneId = 'worker';
    element.scrollIntoView = vi.fn(); document.body.appendChild(element);
    const input = document.createElement('input'); document.body.appendChild(input); input.focus();
    useStore.getState().showSpawnedPane('worker', 'Desk');
    await vi.advanceTimersByTimeAsync(50);
    expect(element.scrollIntoView).toHaveBeenCalled();
    expect(useStore.getState().focusedPaneId).toBe('starter');
    expect(useStore.getState().lastUserFocusId).toBeNull();
    expect(useStore.getState().zoomedPaneId).toBeNull();
    expect(document.activeElement).toBe(input);
    input.remove(); element.remove();
  });

  it('show leaves a different workspace and owner view alone', () => {
    useStore.getState().showSpawnedPane('worker', 'Elsewhere');
    expect(useStore.getState().zoomedPaneId).toBe('starter');
    expect(useStore.getState().activeWorkspace).toBe('Desk');
  });
});
