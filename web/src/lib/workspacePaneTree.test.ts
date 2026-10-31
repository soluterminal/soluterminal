import { describe, expect, it } from 'vitest';
import type { Layout, PaneInfo, Workspace } from '../types';
import { buildWorkspacePaneTrees, createWorkspacePaneTreeSelector, flattenPaneTree, withoutRemovedWorkspacePanes } from './workspacePaneTree';

const pane = (id: string, by?: string, lead?: string): PaneInfo => ({
  id, title: id, cwd: '/project', cmd: ['sh'], status: 'running', state: 'idle',
  ...(by ? { origin: { source: 'agent', byPaneId: by, leadPaneId: lead ?? by, at: 1 } } : {}),
} as PaneInfo);
const layout: Layout = { type: 'pane', paneId: 'lead' };
const workspace = (name = 'Project', paneIds = ['lead']): Workspace => ({ name, paneIds, layout } as Workspace);
const build = (panes: PaneInfo[], workspaces = [workspace()]) => buildWorkspacePaneTrees(workspaces, Object.fromEntries(panes.map(p => [p.id, p])), 'Project', layout);

describe('workspace pane hierarchy', () => {
  it('nests immediate starters and recovers workers missing from every layout', () => {
    const [tree] = build([pane('lead'), pane('worker', 'lead'), pane('sub', 'worker', 'lead')]);
    expect(tree.roots.map(n => n.pane.id)).toEqual(['lead']);
    expect(tree.roots[0].children[0].pane.id).toBe('worker');
    expect(tree.roots[0].children[0].children[0].pane.id).toBe('sub');
    expect(flattenPaneTree(tree.roots).filter(n => n.isLead).map(n => n.pane.id)).toEqual(['lead', 'worker']);
  });
  it('defines a lead by a live child, not its role or historical origin', () => {
    const [tree] = build([pane('lead'), { ...pane('worker', 'lead'), status: 'exited' }]);
    expect(tree.roots[0].isLead).toBe(false);
    expect(tree.roots[0].children[0].isLead).toBe(false);
  });
  it('falls back to a surviving lead when the starter closed', () => {
    const [tree] = build([pane('lead'), pane('sub', 'closed', 'lead')]);
    expect(tree.roots[0].children[0].pane.id).toBe('sub');
    expect(tree.roots[0].isLead).toBe(true);
  });
  it('keeps people-opened panes at the root and true orphans detached', () => {
    const panes = Object.fromEntries([pane('lead'), pane('orphan', 'closed'), pane('human')].map(p => [p.id, p]));
    const full: Layout = { type: 'split', direction: 'horizontal', ratio: 0.5, a: layout, b: { type: 'pane', paneId: 'human' } };
    const [tree] = buildWorkspacePaneTrees([workspace()], panes, 'Project', full);
    expect(tree.paneIds).toEqual(['lead', 'human']);
    expect(tree.roots.map(n => n.pane.id)).toEqual(['lead', 'human']);
  });
  it('uses the recorded workspace when both ancestors closed', () => {
    const orphan = pane('orphan', 'closed');
    orphan.origin!.workspace = 'Project';
    const [tree] = build([pane('lead'), orphan, pane('sub', 'orphan')]);
    expect(tree.roots.map(n => n.pane.id)).toEqual(['lead', 'orphan']);
    expect(tree.roots[1].children[0].pane.id).toBe('sub');
  });
  it('respects an explicit move to another workspace', () => {
    const trees = build([pane('lead'), pane('worker', 'lead')], [workspace(), workspace('Other', ['worker'])]);
    expect(trees[0].roots[0].children).toEqual([]);
    expect(trees[1].roots[0].pane.id).toBe('worker');
    expect(trees[0].roots[0].isLead).toBe(true);
  });
  it('breaks cycles and self links so every pane remains reachable once', () => {
    const panes = Object.fromEntries([pane('lead', 'worker'), pane('worker', 'lead'), pane('self', 'self')].map(p => [p.id, p]));
    const full: Layout = { type: 'split', direction: 'horizontal', ratio: 0.5, a: layout, b: { type: 'pane', paneId: 'self' } };
    const [tree] = buildWorkspacePaneTrees([workspace()], panes, 'Project', full);
    expect(flattenPaneTree(tree.roots).map(n => n.pane.id).sort()).toEqual(['lead', 'self', 'worker']);
  });
  it('does not attach a worker explicitly owned by another backend', () => {
    const win = { ...pane('worker', 'lead'), backend: 'win' as const };
    const trees = build([pane('lead'), win], [workspace(), { ...workspace('Other', ['worker']), backend: 'win' }]);
    expect(trees[0].paneIds).toEqual(['lead']);
    expect(trees[1].paneIds).toEqual(['worker']);
  });
  it('never recovers tombstoned panes from a stale live snapshot or saved layout', () => {
    const closed = { ...workspace(), removedPaneIds: ['worker'] };
    const [tree] = build([pane('lead'), pane('worker', 'lead'), pane('sub', 'worker', 'lead')], [closed]);
    expect(tree.paneIds).toEqual(['lead', 'sub']);
    expect(tree.roots[0].children.map(n => n.pane.id)).toEqual(['sub']);
    expect(tree.nodes.some(n => n.pane.id === 'worker')).toBe(false);
  });

  it('scopes tombstones to the pane backend', () => {
    const panes = { lead: { ...pane('lead'), backend: 'win' as const } };
    const workspaces = [{ ...workspace(), backend: 'wsl' as const, removedPaneIds: ['lead'] }];
    expect(withoutRemovedWorkspacePanes(workspaces, panes, 'win')).toBe(panes);
    expect(withoutRemovedWorkspacePanes(workspaces, { lead: pane('lead') }, 'wsl')).toEqual({});
  });

  it('reuses the tree and summaries across split dragging and ratio-only autosaves', () => {
    const panes = { lead: pane('lead'), worker: pane('worker', 'lead') };
    const full: Layout = { type: 'split', direction: 'horizontal', ratio: 0.3, a: layout, b: { type: 'pane', paneId: 'worker' } };
    const saved = { ...workspace(), layout: full, paneIds: ['lead', 'worker'] };
    const select = createWorkspacePaneTreeSelector();
    const original = select([saved], panes, 'Project', full);
    for (const ratio of [0.4, 0.5, 0.7]) {
      const resized = { ...full, ratio };
      const next = select([{ ...saved, layout: resized, activeAt: String(ratio) }], panes, 'Project', resized);
      expect(next).toBe(original);
      expect(next[0].nodes).toBe(original[0].nodes);
      expect(next[0].roots[0].descendantCount).toBe(1);
    }
    const busy = select([saved], { ...panes, worker: { ...panes.worker, state: 'busy' } }, 'Project', full);
    expect(busy).not.toBe(original);
    expect(busy[0].status).toBe('busy');
    const closed = select([{ ...saved, removedPaneIds: ['worker'] }], panes, 'Project', full);
    expect(closed[0].nodes.map(n => n.pane.id)).toEqual(['lead']);
    expect(closed[0].hasLeads).toBe(false);
    expect(closed[0].roots[0].descendantCount).toBe(0);
  });

});
