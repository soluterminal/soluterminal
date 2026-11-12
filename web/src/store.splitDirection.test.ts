import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, setApiPrincipalAccess, setBackend } from './api';
import { setCloudDualBackend } from './dualBackend';
import { useStore } from './store';
import { balancedLayout } from './lib/autoLayout';
import { collectPaneIds } from './lib/layout';
import { workspaceViewLayout } from './lib/workspaceView';
import type { Layout, PaneInfo, Workspace } from './types';

// "Split right" and "Split down" name a place: the new pane opens on that side
// of the pane they were used on, whatever the workspace's layout mode. Clone
// pane does the same, to the right. Every other way of opening a pane keeps
// following the workspace's placement policy.

const pane = (id: string): PaneInfo => ({ id, title: id, pid: 1, status: 'running', cwd: '/repo', cmd: ['bash'], cols: 80, rows: 24, createdAt: '', state: 'idle', hasClaude: false, lastActivityMs: 0 });
const leaf = (paneId: string): Layout => ({ type: 'pane', paneId });
const split = (direction: 'horizontal' | 'vertical', ratio: number, a: Layout, b: Layout): Layout => ({ type: 'split', direction, ratio, a, b });
const row = (...ids: string[]): Layout => ids.map(leaf).reduce((a, b) => split('horizontal', .5, a, b));

type Rect = { x: number; y: number; w: number; h: number };
// Where each tile sits in a unit square: the geometry the user sees.
function rects(layout: Layout | null, box: Rect = { x: 0, y: 0, w: 1, h: 1 }, out: Record<string, Rect> = {}): Record<string, Rect> {
  if (!layout) return out;
  if (layout.type !== 'split') { if (layout.type === 'pane') out[layout.paneId] = box; return out; }
  if (layout.direction === 'horizontal') {
    rects(layout.a, { ...box, w: box.w * layout.ratio }, out);
    rects(layout.b, { ...box, x: box.x + box.w * layout.ratio, w: box.w * (1 - layout.ratio) }, out);
  } else {
    rects(layout.a, { ...box, h: box.h * layout.ratio }, out);
    rects(layout.b, { ...box, y: box.y + box.h * layout.ratio, h: box.h * (1 - layout.ratio) }, out);
  }
  return out;
}
function expectRightOf(layout: Layout | null, added: string, target: string) {
  const r = rects(layout);
  expect(r[added].x).toBeCloseTo(r[target].x + r[target].w);
  expect(r[added].y).toBeCloseTo(r[target].y);
  expect(r[added].h).toBeCloseTo(r[target].h);
}
function expectBelow(layout: Layout | null, added: string, target: string) {
  const r = rects(layout);
  expect(r[added].y).toBeCloseTo(r[target].y + r[target].h);
  expect(r[added].x).toBeCloseTo(r[target].x);
  expect(r[added].w).toBeCloseTo(r[target].w);
}

let serial = 0, workspace: Workspace, revision: number;
function open(layout: Layout, mode: 'auto' | 'free', focused: string, config: { columns?: number; aspect?: number } = {}) {
  const ids = collectPaneIds(layout);
  const panes = Object.fromEntries(ids.map(id => [id, pane(id)]));
  const columns = config.columns ?? 4, aspect = config.aspect ?? 1.6;
  if (mode === 'auto') layout = balancedLayout(layout, columns, aspect)!;
  workspace = { ...workspace, layoutMode: mode, layoutColumns: columns, layoutAspect: aspect, layout, paneIds: ids, panes: Object.values(panes).map(p => ({ ...p, kind: undefined })) };
  useStore.setState({ layout, paneLayoutMode: mode, layoutColumns: columns, layoutAspect: aspect, panes, workspaces: [workspace],
    activeWorkspaceKnownPaneIds: ids, focusedPaneId: focused, lastUserFocusId: null });
}
const splitFocused = (direction: 'horizontal' | 'vertical') => useStore.getState().splitFocused(direction, { cwd: '/repo', cmd: ['bash'] });

beforeEach(() => {
  vi.useFakeTimers(); localStorage.clear(); setCloudDualBackend(false); setBackend(null); setApiPrincipalAccess(true);
  revision = 0;
  workspace = { name: `Split direction ${++serial}`, paneIds: [], layout: null, activeAt: '', layoutMode: 'auto', layoutColumns: 4, layoutAspect: 1.6, layoutVersion: 0 };
  useStore.setState({ activeWorkspace: workspace.name, activeWorkspaceMoveVersion: 0, activeWorkspaceLayoutVersion: 0,
    workspaceLayoutDirty: false, activeBackend: null, mainPresentation: { kind: 'workspace' }, managedOwnerBindings: new Map(),
    navigationGeneration: 0, workspaceView: null, workspaceAllPanes: {}, zoomedPaneId: null, browsers: {}, files: {}, parkedPaneIds: [],
    updateStatus: null, toasts: [], wsOrder: [] });
  vi.spyOn(api, 'saveWorkspace').mockImplementation(async w => ({ ...w, layoutVersion: ++revision }));
  vi.spyOn(api, 'listWorkspaceSummaries').mockImplementation(async () => useStore.getState().workspaces);
  vi.spyOn(api, 'listWorkspaces').mockImplementation(async () => useStore.getState().workspaces);
  vi.spyOn(api, 'listPanes').mockImplementation(async () => Object.values(useStore.getState().panes));
  vi.spyOn(api, 'paneLiveCwd').mockResolvedValue('/repo');
  vi.spyOn(api, 'createPane').mockImplementation(async () => pane('new'));
});
afterEach(async () => { await vi.advanceTimersByTimeAsync(500); vi.useRealTimers(); vi.restoreAllMocks(); });

