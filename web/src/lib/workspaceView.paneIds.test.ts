import { describe, expect, it } from 'vitest';
import type { Layout, PaneInfo, Workspace } from '../types';
import { workspacePaneIds } from './workspaceView';

const pane = (id: string, by?: string): PaneInfo => ({
  id, title: id, cwd: '/project', cmd: ['sh'], status: 'running', state: 'idle',
  ...(by ? { origin: { source: 'agent', byPaneId: by, leadPaneId: by, at: 1 } } : {}),
} as PaneInfo);

describe('workspacePaneIds', () => {
  const layout: Layout = { type: 'pane', paneId: 'lead' };
  const panes = Object.fromEntries([pane('lead'), pane('w1', 'lead'), pane('elsewhere')].map(p => [p.id, p]));
  const workspaces = [
    { name: 'Project', layout, paneIds: ['lead'] },
    { name: 'Other', layout: { type: 'pane', paneId: 'elsewhere' }, paneIds: ['elsewhere'] },
  ] as Workspace[];

  it('includes a worker recovered beside its lead even though no tile was saved for it', () => {
    const ids = workspacePaneIds({ layout, panes, workspaces, activeWorkspace: 'Project', activeBackend: null });
    expect(ids.sort()).toEqual(['lead', 'w1']);
  });

  it('falls back to the layout tiles when the store has no panes yet', () => {
    expect(workspacePaneIds({ layout })).toEqual(['lead']);
  });
});
