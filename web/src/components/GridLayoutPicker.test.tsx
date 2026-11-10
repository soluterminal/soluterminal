import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, expect, it } from 'vitest';
import type { Layout, PaneInfo, Workspace } from '../types';
import { useStore } from '../store';
import { buildNxMFromLeaves } from '../lib/layout';
import { GridLayoutPicker } from './GridLayoutPicker';

const initial = useStore.getState();
afterEach(() => useStore.setState(initial));

it('counts the tiles the overview shows, not hidden workers', () => {
  const ids = ['lead', 'w1', 'w2', 'w3', 'sub', 'human'];
  const panes = Object.fromEntries(ids.map(id => [id, { id, title: id, cwd: '/p', cmd: ['sh'], state: 'idle', status: 'running',
    ...(id.startsWith('w') || id === 'sub' ? { origin: { source: 'agent', byPaneId: 'lead', leadPaneId: 'lead', at: 1 } } : {}) } as PaneInfo]));
  const layout = buildNxMFromLeaves(ids.map(paneId => ({ type: 'pane', paneId } as Layout)), 2, 3)!;
  useStore.setState({ panes, layout, workspaces: [{ name: 'Project', layout, paneIds: ids } as Workspace], activeWorkspace: 'Project',
    activeBackend: null, workspaceView: null, workspaceAllPanes: {}, paneLayoutMode: 'free', mainPresentation: { kind: 'workspace' } });
  render(<GridLayoutPicker />);
  fireEvent.click(screen.getByRole('button', { name: 'Layout: arrange or add panes' }));
  expect(screen.getByText('2 panes now — hover to add or rearrange')).toBeTruthy();
});

it('says what the grid button is for in its tooltip', () => {
  render(<GridLayoutPicker />);
  expect(screen.getByRole('button', { name: 'Layout: arrange or add panes' })).toHaveAttribute('title', 'Layout: arrange or add panes');
});
