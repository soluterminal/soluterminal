import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError, currentRoutingScope, setApiPrincipalAccess, setBackend } from './api';
import { useStore } from './store';
import { setCloudDualBackend } from './dualBackend';
import { collectPaneIds } from './lib/layout';
import { removeCustomPane, splitLargestPane } from './lib/autoLayout';
import type { PaneInfo, Workspace } from './types';

const pane = (id: string) => ({ id, pid: id === 'p' ? 1 : 2, status: 'running', cwd: '/repo', cmd: ['bash'], title: id, cols: 80, rows: 24 } as PaneInfo);
const workspace = (name: string, ids: string[]): Workspace => ({
  name, paneIds: ids, panes: ids.map(id => ({ id, cwd: '/repo', cmd: ['bash'], title: id, cols: 80, rows: 24 })),
  layout: ids.length === 0 ? null : ids.length === 1 ? { type: 'pane', paneId: ids[0] }
    : { type: 'split', direction: 'horizontal', ratio: 0.4, a: { type: 'pane', paneId: ids[0] }, b: { type: 'pane', paneId: ids[1] } },
  activeAt: '2026-10-24T00:00:00Z',
});
let source: Workspace;
let destination: Workspace;
const originalToast = useStore.getState().toast;
const originalOpenDialog = useStore.getState().openDialog;

beforeEach(() => {
  vi.useFakeTimers();
  localStorage.clear();
  setCloudDualBackend(false); setBackend(null); setApiPrincipalAccess(true);
  source = workspace('Source', ['p', 'keep']); destination = workspace('Destination', ['other']);
  useStore.setState({
    toast: originalToast, openDialog: originalOpenDialog, toasts: [],
    activeWorkspace: 'Source', activeWorkspaceMoveVersion: 0, activeWorkspaceLayoutVersion: 0, activeWorkspaceKnownPaneIds: ['p', 'keep'], workspaceLayoutDirty: false, paneLayoutMode: 'auto', layoutColumns: 4, layoutAspect: 1.6, activeBackend: null,
    layout: source.layout, panes: { p: pane('p'), keep: pane('keep'), other: pane('other') },
    workspaces: [source, destination], managedOwnerBindings: new Map(), mainPresentation: { kind: 'workspace' },
    focusedPaneId: 'p', lastUserFocusId: 'p', zoomedPaneId: 'p', navigationGeneration: 0,
    files: {}, browsers: {}, parkedPaneIds: [], updateStatus: null,
  });
  vi.spyOn(api, 'saveWorkspace').mockImplementation(async w => w);
  vi.spyOn(api, 'listPanes').mockResolvedValue([pane('p'), pane('keep'), pane('other')]);
  vi.spyOn(api, 'listWorkspaceSummaries').mockResolvedValue([source, destination]);
  vi.spyOn(api, 'listWorkspaces').mockImplementation(async () => useStore.getState().workspaces);
});
afterEach(async () => {
  // Flush any scheduled source save before replacing mocks in the next case.
  await vi.advanceTimersByTimeAsync(500);
  vi.useRealTimers(); vi.restoreAllMocks();
});

function moved() {
  return {
    source: { ...workspace('Source', ['keep']), paneMoveVersion: 1 },
    destination: { ...workspace('Destination', ['other', 'p']), paneMoveVersion: 1 },
  };
}

