import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError, setApiPrincipalAccess, setBackend } from './api';
import { setCloudDualBackend } from './dualBackend';
import { useStore } from './store';
import { autoGridRows, balancedLayout } from './lib/autoLayout';
import { collectPaneIds } from './lib/layout';
import type { Layout, PaneInfo, Workspace } from './types';

const pane = (id: string): PaneInfo => ({ id, title: id, pid: 1, status: 'running', cwd: '/repo', cmd: ['bash'], cols: 80, rows: 24, createdAt: '', state: 'idle', hasClaude: false, lastActivityMs: 0 });
const leaf = (paneId: string): Layout => ({ type: 'pane', paneId });
function rows(layout: Layout | null): number[] {
  if (!layout) return [];
  if (layout.type === 'split' && layout.direction === 'vertical') return [...rows(layout.a), ...rows(layout.b)];
  return [collectPaneIds(layout).length];
}
let serial = 0, workspace: Workspace, revision: number;
function withPanes(count: number, mode: 'auto' | 'free' = 'auto') {
  const panes = Object.fromEntries(Array.from({ length: count }, (_, i) => [`p${i + 1}`, pane(`p${i + 1}`)]));
  let layout: Layout | null = null;
  for (const id of Object.keys(panes)) layout = layout ? { type: 'split', direction: 'horizontal', ratio: .5, a: layout, b: leaf(id) } : leaf(id);
  if (mode === 'auto') layout = balancedLayout(layout);
  workspace = { ...workspace, layoutMode: mode, layout, paneIds: Object.keys(panes), panes: Object.values(panes).map(p => ({ ...p, kind: undefined })) };
  useStore.setState({ layout, paneLayoutMode: mode, panes, workspaces: [workspace], activeWorkspaceKnownPaneIds: Object.keys(panes), focusedPaneId: 'p1', lastUserFocusId: null });
}
beforeEach(() => {
  vi.useFakeTimers(); localStorage.clear(); setCloudDualBackend(false); setBackend(null); setApiPrincipalAccess(true);
  revision = 0;
  workspace = { name: `Auto layout ${++serial}`, paneIds: [], layout: null, activeAt: '', layoutMode: 'auto', layoutColumns: 4, layoutAspect: 1.6, layoutVersion: 0 };
  useStore.setState({ activeWorkspace: workspace.name, activeWorkspaceMoveVersion: 0, activeWorkspaceLayoutVersion: 0,
    workspaceLayoutDirty: false, paneLayoutMode: 'auto', layoutColumns: 4, layoutAspect: 1.6, activeBackend: null,
    mainPresentation: { kind: 'workspace' }, managedOwnerBindings: new Map(), navigationGeneration: 0,
    zoomedPaneId: null, browsers: {}, files: {}, parkedPaneIds: [], updateStatus: null, toasts: [], wsOrder: [] });
  withPanes(1);
  vi.spyOn(api, 'saveWorkspace').mockImplementation(async w => ({ ...w, layoutVersion: ++revision }));
  vi.spyOn(api, 'listWorkspaceSummaries').mockImplementation(async () => useStore.getState().workspaces);
  vi.spyOn(api, 'listWorkspaces').mockImplementation(async () => useStore.getState().workspaces);
  vi.spyOn(api, 'listPanes').mockImplementation(async () => Object.values(useStore.getState().panes));
  let nextPane = 1;
  vi.spyOn(api, 'createPane').mockImplementation(async () => pane(`p${++nextPane}`));
});
afterEach(async () => { await vi.advanceTimersByTimeAsync(500); vi.useRealTimers(); vi.restoreAllMocks(); });

