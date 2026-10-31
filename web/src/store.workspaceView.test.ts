import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from './api';
import type { Layout, PaneInfo, Workspace } from './types';
import { effectiveTerminalTarget, effectiveVisiblePaneIds, useStore } from './store';
import { buildNxMFromLeaves, collectLeaves, collectPaneIds } from './lib/layout';
import { workspaceViewLayout } from './lib/workspaceView';

const pane = (id: string, by?: string): PaneInfo => ({ id, title: id, cwd: '/project', cmd: ['sh'], state: 'idle', status: 'running', ...(by ? { origin: { source: 'agent', byPaneId: by, leadPaneId: 'lead', at: 1 } } : {}) } as PaneInfo);
const ids = ['lead', 'w1', 'w2', 'w3', 'sub', 'human'];
const layout = buildNxMFromLeaves(ids.map(paneId => ({ type: 'pane', paneId } as Layout)), 2, 3)!;
const visible = () => collectPaneIds(workspaceViewLayout(useStore.getState()));
const initial = useStore.getState();
beforeEach(() => {
  vi.useFakeTimers();
  useStore.setState({ panes: Object.fromEntries(ids.map(id => [id, pane(id, id === 'sub' ? 'w1' : id.startsWith('w') ? 'lead' : undefined)])), layout, workspaces: [{ name: 'Project', layout, paneIds: ids } as Workspace], activeWorkspace: 'Project', activeBackend: null, activeWorkspaceKnownPaneIds: ids, activeWorkspaceMoveVersion: 0, updateStatus: null, paneLayoutMode: 'free', workspaceLayoutDirty: false, activeWorkspaceLayoutVersion: 0, workspaceView: null, mainPresentation: { kind: 'workspace' }, focusedPaneId: 'human', zoomedPaneId: null });
});
afterEach(() => { vi.clearAllTimers(); vi.useRealTimers(); vi.restoreAllMocks(); useStore.setState(initial); });