describe('pane workspace reassignment', () => {
  it('warns about a stale view and reloads only after the user chooses to replace its layout', async () => {
    vi.mocked(api.saveWorkspace).mockRejectedValue(new ApiError(409, 'workspace changed after a pane move'));
    useStore.getState().setLayout(source.layout);
    await vi.advanceTimersByTimeAsync(500);
    const warning = useStore.getState().toasts.find(toast => toast.message.includes('not being saved'));
    expect(warning?.action?.label).toBe('Reload layout');
    expect(warning?.ttlMs).toBe(0);
    expect(useStore.getState().layout).toEqual(source.layout);
    expect(useStore.getState().activeWorkspaceMoveVersion).toBe(0);
    const result = moved();
    vi.mocked(api.listWorkspaces).mockResolvedValue([result.source, result.destination]);
    const confirm = vi.spyOn(useStore.getState(), 'openDialog').mockImplementation(request => request.resolve(true));
    warning?.action?.run();
    await vi.advanceTimersByTimeAsync(1);
    expect(confirm).toHaveBeenCalledWith(expect.objectContaining({ title: 'Reload workspace layout?' }));
    expect(useStore.getState().activeWorkspaceMoveVersion).toBe(1);
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['keep']);
    vi.mocked(api.saveWorkspace).mockImplementation(async value => value);
    useStore.getState().setLayout(result.source.layout);
    await vi.advanceTimersByTimeAsync(500);
    expect(api.saveWorkspace).toHaveBeenLastCalledWith(expect.objectContaining({ name: 'Source', paneMoveVersion: 1 }), currentRoutingScope());
  });

  it('keeps local layout edits if the user declines the conflict reload', async () => {
    vi.mocked(api.saveWorkspace).mockRejectedValue(new ApiError(409, 'workspace changed after a pane move'));
    useStore.getState().setLayout(source.layout);
    await vi.advanceTimersByTimeAsync(500);
    const warning = useStore.getState().toasts.find(toast => toast.message.includes('not being saved'));
    expect(warning?.action).toBeDefined();
    vi.spyOn(useStore.getState(), 'openDialog').mockImplementation(request => request.resolve(false));
    warning?.action?.run();
    await vi.advanceTimersByTimeAsync(1);
    expect(useStore.getState().layout).toEqual(source.layout);
    expect(useStore.getState().activeWorkspaceMoveVersion).toBe(0);
    expect(api.listWorkspaces).not.toHaveBeenCalled();
  });

  it('persists first, moves the same pane, and opens/focuses the destination', async () => {
    const move = vi.spyOn(api, 'movePaneToWorkspace').mockResolvedValue(moved());
    const create = vi.spyOn(api, 'createPane');
    const original = useStore.getState().panes.p;
    await useStore.getState().movePaneToWorkspace('p', 'Destination');
    expect(api.saveWorkspace).toHaveBeenCalledWith(expect.objectContaining({ name: 'Source', paneMoveVersion: 0 }), currentRoutingScope());
    expect(move).toHaveBeenCalledWith('Source', 'p', 'Destination', 0, currentRoutingScope());
    const state = useStore.getState();
    expect(state.activeWorkspace).toBe('Destination');
    expect(state.focusedPaneId).toBe('p');
    expect(state.activeWorkspaceMoveVersion).toBe(1);
    expect(collectPaneIds(state.layout)).toEqual(['other', 'p']);
    expect(state.workspaces.find(w => w.name === 'Source')?.paneIds).toEqual(['keep']);
    expect(state.panes.p.pid).toBe(original.pid);
    expect(create).not.toHaveBeenCalled();
  });

  it('moves the last pane without reviving it through the outgoing autosave', async () => {
    source = workspace('Source', ['p']);
    useStore.setState({ layout: source.layout, workspaces: [source, destination] });
    vi.spyOn(api, 'movePaneToWorkspace').mockResolvedValue({ ...moved(), source: { ...workspace('Source', []), paneMoveVersion: 1 } });
    await useStore.getState().movePaneToWorkspace('p', 'Destination');
    expect(useStore.getState().activeWorkspace).toBe('Destination');
    expect(useStore.getState().workspaces.find(w => w.name === 'Source')?.layout).toBeNull();
    await vi.advanceTimersByTimeAsync(500);
    const save = vi.mocked(api.saveWorkspace);
    const moveOrder = vi.mocked(api.movePaneToWorkspace).mock.invocationCallOrder[0];
    save.mock.calls.forEach(([w], index) => {
      if (w.name === 'Source') expect(save.mock.invocationCallOrder[index]).toBeLessThan(moveOrder);
    });
  });

  it('leaves the current layout and focus unchanged on a failed move', async () => {
    vi.spyOn(api, 'movePaneToWorkspace').mockRejectedValue(new Error('storage unavailable'));
    const toast = vi.spyOn(useStore.getState(), 'toast');
    await useStore.getState().movePaneToWorkspace('p', 'Destination');
    expect(useStore.getState().layout).toEqual(source.layout);
    expect(useStore.getState().focusedPaneId).toBe('p');
    expect(useStore.getState().activeWorkspace).toBe('Source');
    expect(toast).toHaveBeenCalledWith(expect.stringContaining("Couldn't move pane"), expect.objectContaining({ kind: 'error' }));
  });

  it('does not move when saving the source fails', async () => {
    vi.mocked(api.saveWorkspace).mockRejectedValue(new Error('storage unavailable'));
    const move = vi.spyOn(api, 'movePaneToWorkspace');
    await useStore.getState().movePaneToWorkspace('p', 'Destination');
    expect(move).not.toHaveBeenCalled();
    expect(useStore.getState().layout).toEqual(source.layout);
  });

  it('does not change another view after navigation during the request', async () => {
    vi.spyOn(api, 'movePaneToWorkspace').mockImplementation(async () => {
      useStore.setState({ activeWorkspace: 'Elsewhere', navigationGeneration: 1, layout: { type: 'pane', paneId: 'other' } });
      return moved();
    });
    await useStore.getState().movePaneToWorkspace('p', 'Destination');
    expect(useStore.getState().activeWorkspace).toBe('Elsewhere');
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['other']);
  });

  it.each(['owner', 'different backend', 'missing destination'])('rejects %s before writing', async (kind) => {
    if (kind === 'owner') useStore.setState({ managedOwnerBindings: new Map([['p', 'task']]) });
    if (kind === 'different backend') useStore.setState({ activeBackend: 'wsl', workspaces: [source, { ...destination, backend: 'win' }] });
    const move = vi.spyOn(api, 'movePaneToWorkspace');
    await useStore.getState().movePaneToWorkspace('p', kind === 'missing destination' ? 'Missing' : 'Destination');
    expect(move).not.toHaveBeenCalled();
    expect(api.saveWorkspace).not.toHaveBeenCalled();
  });
});