describe('workspace auto layout', () => {
  it('balances every open and close while preserving current pane order and focus', async () => {
    for (let count = 2; count <= 8; count++) {
      await useStore.getState().splitFocused('vertical', { cwd: '/repo', cmd: ['bash'] });
      expect(rows(useStore.getState().layout)).toEqual(autoGridRows(count));
      expect(collectPaneIds(useStore.getState().layout)).toEqual(Array.from({ length: count }, (_, i) => `p${i + 1}`));
    }
    useStore.getState().closePaneInLayout('p3');
    expect(rows(useStore.getState().layout)).toEqual([4, 3]);
    expect(useStore.getState().focusedPaneId).toBe('p8');
    useStore.getState().closePaneInLayout('p6');
    expect(rows(useStore.getState().layout)).toEqual([3, 3]);
    expect(api.createPane).toHaveBeenCalledTimes(7);
  });
  it('switches to custom immediately when a divider supplies a new layout', () => {
    withPanes(2);
    const layout = useStore.getState().layout as Extract<Layout, { type: 'split' }>;
    useStore.getState().setLayout({ ...layout, ratio: .3 });
    expect(useStore.getState().paneLayoutMode).toBe('free');
    expect(useStore.getState().layout).toMatchObject({ ratio: .3 });
  });
  it('splits the largest custom pane along its longer side', async () => {
    withPanes(2, 'free');
    useStore.setState({ layout: { type: 'split', direction: 'horizontal', ratio: .2, a: leaf('p1'), b: leaf('p2') } });
    vi.mocked(api.createPane).mockResolvedValue(pane('new'));
    await useStore.getState().splitFocused('vertical', { cwd: '/repo', cmd: ['bash'] });
    expect(useStore.getState().layout).toEqual({ type: 'split', direction: 'horizontal', ratio: .2, a: leaf('p1'),
      b: { type: 'split', direction: 'horizontal', ratio: .5, a: leaf('p2'), b: leaf('new') } });
  });
  it('redistributes a closed custom tile among the other row siblings', () => {
    withPanes(3, 'free');
    useStore.setState({ layout: { type: 'split', direction: 'horizontal', ratio: .2, a: leaf('p1'), b: { type: 'split', direction: 'horizontal', ratio: .375, a: leaf('p2'), b: leaf('p3') } } });
    useStore.getState().closePaneInLayout('p2');
    const layout = useStore.getState().layout;
    expect(layout?.type === 'split' && layout.ratio).toBeCloseTo(.35);
    expect(collectPaneIds(layout)).toEqual(['p1', 'p3']);
  });
  it('re-tiles custom geometry without creating panes or changing focus', () => {
    withPanes(8, 'free');
    useStore.setState({ focusedPaneId: 'p5', lastUserFocusId: null });
    useStore.getState().retilePanes();
    expect(rows(useStore.getState().layout)).toEqual([4, 4]);
    expect(useStore.getState().paneLayoutMode).toBe('auto');
    expect(useStore.getState().focusedPaneId).toBe('p5');
    expect(useStore.getState().lastUserFocusId).toBeNull();
    expect(api.createPane).not.toHaveBeenCalled();
  });
  it('a preset or explicit grid selects custom mode', async () => {
    withPanes(4);
    useStore.getState().applyLayoutPreset('rows');
    expect(useStore.getState().paneLayoutMode).toBe('free');
    useStore.getState().retilePanes();
    await useStore.getState().applyGridLayout(2, 2);
    expect(useStore.getState().paneLayoutMode).toBe('free');
  });
  it('reflows only after the viewport crosses the column hysteresis band', () => {
    withPanes(8);
    useStore.getState().updateLayoutViewport(1260, 1000);
    expect(rows(useStore.getState().layout)).toEqual([4, 4]);
    useStore.getState().updateLayoutViewport(1230, 1000);
    expect(rows(useStore.getState().layout)).toEqual([3, 3, 2]);
    useStore.getState().updateLayoutViewport(1300, 1000);
    expect(rows(useStore.getState().layout)).toEqual([3, 3, 2]);
    useStore.getState().updateLayoutViewport(1330, 1000);
    expect(rows(useStore.getState().layout)).toEqual([4, 4]);
    expect(useStore.getState().focusedPaneId).toBe('p1');
  });
  it('loads the persisted mode of each workspace instead of the browser preference', async () => {
    withPanes(2);
    const custom: Workspace = { ...workspace, name: 'Custom', layoutMode: 'free', layoutVersion: 4 };
    useStore.setState({ workspaces: [workspace, custom] });
    await useStore.getState().loadWorkspace('Custom');
    expect(useStore.getState().paneLayoutMode).toBe('free');
    expect(useStore.getState().activeWorkspaceLayoutVersion).toBe(4);
  });
  it('applies a newer saved layout to a clean view and retains focus', async () => {
    withPanes(2);
    const remote: Workspace = { ...workspace, layoutMode: 'free', layoutVersion: 3,
      layout: { type: 'split', direction: 'vertical', ratio: .7, a: leaf('p1'), b: leaf('p2') } };
    vi.mocked(api.listWorkspaces).mockResolvedValue([remote]);
    await useStore.getState().refreshWorkspaces({ full: true });
    expect(useStore.getState().layout).toEqual(remote.layout);
    expect(useStore.getState().activeWorkspaceLayoutVersion).toBe(3);
    expect(useStore.getState().focusedPaneId).toBe('p1');
    expect(api.saveWorkspace).not.toHaveBeenCalled();
  });
  it('keeps unsaved local edits when another view wins the revision check', async () => {
    withPanes(2);
    const local: Layout = { type: 'split', direction: 'horizontal', ratio: .3, a: leaf('p1'), b: leaf('p2') };
    vi.mocked(api.saveWorkspace).mockRejectedValue(new ApiError(409, 'workspace layout changed in another view'));
    vi.mocked(api.listWorkspaces).mockResolvedValue([{ ...workspace, layoutVersion: 3 }]);
    useStore.getState().setLayout(local);
    await vi.advanceTimersByTimeAsync(401);
    await useStore.getState().refreshWorkspaces({ full: true });
    expect(useStore.getState().layout).toEqual(local);
    expect(useStore.getState().activeWorkspaceLayoutVersion).toBe(0);
    expect(useStore.getState().toasts.some(t => t.message.includes('Your local layout has not been saved'))).toBe(true);
  });
});

it('does not save custom viewport changes', async () => {
  withPanes(8, 'free');
  const layout = useStore.getState().layout;
  useStore.getState().updateLayoutViewport(1000, 700);
  await vi.advanceTimersByTimeAsync(500);
  expect(useStore.getState().layout).toBe(layout);
  expect(useStore.getState().layoutAspect).toBe(1.6);
  expect(api.saveWorkspace).not.toHaveBeenCalled();
});
it('accepts canonical geometry quietly when an automatic viewport save loses a race', async () => {
  withPanes(8);
  const canonical: Workspace = { ...workspace, layoutVersion: 3 };
  vi.mocked(api.saveWorkspace).mockRejectedValue(new ApiError(409, 'workspace layout changed in another view'));
  vi.mocked(api.listWorkspaces).mockResolvedValue([canonical]);
  useStore.getState().updateLayoutViewport(1230, 1000);
  expect(rows(useStore.getState().layout)).toEqual([3, 3, 2]);
  await vi.advanceTimersByTimeAsync(500);
  expect(rows(useStore.getState().layout)).toEqual([4, 4]);
  expect(useStore.getState().activeWorkspaceLayoutVersion).toBe(3);
  expect(useStore.getState().toasts.some(t => t.message.includes('not been saved'))).toBe(false);
});
