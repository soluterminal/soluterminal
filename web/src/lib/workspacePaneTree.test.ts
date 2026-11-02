import { describe, expect, it } from 'vitest';
import type { Layout, PaneInfo, Workspace } from '../types';
import { buildWorkspacePaneTrees, createWorkspacePaneTreeSelector, flattenPaneTree, withoutRemovedWorkspacePanes } from './workspacePaneTree';
import { paneTeamKind as paneTeamKindOf } from './paneTeam';

const pane = (id: string, by?: string, lead?: string): PaneInfo => ({
  id, title: id, cwd: '/project', cmd: ['sh'], status: 'running', state: 'idle',
  ...(by ? { origin: { source: 'agent', byPaneId: by, leadPaneId: lead ?? by, at: 1 } } : {}),
} as PaneInfo);
const layout: Layout = { type: 'pane', paneId: 'lead' };
const workspace = (name = 'Project', paneIds = ['lead']): Workspace => ({ name, paneIds, layout } as Workspace);
const build = (panes: PaneInfo[], workspaces = [workspace()]) => buildWorkspacePaneTrees(workspaces, Object.fromEntries(panes.map(p => [p.id, p])), 'Project', layout);

describe('workspace pane hierarchy', () => {
  it('keeps every lead top level as a peer, with who started it, and nests workers under their starter', () => {
    const lead = (id: string, by?: string, leadId?: string): PaneInfo => ({ ...pane(id, by, leadId), origin: { source: by ? 'agent' : 'user', ...(by ? { byPaneId: by, leadPaneId: leadId ?? by } : {}), at: 1, teamKind: 'lead', leadRole: id } } as PaneInfo);
    const worker = (id: string, by: string, leadId?: string): PaneInfo => ({ ...pane(id, by, leadId), origin: { ...pane(id, by, leadId).origin!, teamKind: 'worker' } } as PaneInfo);
    const leaf = (paneId: string): Layout => ({ type: 'pane', paneId });
    const row = (ids: string[]): Layout => ids.length === 1 ? leaf(ids[0]) : { type: 'split', direction: 'horizontal', ratio: 0.5, a: leaf(ids[0]), b: row(ids.slice(1)) };
    const [tree] = buildWorkspacePaneTrees([workspace()], Object.fromEntries([
      pane('lead'), // your own pane, no kind: @cmo
      lead('social', 'lead'), // a sub-lead your pane started: a peer
      // leadPaneId is the top of the spawn chain (the daemon's cap and authority key).
      worker('post', 'social', 'lead'), // a sub-lead's worker nests under it
      lead('video', 'post', 'lead'), // a lead a worker started: a peer too
      lead('qa', 'gone'), // its starter closed: top level
      lead('secure'), // a user pane made a lead: top level
      worker('task', 'lead'),
      worker('lost', 'gone'), // a worker with no surviving lead: unattached
    ].map(p => [p.id, p])), 'Project', row(['lead', 'lost', 'qa', 'secure']));
    const shape = (n: { pane: PaneInfo; children: unknown[] }): unknown => [n.pane.id, (n.children as typeof tree.roots).map(shape)];
    expect(tree.roots.map(shape)).toEqual([
      ['lead', [['task', []]]],
      ['lost', []], ['qa', []], ['secure', []],
      ['social', [['post', []]]], ['video', []],
    ]);
    expect(tree.nodes.filter(n => n.isTopLevel).map(n => n.pane.id)).toEqual(['lead', 'qa', 'secure', 'social', 'video']);
    expect(tree.unattached.map(n => n.pane.id)).toEqual(['lost']);
    // Provenance stays: who started each lead, as it reads now. A lead whose
    // starter closed, or one you made yourself, has none.
    const startedBy = Object.fromEntries(tree.nodes.filter(n => paneTeamKindOf(n.pane) === 'lead').map(n => [n.pane.id, n.startedBy]));
    expect(startedBy).toEqual({ social: 'lead', video: 'post', qa: undefined, secure: undefined });
  });
  it('keeps a sub-lead top level in a workspace other than its parent', () => {
    const social = { ...pane('social', 'lead'), origin: { source: 'agent', byPaneId: 'lead', leadPaneId: 'lead', at: 1, teamKind: 'lead', leadRole: 'social' } } as PaneInfo;
    const trees = build([pane('lead'), social], [workspace(), workspace('Other', ['social'])]);
    expect(trees[1].roots.map(n => [n.pane.id, n.isTopLevel])).toEqual([['social', true]]);
    expect(trees[1].unattached).toEqual([]);
  });
  it('nests immediate starters and recovers workers missing from every layout', () => {
    const [tree] = build([pane('lead'), pane('worker', 'lead'), pane('sub', 'worker', 'lead')]);
    expect(tree.roots.map(n => n.pane.id)).toEqual(['lead']);
    expect(tree.roots[0].children[0].pane.id).toBe('worker');
    expect(tree.roots[0].children[0].children[0].pane.id).toBe('sub');
    expect(flattenPaneTree(tree.roots).filter(n => n.isTopLevel).map(n => n.pane.id)).toEqual(['lead']);
  });
  it('counts the panes below each node that need the person, apart from the node itself', () => {
    const asking = (p: PaneInfo): PaneInfo => ({ ...p, state: 'choice-prompt', attention: 'needs-user' } as PaneInfo);
    const [tree] = build([pane('lead'), asking(pane('worker', 'lead')), asking(pane('sub', 'worker', 'lead')), pane('quiet', 'lead')]);
    const [lead] = tree.roots;
    const child = (id: string) => lead.children.find(n => n.pane.id === id)!;
    const worker = child('worker'), quiet = child('quiet');
    expect([lead.needsYouBelow, lead.needsYou, lead.status]).toEqual([2, true, 'idle']);
    expect([worker.needsYouBelow, worker.needsYou, worker.status]).toEqual([1, true, 'needs-you']);
    expect([quiet.needsYouBelow, quiet.needsYou]).toEqual([0, false]);
  });

  it('keeps roots and every generation stable when input keys and activity change', () => {
    const dated = (id: string, at: number, by?: string) => ({ ...pane(id, by), createdAt: new Date(at).toISOString() });
    const panes = [dated('lead', 90), dated('other', 10), dated('late', 30, 'lead'),
      dated('early', 20, 'lead'), dated('z', 50, 'early'), dated('a', 50, 'early')];
    const full: Layout = { type: 'split', direction: 'horizontal', ratio: 0.5,
      a: layout, b: { type: 'pane', paneId: 'other' } };
    const run = (input: PaneInfo[]) => buildWorkspacePaneTrees([workspace()],
      Object.fromEntries(input.map(p => [p.id, p])), 'Project', full)[0];
    const original = run(panes);
    expect(original.roots.map(n => n.pane.id)).toEqual(['lead', 'other']);
    expect(original.roots[0].children.map(n => n.pane.id)).toEqual(['early', 'late']);
    expect(original.nodes.map(n => n.pane.id)).toEqual(['lead', 'early', 'a', 'z', 'late', 'other']);
    for (let offset = 0; offset < panes.length; offset++) {
      const shuffled = [...panes.slice(offset), ...panes.slice(0, offset)].reverse();
      expect(run(shuffled.map(p => ({ ...p, state: 'busy' }))).paneIds).toEqual(original.paneIds);
    }
  });
  it('orders recovered roots by creation time then ID regardless of saved membership order', () => {
    const recovered = (id: string, at: number) => ({ ...pane(id, 'closed'),
      origin: { ...pane(id, 'closed').origin!, workspace: 'Project', at } });
    const panes = [pane('lead'), recovered('z', 20), recovered('b', 10), recovered('a', 10)];
    for (const input of [panes, [...panes].reverse()]) {
      const [tree] = build(input);
      expect(tree.roots.map(n => n.pane.id)).toEqual(['lead', 'a', 'b', 'z']);
    }
  });
  it('uses saved layout positions for inactive workspaces and children before creation time', () => {
    const full: Layout = { type: 'split', direction: 'horizontal', ratio: 0.5,
      a: { type: 'pane', paneId: 'z' }, b: { type: 'pane', paneId: 'a' } };
    const [tree] = buildWorkspacePaneTrees([{ ...workspace(), layout: full, paneIds: ['lead'] }],
      { lead: pane('lead'), a: pane('a', 'lead'), z: pane('z', 'lead') }, 'Other', layout);
    expect(tree.roots[0].children.map(n => n.pane.id)).toEqual(['z', 'a']);
  });
  it('falls back to origin time and ID for panes absent from the layout', () => {
    const [tree] = build([pane('lead'), { ...pane('z', 'lead'), origin: { ...pane('z', 'lead').origin!, at: 2 } },
      pane('b', 'lead'), pane('a', 'lead'), { ...pane('invalid', 'lead'), createdAt: '0001-01-01T00:00:00Z' }]);
    expect(tree.roots[0].children.map(n => n.pane.id)).toEqual(['a', 'b', 'invalid', 'z']);
  });
  it('keeps human-opened panes top-level even after all workers finish', () => {
    const [tree] = build([pane('lead'), { ...pane('worker', 'lead'), status: 'exited' }]);
    expect(tree.roots[0].isTopLevel).toBe(true);
    expect(tree.roots[0].children[0].isTopLevel).toBe(false);
  });
  it('falls back to a surviving lead when the starter closed', () => {
    const [tree] = build([pane('lead'), pane('sub', 'closed', 'lead')]);
    expect(tree.roots[0].children[0].pane.id).toBe('sub');
    expect(tree.roots[0].isTopLevel).toBe(true);
  });
  it('keeps people-opened panes at the root and true orphans detached', () => {
    const panes = Object.fromEntries([pane('lead'), pane('orphan', 'closed'), pane('human')].map(p => [p.id, p]));
    const full: Layout = { type: 'split', direction: 'horizontal', ratio: 0.5, a: layout, b: { type: 'pane', paneId: 'human' } };
    const [tree] = buildWorkspacePaneTrees([workspace()], panes, 'Project', full);
    // No surviving ancestor and no recorded home: Background sessions own it.
    expect(tree.paneIds).toEqual(['lead', 'human']);
    expect(tree.unattached).toEqual([]);
    const recorded = { ...panes.orphan, origin: { ...panes.orphan.origin!, workspace: 'Project' } };
    const [home] = buildWorkspacePaneTrees([workspace()], { ...panes, orphan: recorded }, 'Project', full);
    expect(home.unattached.map(n => n.pane.id)).toEqual(['orphan']);
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
    expect(trees[0].roots[0].isTopLevel).toBe(true);
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

  it('keeps plain panes top-level across status changes and nests agent-started leads', () => {
    for (const status of ['running', 'exited'] as const) {
      const [tree] = build([pane('lead'), { ...pane('human'), status },
        { ...pane('worker', 'lead'), role: 'orchestrator' }, pane('nested', 'worker')],
        [workspace('Other', ['lead', 'human'])]);
      expect(tree.nodes.filter(n => n.isTopLevel).map(n => n.pane.id)).toEqual(['lead', 'human']);
      expect(tree.nodes.find(n => n.pane.id === 'worker')!.children[0].pane.id).toBe('nested');
    }
  });
  it('recovers an unattached subtree only into its recorded workspace and backend, respecting tombstones', () => {
    const orphan = { ...pane('orphan', 'closed'), backend: 'wsl' as const };
    orphan.origin!.workspace = 'Project';
    const foreign = { ...pane('foreign', 'closed'), backend: 'win' as const };
    foreign.origin!.workspace = 'Project';
    const [tree] = buildWorkspacePaneTrees([{ ...workspace(), backend: 'wsl', removedPaneIds: ['removed'] }],
      { lead: pane('lead'), orphan, foreign, sub: pane('sub', 'orphan'), removed: { ...pane('removed', 'closed'), origin: { ...orphan.origin! } } },
      'Project', layout, 'wsl');
    expect(tree.unattached.map(n => n.pane.id)).toEqual(['orphan']);
    expect(tree.unattached[0].children.map(n => n.pane.id)).toEqual(['sub']);
    expect(tree.paneIds).not.toContain('foreign');
    expect(tree.paneIds).not.toContain('removed');
  });
  it('leaves home-less orphans and their descendants to Background sessions on every switch', () => {
    const panes = { orphan: pane('orphan', 'closed'), child: pane('child', 'orphan') };
    panes.orphan.origin!.workspace = 'Deleted workspace';
    const alpha = { ...workspace('Alpha', []), layout: null };
    const beta = { ...workspace('Beta', []), layout: null };
    for (const active of ['Alpha', 'Beta', '_current']) {
      const current = { ...workspace('_current', []), layout: null };
      for (const workspaces of [[beta, alpha, current], [current, alpha, beta]]) {
        const trees = buildWorkspacePaneTrees(workspaces, panes, active, null);
        expect(trees.filter(t => t.paneIds.includes('orphan') || t.paneIds.includes('child'))).toEqual([]);
      }
    }
    panes.orphan.origin!.workspace = 'Beta';
    const trees = buildWorkspacePaneTrees([alpha, beta], panes, 'Alpha', null);
    expect(trees.find(t => t.workspace.name === 'Beta')!.paneIds).toEqual(['orphan', 'child']);
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
    expect(closed[0].hasTopLevelPanes).toBe(true);
    expect(closed[0].roots[0].descendantCount).toBe(0);
  });

});

describe('worker records', () => {
  it('nests an auto worker whose caller had closed, but never your own pane', () => {
    const api = { ...pane('auto'), origin: { source: 'api', byPaneId: 'lead', leadPaneId: 'lead', at: 1, worker: { state: 'running' } } } as PaneInfo;
    const mine = { ...pane('mine'), origin: { source: 'user', at: 1, worker: { state: 'running' } } } as PaneInfo;
    const [tree] = buildWorkspacePaneTrees([workspace('Project', ['lead', 'auto', 'mine'])], { lead: pane('lead'), auto: api, mine }, 'Other', null);
    expect(tree.nodes.filter(n => n.isTopLevel).map(n => n.pane.id).sort()).toEqual(['lead', 'mine']);
  });
});