describe('daemon initiated workspace changes', () => {
  it('removes a remotely moved pane live and retains its running identity', async () => {
    const result = moved();
    useStore.setState({ activeWorkspaceKnownPaneIds: ['p', 'keep'], workspaceLayoutDirty: true, activeWorkspaceLayoutVersion: 0 });
    useStore.getState().setLayout(source.layout);
    vi.mocked(api.listWorkspaces).mockResolvedValue([result.source, result.destination]);
    await useStore.getState().refreshWorkspaces({ full: true, paneMove: { paneId: 'p', source: 'Source', destination: 'Destination' } });
    const state = useStore.getState();
    expect(state.activeWorkspace).toBe('Source');
    expect(state.activeWorkspaceMoveVersion).toBe(1);
    expect(collectPaneIds(state.layout)).toEqual(['keep']);
    expect(state.panes.p.pid).toBe(1);
    expect(state.focusedPaneId).toBe('keep');
    expect(state.zoomedPaneId).toBeNull();
    await vi.advanceTimersByTimeAsync(500);
    expect(api.saveWorkspace).toHaveBeenCalledWith(expect.objectContaining({ name: 'Source', paneMoveVersion: 1, paneIds: ['keep'] }), expect.anything());
  });

  it('appends a remotely moved pane live in the destination from the move event', async () => {
    const result = moved();
    useStore.setState({ activeWorkspace: 'Destination', layout: destination.layout, activeWorkspaceKnownPaneIds: ['other'], focusedPaneId: 'other', activeWorkspaceLayoutVersion: 0, workspaceLayoutDirty: false });
    vi.mocked(api.listWorkspaceSummaries).mockResolvedValue([result.source, result.destination]);
    vi.mocked(api.listWorkspaces).mockResolvedValue([result.source, result.destination]);
    await useStore.getState().refreshWorkspaces({ full: true, paneMove: { paneId: 'p', source: 'Source', destination: 'Destination' } });
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['other', 'p']);
    expect(useStore.getState().activeWorkspaceMoveVersion).toBe(1);
    expect(useStore.getState().focusedPaneId).toBe('other');
  });

  it('migrates an active rename once and autosaves only under the new name', async () => {
    useStore.setState({ wsOrder: ['Source', 'Destination'], wsLastViewedMs: { Source: 7 } });
    useStore.getState().setLayout(source.layout);
    useStore.getState().applyWorkspaceNameChange('Source', 'Renamed');
    useStore.getState().applyWorkspaceNameChange('Source', 'Renamed');
    expect(useStore.getState().activeWorkspace).toBe('Renamed');
    expect(useStore.getState().wsOrder).toEqual(['Renamed', 'Destination']);
    expect(useStore.getState().wsLastViewedMs).toEqual({ Renamed: 7 });
    await vi.advanceTimersByTimeAsync(500);
    expect(api.saveWorkspace).not.toHaveBeenCalled();
    useStore.getState().setLayout(source.layout);
    await vi.advanceTimersByTimeAsync(500);
    expect(api.saveWorkspace).toHaveBeenCalledWith(expect.objectContaining({ name: 'Renamed' }), expect.anything());
  });

  it('drops active deleted name and queued autosave without killing panes', async () => {
    useStore.getState().setLayout(source.layout);
    useStore.getState().applyWorkspaceNameChange('Source');
    await vi.advanceTimersByTimeAsync(500);
    expect(useStore.getState().activeWorkspace).toBe('_current');
    expect(useStore.getState().workspaces.some(w => w.name === 'Source')).toBe(false);
    expect(useStore.getState().panes.p.pid).toBe(1);
    expect(api.saveWorkspace).not.toHaveBeenCalled();
  });

  it('keeps the UI rename path using the shared local name migration', async () => {
    vi.spyOn(api, 'renameWorkspace').mockResolvedValue(undefined);
    vi.mocked(api.listWorkspaces).mockResolvedValue([{ ...source, name: 'Renamed' }, destination]);
    await useStore.getState().renameWorkspace('Source', ' Renamed ');
    expect(api.renameWorkspace).toHaveBeenCalledWith('Source', 'Renamed');
    expect(useStore.getState().activeWorkspace).toBe('Renamed');
  });
});


