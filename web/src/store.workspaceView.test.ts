import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, currentRoutingScope } from './api';
import type { Layout, PaneInfo, Workspace } from './types';
import { effectiveTerminalTarget, effectiveVisiblePaneIds, useStore } from './store';
import { buildNxMFromLeaves, collectLeaves, collectPaneIds } from './lib/layout';
import { readWorkspaceAllPanes, saveWorkspaceAllPanes, workspaceViewKey, canonicalWorkspaceNode, effectiveWorkspaceView, isDerivedWorkspaceLayout, isTemporaryWorkspaceView, workspaceViewLayout } from './lib/workspaceView';

const pane = (id: string, by?: string): PaneInfo => ({ id, title: id, cwd: '/project', cmd: ['sh'], state: 'idle', status: 'running', ...(by ? { origin: { source: 'agent', byPaneId: by, leadPaneId: 'lead', at: 1 } } : {}) } as PaneInfo);
const ids = ['lead', 'w1', 'w2', 'w3', 'sub', 'human'];
const layout = buildNxMFromLeaves(ids.map(paneId => ({ type: 'pane', paneId } as Layout)), 2, 3)!;
const visible = () => collectPaneIds(workspaceViewLayout(useStore.getState()));
const viewKind = () => effectiveWorkspaceView(useStore.getState()).kind;
const initial = useStore.getState();
beforeEach(() => {
  vi.useFakeTimers();
  localStorage.removeItem('solu.workspaceAllPanes.v1');
  useStore.setState({ panes: Object.fromEntries(ids.map(id => [id, pane(id, id === 'sub' ? 'w1' : id.startsWith('w') ? 'lead' : undefined)])), layout, workspaces: [{ name: 'Project', layout, paneIds: ids } as Workspace], activeWorkspace: 'Project', activeBackend: null, activeWorkspaceKnownPaneIds: ids, activeWorkspaceMoveVersion: 0, updateStatus: null, paneLayoutMode: 'free', workspaceLayoutDirty: false, activeWorkspaceLayoutVersion: 0, workspaceView: null, workspaceAllPanes: {}, mainPresentation: { kind: 'workspace' }, focusedPaneId: 'human', zoomedPaneId: null });
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
  it.each(['team', 'single', 'top-level'] as const)('keeps %s grid leaf order and pane identities across shuffled refreshes', async kind => {
    if (kind === 'top-level') useStore.getState().showWorkspaceOverview();
    else useStore.getState().showWorkspacePaneView('lead', kind === 'team');
    const state = useStore.getState();
    const before = visible();
    const shuffled = Object.values(state.panes).reverse();
    expect(collectPaneIds(workspaceViewLayout({ ...state, panes: Object.fromEntries(shuffled.map(p => [p.id, p])) }))).toEqual(before);
    vi.spyOn(api, 'listPanes').mockResolvedValue(shuffled);
    await useStore.getState().refreshPanes();
    expect(useStore.getState().panes).toBe(state.panes);
    for (const p of shuffled) expect(useStore.getState().panes[p.id]).toBe(p);
    expect(visible()).toEqual(before);
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
  it('maximises a worker and returns to the overview', () => {
    useStore.getState().showWorkspacePaneView('w2', false);
    expect(visible()).toEqual(['w2']);
    expect(effectiveTerminalTarget(useStore.getState())).toBe('w2');
    expect(useStore.getState().focusedPaneId).toBe('w2');
    useStore.getState().showWorkspaceOverview();
    expect(visible()).toEqual(['lead', 'human']);
    expect(useStore.getState().workspaceView).toBeNull();
    expect(useStore.getState().focusedPaneId).toBe('lead');
  });
  it('includes a worker missing from the saved layout without adopting it', () => {
    useStore.setState(s => ({ panes: { ...s.panes, lost: pane('lost', 'lead') } }));
    useStore.getState().showWorkspacePaneView('lost', false);
    expect(visible()).toEqual(['lost']);
    expect(effectiveTerminalTarget(useStore.getState())).toBe('lost');
    expect(collectPaneIds(useStore.getState().layout)).not.toContain('lost');
  });
  it('shows human-opened top-level panes and toggles all panes without changing the canonical layout', () => {
    useStore.getState().showWorkspaceOverview();
    expect(visible()).toEqual(['lead', 'human']);
    useStore.getState().toggleWorkspaceAllPanes();
    expect(workspaceViewLayout(useStore.getState())).toBe(layout);
  });
  it('falls back to all panes when every pane was agent-started', () => {
    useStore.setState(s => ({ panes: Object.fromEntries(Object.entries(s.panes).map(([id, p]) =>
      [id, { ...p, origin: p.origin ?? { source: 'agent', byPaneId: 'closed', at: 1 } }])) }));
    useStore.getState().showWorkspaceOverview();
    expect(new Set(visible())).toEqual(new Set(ids));
  });
  it('persists All panes independently per workspace and backend, safely handling storage failure', () => {
    useStore.getState().toggleWorkspaceAllPanes();
    const key = workspaceViewKey('Project', null);
    expect(readWorkspaceAllPanes()[key]).toBe(true);
    useStore.setState({ activeWorkspace: 'Other' });
    expect(viewKind()).toBe('top-level');
    useStore.setState({ activeWorkspace: 'Project', activeBackend: 'win' });
    expect(viewKind()).toBe('top-level');
    useStore.setState({ activeBackend: null });
    expect(viewKind()).toBe('all');
    // The setting can be changed for a workspace that is not open.
    useStore.getState().toggleWorkspaceAllPanes('Other', null);
    expect(readWorkspaceAllPanes()[workspaceViewKey('Other', null)]).toBe(true);
    expect(viewKind()).toBe('all');
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('disabled'); });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('quota'); });
    expect(readWorkspaceAllPanes()).toEqual({});
    expect(() => saveWorkspaceAllPanes({ [key]: true })).not.toThrow();
    expect(() => useStore.getState().toggleWorkspaceAllPanes()).not.toThrow();
    expect(viewKind()).toBe('top-level');
  });
  it('preserves saved splits and non-terminal leaves in an editable overview', () => {
    const browser: Layout = { type: 'browser', browserId: 'browser' };
    const file: Layout = { type: 'file', fileId: 'file' };
    const split: Layout = { type: 'split', direction: 'vertical', ratio: 0.27,
      a: { type: 'pane', paneId: 'lead' }, b: { type: 'split', direction: 'horizontal', ratio: 0.61,
        a: { type: 'pane', paneId: 'w1' }, b: browser } };
    const saved: Layout = { type: 'split', direction: 'horizontal', ratio: 0.38, a: split, b: file };
    useStore.setState({ layout: saved });
    useStore.getState().showWorkspaceOverview();
    const projected = workspaceViewLayout(useStore.getState())!;
    expect(projected).toMatchObject({ type: 'split', ratio: 0.38, a: { type: 'split', ratio: 0.27, b: browser }, b: file });
    expect(canonicalWorkspaceNode(projected)).toBe(saved);
    expect(isTemporaryWorkspaceView(useStore.getState().workspaceView)).toBe(false);
    expect(collectLeaves(projected)).toEqual([{ type: 'pane', paneId: 'lead' }, browser, file]);
    useStore.getState().beginCustomLayout();
    expect(useStore.getState().paneLayoutMode).toBe('free');
    expect(useStore.getState().layout).toBe(saved);
    // Arranging the overview saves what it shows and keeps the hidden worker
    // beside its lead, so the projection is exactly the preset.
    useStore.getState().applyLayoutPreset('rows');
    const arranged = useStore.getState().layout!;
    expect(useStore.getState().workspaceView).toBeNull();
    expect(new Set(collectLeaves(arranged))).toEqual(new Set(collectLeaves(saved)));
    expect(collectLeaves(workspaceViewLayout(useStore.getState()))).toEqual([{ type: 'pane', paneId: 'lead' }, browser, file]);
    expect(isDerivedWorkspaceLayout(workspaceViewLayout(useStore.getState()))).toBe(false);
  });
  it('preserves non-terminal focus and restored zoom when returning from an owner', () => {
    const mixed: Layout = { type: 'split', direction: 'horizontal', ratio: 0.4,
      a: { type: 'pane', paneId: 'lead' }, b: { type: 'browser', browserId: 'browser' } };
    useStore.setState({ layout: mixed, focusedPaneId: 'browser', zoomedPaneId: 'browser',
      mainPresentation: { kind: 'owner', taskId: 'owner', scope: currentRoutingScope(), history: false, loading: false } });
    useStore.getState().returnToWorkspace();
    expect(useStore.getState().focusedPaneId).toBe('browser');
    expect(useStore.getState().zoomedPaneId).toBe('browser');
    useStore.getState().showWorkspaceOverview();
    expect(useStore.getState().focusedPaneId).toBe('browser');
    expect(useStore.getState().zoomedPaneId).toBeNull();
  });
  it('reuses the exact saved layout when no panes need filtering', () => {
    useStore.setState(s => ({ panes: Object.fromEntries(Object.entries(s.panes).map(([id, p]) => [id, { ...p, origin: undefined }])) }));
    useStore.getState().showWorkspaceOverview();
    expect(workspaceViewLayout(useStore.getState())).toBe(layout);
  });
  it('rejects a drop onto an appended tile without removing or autosaving its source', async () => {
    const save = vi.spyOn(api, 'saveWorkspace');
    useStore.setState(s => ({ panes: { ...s.panes, appended: pane('appended', 'lead') } }));
    useStore.getState().toggleWorkspaceAllPanes();
    expect(visible()).toContain('appended');
    for (const zone of ['left', 'right', 'top', 'bottom', 'center'] as const) {
      useStore.getState().movePaneInLayout('human', 'appended', zone);
      expect(useStore.getState().layout).toBe(layout);
      expect(visible()).toContain('human');
    }
    await vi.advanceTimersByTimeAsync(500);
    expect(save).not.toHaveBeenCalled();
  });
  it('balances recovered tiles instead of repeatedly halving the saved layout', () => {
    const extra = Object.fromEntries(Array.from({ length: 8 }, (_, i) => [`extra${i}`, pane(`extra${i}`, 'lead')]));
    useStore.setState(s => ({ panes: { ...s.panes, ...extra } }));
    useStore.getState().toggleWorkspaceAllPanes();
    const areas: number[] = [];
    const visit = (node: Layout, area = 1) => {
      if (node.type !== 'split') { areas.push(area); return; }
      visit(node.a, area * node.ratio); visit(node.b, area * (1 - node.ratio));
    };
    visit(workspaceViewLayout(useStore.getState())!);
    expect(areas).toHaveLength(ids.length + 8);
    expect(Math.max(...areas) / Math.min(...areas)).toBeLessThanOrEqual(2);
    expect(useStore.getState().layout).toBe(layout);
  });
  it.each([false, true])('falls back to the saved overview when a selected pane disappears (All panes %s)', all => {
    if (all) useStore.getState().toggleWorkspaceAllPanes();
    useStore.getState().showWorkspacePaneView('w2', false);
    useStore.setState(s => ({ panes: Object.fromEntries(Object.entries(s.panes).filter(([id]) => id !== 'w2')),
      layout: buildNxMFromLeaves(ids.filter(id => id !== 'w2').map(paneId => ({ type: 'pane', paneId })), 2, 3) }));
    expect(viewKind()).toBe(all ? 'all' : 'top-level');
    expect(useStore.getState().workspaceView).toBeNull();
    expect(visible()).toEqual(all ? ids.filter(id => id !== 'w2') : ['lead', 'human']);
  });
  it('opens the top-level ancestor team for a deliberate focus on a hidden worker', () => {
    useStore.getState().setFocus('sub');
    expect(useStore.getState().workspaceView).toMatchObject({ kind: 'team', paneId: 'lead' });
    expect(useStore.getState().focusedPaneId).toBe('sub');
    expect(visible()).toEqual(['lead', 'w1', 'sub', 'w2', 'w3']);
    expect(effectiveTerminalTarget(useStore.getState())).toBe('sub');
    // A pane the team view already shows keeps the view.
    useStore.getState().setFocus('w2');
    expect(useStore.getState().workspaceView).toMatchObject({ kind: 'team', paneId: 'lead' });
    // A top-level pane outside it returns to the overview.
    useStore.getState().setFocus('human');
    expect(useStore.getState().workspaceView).toBeNull();
    expect(visible()).toEqual(['lead', 'human']);
  });
  it('opens a sub-lead\'s team, not its parent\'s, for its hidden worker', () => {
    // w1 becomes a sub-lead (@social under the lead); sub is its worker.
    useStore.setState(s => ({ panes: { ...s.panes,
      w1: { ...s.panes.w1, origin: { ...s.panes.w1.origin!, teamKind: 'lead', leadRole: 'social' } },
      sub: { ...s.panes.sub, origin: { ...s.panes.sub.origin!, teamKind: 'worker' } } } }));
    expect(visible()).toEqual(['lead', 'human']);
    useStore.getState().setFocus('sub');
    expect(useStore.getState().workspaceView).toMatchObject({ kind: 'team', paneId: 'w1' });
    expect(visible()).toEqual(['w1', 'sub']);
    // A hidden sub-lead opens its own team too.
    useStore.getState().showWorkspaceOverview();
    useStore.getState().setFocus('w1');
    expect(useStore.getState().workspaceView).toMatchObject({ kind: 'team', paneId: 'w1' });
  });
  it('keeps the view and the current pane when focus lands on a hidden pane incidentally', () => {
    useStore.getState().showWorkspacePaneView('w1', true);
    expect(useStore.getState().focusedPaneId).toBe('w1');
    useStore.setState({ focusedPaneId: 'w3' });
    expect(useStore.getState().workspaceView).toMatchObject({ kind: 'team', paneId: 'w1' });
    expect(useStore.getState().focusedPaneId).toBe('w1');
    useStore.getState().showWorkspaceOverview();
    useStore.setState({ focusedPaneId: 'w2' });
    expect(useStore.getState().workspaceView).toBeNull();
    expect(useStore.getState().focusedPaneId).toBe('lead');
  });
  it('reveals a hidden worker into its team for attention, toast and palette jumps', async () => {
    useStore.getState().showWorkspaceOverview();
    expect(visible()).toEqual(['lead', 'human']);
    await useStore.getState().revealPane('sub');
    expect(useStore.getState().workspaceView).toMatchObject({ kind: 'team', paneId: 'lead' });
    expect(useStore.getState().focusedPaneId).toBe('sub');
    expect(effectiveVisiblePaneIds(useStore.getState())).toContain('sub');
    // A recovered worker outside the saved layout is shown without adopting it.
    useStore.setState(s => ({ panes: { ...s.panes, lost: pane('lost', 'w1') } }));
    await useStore.getState().revealPane('lost');
    expect(useStore.getState().focusedPaneId).toBe('lost');
    expect(visible()).toContain('lost');
    expect(collectPaneIds(useStore.getState().layout)).not.toContain('lost');
  });
  it('reveals an unattached orphan into its own subtree view', async () => {
    const orphan = { ...pane('orphan', 'closed'), origin: { source: 'agent', byPaneId: 'closed', workspace: 'Project', at: 1 } } as PaneInfo;
    useStore.setState(s => ({ panes: { ...s.panes, orphan, kid: pane('kid', 'orphan') } }));
    await useStore.getState().revealPane('kid');
    expect(useStore.getState().workspaceView).toMatchObject({ kind: 'team', paneId: 'orphan' });
    expect(visible()).toEqual(['orphan', 'kid']);
    expect(useStore.getState().focusedPaneId).toBe('kid');
  });
  it('opens a queued pane view in the same update that switches workspace', () => {
    const otherLayout = buildNxMFromLeaves(['o-lead', 'o-worker'].map(paneId => ({ type: 'pane', paneId } as Layout)), 1, 2)!;
    const other = { name: 'Other', layout: otherLayout, paneIds: ['o-lead', 'o-worker'] } as Workspace;
    useStore.setState(s => ({ workspaces: [...s.workspaces, other], panes: { ...s.panes, 'o-lead': pane('o-lead'), 'o-worker': { ...pane('o-worker', 'o-lead'), origin: { source: 'agent', byPaneId: 'o-lead', leadPaneId: 'o-lead', at: 1 } } } }));
    const seen: string[][] = [];
    const unsubscribe = useStore.subscribe(state => { if (state.activeWorkspace === 'Other') seen.push(collectPaneIds(workspaceViewLayout(state))); });
    useStore.getState().queueWorkspaceReveal({ workspace: 'Other', backend: null, paneId: 'o-worker' });
    useStore.setState({ activeWorkspace: 'Other', layout: otherLayout, focusedPaneId: 'o-lead' });
    unsubscribe();
    expect(useStore.getState().workspaceView).toMatchObject({ kind: 'team', paneId: 'o-lead' });
    expect(useStore.getState().focusedPaneId).toBe('o-worker');
    // The final view is what renders; an incidental first focus does not
    // switch views on its own.
    expect(seen.at(-1)).toEqual(['o-lead', 'o-worker']);
  });
  it('lands on the overview with a visible focus when a workspace switch focuses a hidden worker', () => {
    useStore.getState().showWorkspacePaneView('w1', true);
    const otherLayout = buildNxMFromLeaves(['o-worker', 'o-lead'].map(paneId => ({ type: 'pane', paneId } as Layout)), 1, 2)!;
    useStore.setState(s => ({ workspaces: [...s.workspaces, { name: 'Other', layout: otherLayout, paneIds: ['o-worker', 'o-lead'] } as Workspace],
      panes: { ...s.panes, 'o-lead': pane('o-lead'), 'o-worker': { ...pane('o-worker'), origin: { source: 'agent', byPaneId: 'o-lead', leadPaneId: 'o-lead', at: 1 } } } }));
    const seen: string[][] = [];
    const unsubscribe = useStore.subscribe(state => { seen.push(collectPaneIds(workspaceViewLayout(state))); });
    useStore.setState({ activeWorkspace: 'Other', layout: otherLayout, focusedPaneId: 'o-worker' });
    unsubscribe();
    // Never the raw saved layout with its worker, not even for one update.
    expect(seen.every(shown => !shown.includes('o-worker'))).toBe(true);
    expect(useStore.getState().workspaceView).toBeNull();
    expect(useStore.getState().focusedPaneId).toBe('o-lead');
  });
  it('keeps the shown arrangement stable across unrelated pane updates', () => {
    useStore.setState({ paneLayoutMode: 'auto' });
    const before = workspaceViewLayout(useStore.getState());
    useStore.setState(s => ({ panes: { ...s.panes, w1: { ...s.panes.w1, lastActivityMs: 99 } } }));
    expect(workspaceViewLayout(useStore.getState())).toBe(before);
  });
  it('does not mount a named workspace orphan in the synthetic current overview', () => {
    const named = { name: 'Alpha', layout: null, paneIds: [] } as unknown as Workspace;
    const s = useStore.getState();
    expect(workspaceViewLayout({ ...s, workspaces: [named], panes: { orphan: pane('orphan', 'closed') },
      layout: null, activeWorkspace: '_current', workspaceView: { workspace: '_current', backend: null, kind: 'all' } })).toBeNull();
  });
  it('auto-tiles the visible overview evenly and keeps hidden workers in the saved layout', async () => {
    vi.spyOn(api, 'saveWorkspace').mockImplementation(async w => w);
    useStore.setState({ paneLayoutMode: 'auto', layoutColumns: 4, layoutAspect: 1.6 });
    expect(workspaceViewLayout(useStore.getState())).toMatchObject({ type: 'split', ratio: 0.5 });
    useStore.getState().updateLayoutViewport(640, 900);
    expect(visible()).toEqual(['lead', 'human']);
    expect(workspaceViewLayout(useStore.getState())).toMatchObject({ type: 'split', ratio: 0.5 });
    useStore.getState().retilePanes();
    expect(new Set(collectPaneIds(useStore.getState().layout))).toEqual(new Set(ids));
    expect(useStore.getState().paneLayoutMode).toBe('auto');
    expect(visible()).toEqual(['lead', 'human']);
  });
  it('saves the shown auto overview when a splitter is first dragged, without moving a tile', async () => {
    const save = vi.spyOn(api, 'saveWorkspace').mockImplementation(async w => w);
    useStore.setState({ paneLayoutMode: 'auto', layoutColumns: 4, layoutAspect: 1.6 });
    const shown = workspaceViewLayout(useStore.getState())!;
    expect(isDerivedWorkspaceLayout(shown)).toBe(true);
    expect(canonicalWorkspaceNode(shown)).toBeNull();
    useStore.getState().beginCustomLayout();
    const state = useStore.getState();
    expect(state.paneLayoutMode).toBe('free');
    expect(new Set(collectPaneIds(state.layout))).toEqual(new Set(ids));
    const after = workspaceViewLayout(state)!;
    expect(JSON.parse(JSON.stringify(after))).toEqual(JSON.parse(JSON.stringify(shown)));
    expect(isDerivedWorkspaceLayout(after)).toBe(false);
    // The resize now maps to a saved split.
    expect(canonicalWorkspaceNode(after)).not.toBeNull();
    // Hidden workers sit beside their lead in All panes.
    useStore.getState().toggleWorkspaceAllPanes();
    const all = workspaceViewLayout(useStore.getState())!;
    expect(all).toBe(useStore.getState().layout);
    expect(all).toMatchObject({ a: { type: 'split', a: { paneId: 'lead' } }, b: { paneId: 'human' } });
    await vi.advanceTimersByTimeAsync(500);
    expect(save).toHaveBeenCalledWith(expect.objectContaining({ layoutMode: 'free', paneIds: expect.arrayContaining(ids) }), expect.anything());
  });
  it('commits an auto overview before moving a tile so nothing jumps', () => {
    useStore.setState({ paneLayoutMode: 'auto', layoutColumns: 4, layoutAspect: 1.6 });
    useStore.getState().movePaneInLayout('human', 'lead', 'left');
    expect(visible()).toEqual(['human', 'lead']);
    expect(workspaceViewLayout(useStore.getState())).toMatchObject({ type: 'split', ratio: 0.5 });
    expect(new Set(collectPaneIds(useStore.getState().layout))).toEqual(new Set(ids));
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
  it('fills a larger overview grid with new shells and keeps hidden workers saved', async () => {
    let n = 0;
    const create = vi.spyOn(api, 'createPane').mockImplementation(async () => pane(`new${++n}`));
    vi.spyOn(api, 'paneLiveCwd').mockResolvedValue('/project');
    vi.spyOn(api, 'saveWorkspace').mockImplementation(async w => w);
    expect(visible()).toEqual(['lead', 'human']);
    await useStore.getState().applyGridLayout(2, 2);
    expect(create).toHaveBeenCalledTimes(2);
    expect(visible()).toEqual(['lead', 'human', 'new1', 'new2']);
    expect(workspaceViewLayout(useStore.getState())).toMatchObject({ type: 'split', ratio: 0.5, a: { type: 'split', ratio: 0.5 }, b: { type: 'split', ratio: 0.5 } });
    expect(new Set(collectPaneIds(useStore.getState().layout))).toEqual(new Set([...ids, 'new1', 'new2']));
    expect(useStore.getState().workspaceView).toBeNull();
    expect(useStore.getState().focusedPaneId).toBe('new1');
  });
  it('includes newly spawned workers after a temporary grid reflow', async () => {
    useStore.getState().showWorkspacePaneView('lead', true);
    await useStore.getState().applyGridLayout(2, 3);
    useStore.setState(s => ({ panes: { ...s.panes, extra1: pane('extra1', 'lead'), extra2: pane('extra2', 'lead') } }));
    expect(visible()).toHaveLength(7);
    expect(visible()).toContain('extra2');
  });
  it('keeps top-level panes visible when their last live workers finish', () => {
    useStore.getState().showWorkspaceOverview();
    useStore.setState(s => ({ panes: Object.fromEntries(Object.entries(s.panes).map(([id, p]) => [id, { ...p, status: 'exited' }])) }));
    expect(visible()).toEqual(['lead', 'human']);
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
    expect(viewKind()).toBe('top-level');
    expect(useStore.getState().workspaceView).toBeNull();
    useStore.getState().showWorkspaceOverview();
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
    useStore.getState().toggleWorkspaceAllPanes();
    expect(visible()).toContain('human');
    expect(visible()).toContain('late');
  });

  it.each(['team', 'single'] as const)('ignores cross-pane moves in a %s view without autosaving', async kind => {
    const save = vi.spyOn(api, 'saveWorkspace');
    useStore.getState().showWorkspacePaneView('lead', kind === 'team');
    const view = useStore.getState().workspaceView;
    const beforeVisible = visible();
    useStore.getState().movePaneInLayout('w2', 'w1', 'left');
    await vi.advanceTimersByTimeAsync(500);
    expect(useStore.getState().layout).toBe(layout);
    expect(useStore.getState().workspaceView).toBe(view);
    expect(visible()).toEqual(beforeVisible);
    expect(save).not.toHaveBeenCalled();
    useStore.getState().showWorkspaceOverview();
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
    useStore.getState().toggleWorkspaceAllPanes();
    expect(workspaceViewLayout(useStore.getState())).toBe(layout);
  });

  it.each(['team', 'single', 'top-level'] as const)('reveals a newly split pane while leaving a %s view', async kind => {
    vi.spyOn(api, 'createPane').mockResolvedValue(pane('new'));
    vi.spyOn(api, 'saveWorkspace').mockImplementation(async w => w);
    vi.spyOn(api, 'listWorkspaceSummaries').mockResolvedValue([]);
    if (kind === 'top-level') useStore.getState().showWorkspaceOverview();
    else useStore.getState().showWorkspacePaneView('lead', kind === 'team');
    await useStore.getState().splitFocused('horizontal', { cwd: '/project', cmd: ['sh'] });
    expect(useStore.getState().workspaceView).toBeNull();
    expect(viewKind()).toBe('top-level');
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