describe('Split right and Split down open the pane on the named side', () => {
  it('Split right on the only pane of a narrow auto workspace opens beside it, not below', async () => {
    // One column fits: the balanced grid would stack the second pane underneath.
    open(leaf('p1'), 'auto', 'p1', { columns: 1, aspect: .6 });
    await splitFocused('horizontal');
    expect(useStore.getState().layout).toEqual(split('horizontal', .5, leaf('p1'), leaf('new')));
    expectRightOf(useStore.getState().layout, 'new', 'p1');
  });

  it('Split right in a full auto row opens directly right of the focused pane and shares the row equally', async () => {
    // A fourth pane in the balanced grid would start a second row at the end.
    open(row('p1', 'p2', 'p3'), 'auto', 'p1');
    await splitFocused('horizontal');
    const layout = useStore.getState().layout;
    expect(collectPaneIds(layout)).toEqual(['p1', 'new', 'p2', 'p3']);
    expectRightOf(layout, 'new', 'p1');
    for (const r of Object.values(rects(layout))) { expect(r.w).toBeCloseTo(.25); expect(r.h).toBeCloseTo(1); }
  });

  it('Split down in an auto row opens directly below the focused pane and leaves the other columns alone', async () => {
    open(row('p1', 'p2', 'p3'), 'auto', 'p2');
    const before = rects(useStore.getState().layout);
    await splitFocused('vertical');
    const layout = useStore.getState().layout;
    expectBelow(layout, 'new', 'p2');
    const after = rects(layout);
    expect(after.p2).toEqual({ ...before.p2, h: .5 });
    expect(after.p1).toEqual(before.p1);
    expect(after.p3).toEqual(before.p3);
  });

  it('selects Custom layout and saves it, so the next balance does not move the pane away', async () => {
    open(row('p1', 'p2', 'p3'), 'auto', 'p1');
    await splitFocused('horizontal');
    expect(useStore.getState().paneLayoutMode).toBe('free');
    const saved = vi.mocked(api.saveWorkspace).mock.calls.at(-1)![0];
    expect(saved.layoutMode).toBe('free');
    expect(saved.layout).toEqual(useStore.getState().layout);
    expect(useStore.getState().focusedPaneId).toBe('new');
  });

  it('splits the focused pane of a nested custom layout, not the largest tile', async () => {
    // p1 is the largest tile and wider than tall; p3 is a small tile bottom left.
    open(split('horizontal', .3, split('vertical', .7, leaf('p2'), leaf('p3')), leaf('p1')), 'free', 'p3');
    const before = rects(useStore.getState().layout);
    await splitFocused('horizontal');
    const layout = useStore.getState().layout;
    expect(layout).toEqual(split('horizontal', .3, split('vertical', .7, leaf('p2'), split('horizontal', .5, leaf('p3'), leaf('new'))), leaf('p1')));
    expectRightOf(layout, 'new', 'p3');
    expect(rects(layout).p1).toEqual(before.p1);
    expect(rects(layout).p2).toEqual(before.p2);
  });

  it('Split down inside a nested custom column opens below the focused pane and shares that column equally', async () => {
    open(split('horizontal', .6, leaf('p1'), split('vertical', .5, leaf('p2'), leaf('p3'))), 'free', 'p2');
    await splitFocused('vertical');
    const layout = useStore.getState().layout, r = rects(layout);
    expect(collectPaneIds(layout)).toEqual(['p1', 'p2', 'new', 'p3']);
    expectBelow(layout, 'new', 'p2');
    for (const id of ['p2', 'new', 'p3']) { expect(r[id].h).toBeCloseTo(1 / 3); expect(r[id].w).toBeCloseTo(.4); }
    expect(r.p1).toEqual({ x: 0, y: 0, w: .6, h: 1 });
  });

  it('rebalances the run of same-direction splits the new pane joins, and only that run', async () => {
    // Top row: p1 50%, p2 25%, p3 25%. Bottom row: p4 80%, p5 20%.
    open(split('vertical', .7, split('horizontal', .5, leaf('p1'), row('p2', 'p3')), split('horizontal', .8, leaf('p4'), leaf('p5'))), 'free', 'p2');
    await splitFocused('horizontal');
    const layout = useStore.getState().layout, r = rects(layout);
    expect(collectPaneIds(layout)).toEqual(['p1', 'p2', 'new', 'p3', 'p4', 'p5']);
    expectRightOf(layout, 'new', 'p2');
    for (const id of ['p1', 'p2', 'new', 'p3']) { expect(r[id].w).toBeCloseTo(.25); expect(r[id].h).toBeCloseTo(.7); }
    expect(r.p4.w).toBeCloseTo(.8);
    expect(r.p5.w).toBeCloseTo(.2);
  });

  it('"Split right with shell here" opens right of the pane whose menu was used, even when another pane has focus', async () => {
    open(row('p1', 'p2', 'p3'), 'auto', 'p3');
    await useStore.getState().launchShellHere('p1', 'horizontal');
    const layout = useStore.getState().layout;
    expect(collectPaneIds(layout)).toEqual(['p1', 'new', 'p2', 'p3']);
    expectRightOf(layout, 'new', 'p1');
    expect(useStore.getState().paneLayoutMode).toBe('free');
  });

  it('Clone pane opens the copy directly right of the focused pane, as its help says', async () => {
    open(row('p1', 'p2', 'p3'), 'auto', 'p2');
    await useStore.getState().cloneFocused('horizontal');
    const layout = useStore.getState().layout;
    expect(collectPaneIds(layout)).toEqual(['p1', 'p2', 'new', 'p3']);
    expectRightOf(layout, 'new', 'p2');
    for (const r of Object.values(rects(layout))) expect(r.w).toBeCloseTo(.25);
    expect(useStore.getState().paneLayoutMode).toBe('free');
    expect(api.createPane).toHaveBeenCalledWith(expect.objectContaining({ cwd: '/repo', cmd: ['bash'], title: 'p2' }));
  });

  it('splits the tile on screen when the overview hides worker panes', async () => {
    open(row('lead', 'w1', 'p2'), 'auto', 'lead');
    const worker: PaneInfo = { ...pane('w1'), origin: { source: 'agent', byPaneId: 'lead', leadPaneId: 'lead', at: 1 } } as PaneInfo;
    useStore.setState(s => ({ panes: { ...s.panes, w1: worker } }));
    expect(collectPaneIds(workspaceViewLayout(useStore.getState()))).toEqual(['lead', 'p2']);
    await splitFocused('horizontal');
    const shown = workspaceViewLayout(useStore.getState());
    expect(collectPaneIds(shown)).toEqual(['lead', 'new', 'p2']);
    expectRightOf(shown, 'new', 'lead');
    expect(collectPaneIds(useStore.getState().layout)).toContain('w1');
    expect(useStore.getState().paneLayoutMode).toBe('free');
  });

  it('saves the side split to the workspace it was made in when the user has already switched away', async () => {
    open(row('p1', 'p2', 'p3'), 'auto', 'p1');
    const origin = workspace.name;
    vi.mocked(api.createPane).mockImplementation(async () => { useStore.setState({ activeWorkspace: 'Elsewhere', layout: null }); return pane('new'); });
    await splitFocused('horizontal');
    const saved = vi.mocked(api.saveWorkspace).mock.calls.find(([w]) => w.name === origin)![0];
    expect(saved.layoutMode).toBe('free');
    expect(collectPaneIds(saved.layout)).toEqual(['p1', 'new', 'p2', 'p3']);
    expectRightOf(saved.layout, 'new', 'p1');
  });

  it('places the pane beside its target even when a workspace refresh showed it first', async () => {
    open(row('p1', 'p2', 'p3'), 'auto', 'p1');
    vi.mocked(api.createPane).mockImplementation(async () => {
      // The daemon has already added the pane to the saved workspace; a
      // refresh that lands before the create call returns appends it by policy.
      useStore.setState(s => ({ panes: { ...s.panes, new: pane('new') }, layout: balancedLayout(row('p1', 'p2', 'p3', 'new'), 4, 1.6) }));
      return pane('new');
    });
    await splitFocused('horizontal');
    const layout = useStore.getState().layout;
    expect(collectPaneIds(layout)).toEqual(['p1', 'new', 'p2', 'p3']);
    expectRightOf(layout, 'new', 'p1');
  });
});

describe('opening a pane without naming a side keeps the workspace placement policy', () => {
  it('a new session in an auto workspace joins the balanced grid', async () => {
    open(row('p1', 'p2', 'p3'), 'auto', 'p1');
    await useStore.getState().createPane({ cwd: '/repo', cmd: ['bash'] });
    expect(collectPaneIds(useStore.getState().layout)).toEqual(['p1', 'p2', 'p3', 'new']);
    expect(useStore.getState().paneLayoutMode).toBe('auto');
    expect(useStore.getState().layout).toEqual(balancedLayout(row('p1', 'p2', 'p3', 'new'), 4, 1.6));
  });

  it('Split right from a single-pane view cannot edit the hidden grid, so the pane joins it by policy', async () => {
    open(row('p1', 'p2', 'p3'), 'auto', 'p1');
    useStore.getState().showWorkspacePaneView('p1', false);
    await splitFocused('horizontal');
    expect(useStore.getState().paneLayoutMode).toBe('auto');
    expect(useStore.getState().layout).toEqual(balancedLayout(row('p1', 'p2', 'p3', 'new'), 4, 1.6));
  });
});