describe('live moves preserve unsaved split sizes', () => {
  for (const activeWorkspace of ['Source', 'Destination']) {
    it(`keeps the resized ${activeWorkspace} and loaded peer geometry through move out and back, then saves with the new revision`, async () => {
      const sourceSaved: Workspace = { ...workspace('Source', ['p', 'keep']), layoutMode: 'free',
        paneIds: ['p', 'keep', 'side'], panes: ['p', 'keep', 'side'].map(id => ({ ...pane(id), kind: undefined })),
        layout: { type: 'split', direction: 'vertical', ratio: 0.4,
          a: { type: 'split', direction: 'horizontal', ratio: 0.5, a: { type: 'pane', paneId: 'p' }, b: { type: 'pane', paneId: 'keep' } },
          b: { type: 'pane', paneId: 'side' } },
      };
      const targetSaved: Workspace = { ...workspace('Destination', ['other', 'extra']), layoutMode: 'free', layout: { type: 'split', direction: 'vertical', ratio: 0.4, a: { type: 'pane', paneId: 'other' }, b: { type: 'pane', paneId: 'extra' } } };
      const sourceResized = { ...sourceSaved.layout!, ratio: 0.73 } as Workspace['layout'];
      const targetResized = { ...targetSaved.layout!, ratio: 0.67 } as Workspace['layout'];
      const activeSource = activeWorkspace === 'Source';
      useStore.setState({ activeWorkspace, layout: activeSource ? sourceResized : targetResized,
        activeWorkspaceKnownPaneIds: activeSource ? sourceSaved.paneIds : targetSaved.paneIds,
        paneLayoutMode: 'free', workspaceLayoutDirty: true,
        panes: Object.fromEntries(['p', 'keep', 'side', 'other', 'extra'].map(id => [id, pane(id)])),
        workspaces: [{ ...sourceSaved, layout: sourceResized }, { ...targetSaved, layout: targetResized }],
        focusedPaneId: activeSource ? 'keep' : 'other', zoomedPaneId: null, lastUserFocusId: null,
      });
      // The queued resize has not reached the server. Its snapshots still have
      // the original 0.4 ratio, unlike either loaded client layout.
      useStore.getState().setLayout(activeSource ? sourceResized : targetResized);
      const movedSource: Workspace = { ...sourceSaved, paneIds: ['keep', 'side'], panes: sourceSaved.panes?.filter(p => p.id !== 'p'), layout: removeCustomPane(sourceSaved.layout, 'p'), paneMoveVersion: 1, layoutVersion: 1 };
      const movedTarget: Workspace = { ...targetSaved, paneIds: ['other', 'extra', 'p'], panes: [...targetSaved.panes!, sourceSaved.panes![0]], layout: splitLargestPane(targetSaved.layout, 'p'), paneMoveVersion: 1, layoutVersion: 1 };
      vi.mocked(api.listWorkspaces).mockResolvedValue([movedSource, movedTarget]);
      await useStore.getState().refreshWorkspaces({ full: true, paneMove: { paneId: 'p', source: 'Source', destination: 'Destination' } });
      let state = useStore.getState();
      expect(state.layout).toMatchObject({ type: 'split', ratio: activeSource ? 0.73 : 0.67 });
      expect(state.workspaces.find(w => w.name === 'Source')?.layout).toMatchObject({ type: 'split', ratio: 0.73 });
      expect(state.workspaces.find(w => w.name === 'Destination')?.layout).toMatchObject({ type: 'split', ratio: 0.67 });
      expect(collectPaneIds(state.layout).includes('p')).toBe(!activeSource);
      expect(state.workspaceLayoutDirty).toBe(true);
      expect(state.paneLayoutMode).toBe('free');
      expect(state.focusedPaneId).toBe(activeSource ? 'keep' : 'other');
      expect(api.saveWorkspace).not.toHaveBeenCalled();
      const returnedSource = { ...sourceSaved, paneMoveVersion: 2, layoutVersion: 2, layout: splitLargestPane(movedSource.layout, 'p') };
      const returnedTarget = { ...targetSaved, paneMoveVersion: 2, layoutVersion: 2, layout: removeCustomPane(movedTarget.layout, 'p') };
      vi.mocked(api.listWorkspaces).mockResolvedValue([returnedSource, returnedTarget]);
      vi.mocked(api.listWorkspaceSummaries).mockResolvedValue([returnedSource, returnedTarget]);
      await state.refreshWorkspaces({ full: true, paneMove: { paneId: 'p', source: 'Destination', destination: 'Source' } });
      state = useStore.getState();
      expect(state.layout).toMatchObject({ type: 'split', ratio: activeSource ? 0.73 : 0.67 });
      expect(collectPaneIds(state.layout).includes('p')).toBe(activeSource);
      expect(state.activeWorkspaceMoveVersion).toBe(2);
      expect(state.activeWorkspaceLayoutVersion).toBe(2);
      await vi.advanceTimersByTimeAsync(500);
      expect(api.saveWorkspace).toHaveBeenCalledTimes(1);
      expect(api.saveWorkspace).toHaveBeenCalledWith(expect.objectContaining({ name: activeWorkspace, paneMoveVersion: 2, layoutVersion: 2, layoutMode: 'free', layout: expect.objectContaining({ ratio: activeSource ? 0.73 : 0.67 }) }), expect.anything());
    });
  }

  it('uses persisted geometry only when the target has not been loaded', async () => {
    const result = moved();
    const unloaded = { ...destination, summary: true, layout: null };
    useStore.setState({ workspaces: [source, unloaded] });
    vi.mocked(api.listWorkspaces).mockResolvedValue([result.source, result.destination]);
    await useStore.getState().refreshWorkspaces({ full: true, paneMove: { paneId: 'p', source: 'Source', destination: 'Destination' } });
    expect(useStore.getState().workspaces.find(w => w.name === 'Destination')?.layout).toEqual(result.destination.layout);
  });

  it('still applies the event delta when a summary poll already advanced cached membership', async () => {
    const result = moved();
    useStore.setState({ workspaces: [result.source, result.destination] });
    vi.mocked(api.listWorkspaces).mockResolvedValue([result.source, result.destination]);
    await useStore.getState().refreshWorkspaces({ full: true, paneMove: { paneId: 'p', source: 'Source', destination: 'Destination' } });
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['keep']);
  });
});