describe('temporary workspace views', () => {
  it('shows a lead and its descendants without saving or changing the canonical layout', async () => {
    const save = vi.spyOn(api, 'saveWorkspace').mockResolvedValue({ name: 'Project', layout, paneIds: ids } as Workspace);
    useStore.getState().showWorkspacePaneView('lead', true);
    expect(visible()).toEqual(['lead', 'w1', 'sub', 'w2', 'w3']);
    expect(effectiveVisiblePaneIds(useStore.getState())).toEqual(visible());
    expect(useStore.getState().layout).toBe(layout);
    await vi.advanceTimersByTimeAsync(1000);
    expect(save).not.toHaveBeenCalled();
  });
  it('autosaves the complete canonical layout when a pane is edited during a single view', async () => {
    const savedWorkspace = { name: 'Project', layout, paneIds: ids } as Workspace;
    const save = vi.spyOn(api, 'saveWorkspace').mockImplementation(async w => w);
    vi.spyOn(api, 'listWorkspaces').mockResolvedValue([savedWorkspace]);
    vi.spyOn(api, 'renamePane').mockResolvedValue({ ...pane('w2', 'lead'), title: 'Reviewed worker' });
    useStore.getState().showWorkspacePaneView('w2', false);
    await useStore.getState().renamePane('w2', 'Reviewed worker');
    await vi.advanceTimersByTimeAsync(500);
    expect(save).toHaveBeenCalledWith(expect.objectContaining({
      layout,
      paneIds: ids,
      knownPaneIds: ids,
      panes: expect.arrayContaining([expect.objectContaining({ id: 'w2', title: 'Reviewed worker' })]),
    }), expect.anything());
    expect(visible()).toEqual(['w2']);
  });
  it('shows a nested lead and only its own team', () => {
    useStore.getState().showWorkspacePaneView('w1', true);
    expect(visible()).toEqual(['w1', 'sub']);
  });
  it('maximises a worker and returns to the exact full layout', () => {
    useStore.getState().showWorkspacePaneView('w2', false);
    expect(visible()).toEqual(['w2']);
    expect(effectiveTerminalTarget(useStore.getState())).toBe('w2');
    expect(useStore.getState().focusedPaneId).toBe('w2');
    useStore.getState().returnWorkspaceLayout();
    expect(workspaceViewLayout(useStore.getState())).toBe(layout);
    expect(useStore.getState().workspaceView).toBeNull();
  });
  it('includes a worker missing from the saved layout without adopting it', () => {
    useStore.setState(s => ({ panes: { ...s.panes, lost: pane('lost', 'lead') } }));
    useStore.getState().showWorkspacePaneView('lost', false);
    expect(visible()).toEqual(['lost']);
    expect(effectiveTerminalTarget(useStore.getState())).toBe('lost');
    expect(collectPaneIds(useStore.getState().layout)).not.toContain('lost');
  });
  it('shows only live leads and toggles back', () => {
    useStore.getState().toggleWorkspaceLeadsOnly();
    expect(visible()).toEqual(['lead', 'w1']);
    useStore.getState().toggleWorkspaceLeadsOnly();
    expect(workspaceViewLayout(useStore.getState())).toBe(layout);
  });
  it('reflows team views with the existing presets and grid without saving or spawning', async () => {
    const create = vi.spyOn(api, 'createPane');
    useStore.getState().showWorkspacePaneView('lead', true);
    useStore.getState().applyLayoutPreset('rows');
    expect(visible()).toEqual(['lead', 'w1', 'sub', 'w2', 'w3']);
    await useStore.getState().applyGridLayout(2, 3);
    expect(visible()).toHaveLength(5);
    expect(create).not.toHaveBeenCalled();
    expect(useStore.getState().layout).toBe(layout);
  });
  it('includes newly spawned workers after a temporary grid reflow', async () => {
    useStore.getState().showWorkspacePaneView('lead', true);
    await useStore.getState().applyGridLayout(2, 3);
    useStore.setState(s => ({ panes: { ...s.panes, extra1: pane('extra1', 'lead'), extra2: pane('extra2', 'lead') } }));
    expect(visible()).toHaveLength(7);
    expect(visible()).toContain('extra2');
  });
  it('shows an empty leads view if the last live workers finish', () => {
    useStore.getState().toggleWorkspaceLeadsOnly();
    useStore.setState(s => ({ panes: Object.fromEntries(Object.entries(s.panes).map(([id, p]) => [id, { ...p, status: 'exited' }])) }));
    expect(workspaceViewLayout(useStore.getState())).toBeNull();
  });
  it('clears the filter on workspace, backend or owner navigation', () => {
    for (const change of [{ activeWorkspace: 'Other' }, { activeBackend: 'win' as const }, { mainPresentation: { kind: 'owner' as const, ownerId: 'owner', history: false } }]) {
      useStore.setState({ activeWorkspace: 'Project', activeBackend: null, activeWorkspaceKnownPaneIds: ids, activeWorkspaceMoveVersion: 0, updateStatus: null, mainPresentation: { kind: 'workspace' } });
      useStore.getState().showWorkspacePaneView('lead', true);
      useStore.setState(change as Partial<typeof initial>);
      expect(useStore.getState().workspaceView).toBeNull();
    }
  });
  it('keeps background layout changes and falls back when the selected pane closes', () => {
    useStore.getState().showWorkspacePaneView('w2', false);
    useStore.setState(s => ({ panes: Object.fromEntries(Object.entries(s.panes).filter(([id]) => id !== 'w2')), layout: { type: 'pane', paneId: 'human' } }));
    expect(visible()).toEqual(['human']);
    expect(useStore.getState().workspaceView).toBeNull();
    useStore.getState().returnWorkspaceLayout();
    expect(visible()).toEqual(['human']);
  });
  it('applies server tombstones to a selected worker and rejects late stale DTOs', async () => {
    useStore.getState().showWorkspacePaneView('w2', false);
    const closedLayout = { type: 'pane', paneId: 'lead' } as Layout;
    const closed = { name: 'Project', layout: closedLayout, paneIds: ['lead'], removedPaneIds: ['w2'] } as Workspace;
    vi.spyOn(api, 'listWorkspaces').mockResolvedValue([closed]);
    await useStore.getState().refreshWorkspaces({ full: true });
    expect(useStore.getState().panes.w2).toBeUndefined();
    expect(useStore.getState().workspaceView).toBeNull();
    // A late pane poll can still mention the worker. Origin recovery must
    // respect the workspace tombstone before the next membership refresh.
    useStore.setState(s => ({ panes: { ...s.panes, w2: pane('w2', 'lead') } }));
    useStore.getState().showWorkspacePaneView('lead', true);
    expect(visible()).not.toContain('w2');
  });

  it('reconciles new daemon members during a team view and autosaves the full workspace', async () => {
    const late = pane('late', 'lead');
    const updated = { name: 'Project', activeAt: '', layout, paneIds: [...ids, 'late'], panes: [...Object.values(useStore.getState().panes), late] } as Workspace;
    vi.spyOn(api, 'listWorkspaces').mockResolvedValue([updated]);
    const save = vi.spyOn(api, 'saveWorkspace').mockImplementation(async w => w);
    useStore.getState().showWorkspacePaneView('lead', true);
    await useStore.getState().refreshWorkspaces({ full: true });
    expect(visible()).toContain('late');
    expect(visible()).not.toContain('human');
    expect(useStore.getState().activeWorkspaceKnownPaneIds).toContain('late');
    useStore.getState().setLayout(useStore.getState().layout);
    await vi.advanceTimersByTimeAsync(500);
    expect(save).toHaveBeenCalledWith(expect.objectContaining({
      paneIds: expect.arrayContaining([...ids, 'late']),
      knownPaneIds: expect.arrayContaining([...ids, 'late']),
    }), expect.anything());
    useStore.getState().returnWorkspaceLayout();
    expect(visible()).toContain('human');
    expect(visible()).toContain('late');
  });

  it.each(['team', 'single', 'leads'] as const)('ignores cross-pane moves in a %s view without autosaving', async kind => {
    const save = vi.spyOn(api, 'saveWorkspace');
    if (kind === 'leads') useStore.getState().toggleWorkspaceLeadsOnly();
    else useStore.getState().showWorkspacePaneView('lead', kind === 'team');
    const view = useStore.getState().workspaceView;
    const beforeVisible = visible();
    useStore.getState().movePaneInLayout('w2', 'w1', 'left');
    await vi.advanceTimersByTimeAsync(500);
    expect(useStore.getState().layout).toBe(layout);
    expect(useStore.getState().workspaceView).toBe(view);
    expect(visible()).toEqual(beforeVisible);
    expect(save).not.toHaveBeenCalled();
    useStore.getState().returnWorkspaceLayout();
    useStore.getState().movePaneInLayout('w2', 'w1', 'left');
    expect(useStore.getState().layout).not.toBe(layout);
    vi.clearAllTimers();
  });

  it('keeps auto-layout viewport, resize and re-tile actions local to a temporary view', async () => {
    const save = vi.spyOn(api, 'saveWorkspace');
    useStore.setState({ paneLayoutMode: 'auto', layoutColumns: 3, layoutAspect: 1.6 });
    useStore.getState().showWorkspacePaneView('lead', true);
    useStore.getState().beginCustomLayout();
    useStore.getState().updateLayoutViewport(400, 900);
    useStore.getState().retilePanes();
    await vi.advanceTimersByTimeAsync(500);
    expect(useStore.getState().layout).toBe(layout);
    expect(useStore.getState().paneLayoutMode).toBe('auto');
    expect(useStore.getState().layoutColumns).toBe(3);
    expect(useStore.getState().layoutAspect).toBe(1.6);
    expect(useStore.getState().workspaceLayoutDirty).toBe(false);
    expect(useStore.getState().workspaceView?.preset).toBe('grid');
    expect(save).not.toHaveBeenCalled();
    useStore.getState().returnWorkspaceLayout();
    expect(workspaceViewLayout(useStore.getState())).toBe(layout);
  });

  it.each(['team', 'single', 'leads'] as const)('reveals a newly split pane while leaving a %s view', async kind => {
    vi.spyOn(api, 'createPane').mockResolvedValue(pane('new'));
    vi.spyOn(api, 'saveWorkspace').mockImplementation(async w => w);
    vi.spyOn(api, 'listWorkspaceSummaries').mockResolvedValue([]);
    if (kind === 'leads') useStore.getState().toggleWorkspaceLeadsOnly();
    else useStore.getState().showWorkspacePaneView('lead', kind === 'team');
    await useStore.getState().splitFocused('horizontal', { cwd: '/project', cmd: ['sh'] });
    expect(useStore.getState().workspaceView).toBeNull();
    expect(useStore.getState().focusedPaneId).toBe('new');
    expect(effectiveTerminalTarget(useStore.getState())).toBe('new');
    expect(visible()).toContain('human');
    expect(visible()).toContain('new');
  });
  it('reveals a newly opened diff without replacing its focus with a terminal', () => {
    useStore.getState().showWorkspacePaneView('w2', false);
    useStore.getState().openDiffHere('/project/file.txt');
    const state = useStore.getState();
    expect(state.workspaceView).toBeNull();
    expect(collectLeaves(state.layout).find(l => l.type === 'diff')).toMatchObject({ diffId: state.focusedPaneId });
  });

});