it('rebases coalesced moves before autosave even if a summary already advanced the cache', async () => {
  const emptySource = { ...workspace('Source', []), paneMoveVersion: 2, layoutVersion: 2 };
  const fullTarget = { ...workspace('Destination', ['other', 'p']), paneIds: ['other', 'p', 'keep'], panes: ['other', 'p', 'keep'].map(id => ({ ...pane(id), kind: undefined })), paneMoveVersion: 2, layoutVersion: 2 };
  useStore.setState({ workspaces: [{ ...source, paneIds: [], paneMoveVersion: 2 }, { ...destination, paneIds: ['other', 'p', 'keep'], paneMoveVersion: 2 }] });
  vi.mocked(api.listWorkspaces).mockResolvedValue([emptySource, fullTarget]);
  await useStore.getState().refreshWorkspaces({ full: true, paneMove: { paneId: 'p', source: 'Source', destination: 'Destination' } });
  expect(collectPaneIds(useStore.getState().layout)).toEqual([]);
  expect(useStore.getState().activeWorkspaceMoveVersion).toBe(2);
  expect(useStore.getState().activeWorkspaceKnownPaneIds).toEqual([]);
  await vi.advanceTimersByTimeAsync(500);
  expect(api.saveWorkspace).not.toHaveBeenCalled();
});
