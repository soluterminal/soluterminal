import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../api';
import { useStore } from '../../store';
import { buildWorkspacePaneTrees } from '../../lib/workspacePaneTree';
import type { PaneInfo, Workspace } from '../../types';
import { MakeLeadsDialog, leadCandidates } from './MakeLeadsDialog';

const pane = (id: string, extra: Partial<PaneInfo> = {}): PaneInfo => ({
  id, title: id, cwd: '/p', cmd: ['claude'], status: 'running', state: 'idle', pid: 1, cols: 80, rows: 24, createdAt: '2026-11-01T00:00:00Z', ...extra,
} as PaneInfo);
const panes = [
  pane('cmo', { title: '@cmo · marketing', identity: { handle: 'cmo' } }),
  pane('chief', { title: 'Chief of staff' }),
  pane('ops', { title: '@ops', identity: { handle: 'ops' } }),
  pane('secure', { title: '@secure', identity: { handle: 'secure' }, origin: { source: 'user', at: 1, teamKind: 'lead', leadRole: 'security' } }),
  pane('copy', { title: 'Landing copy', origin: { source: 'agent', byPaneId: 'cmo', leadPaneId: 'cmo', at: 2, teamKind: 'worker' } }),
  pane('gone', { title: 'Exited shell', status: 'exited' }),
];
const nodes = () => {
  const ids = panes.map(p => p.id);
  const [tree] = buildWorkspacePaneTrees([{ name: 'Launch', paneIds: ids, layout: null } as unknown as Workspace],
    Object.fromEntries(panes.map(p => [p.id, p])), 'Launch', null);
  return tree.nodes;
};

describe('Make leads…', () => {
  afterEach(() => { vi.restoreAllMocks(); useStore.setState({ panes: {}, navigationGeneration: 0 }); });

  it('offers your own running panes that are not leads or workers, ticking those that started workers', () => {
    expect(leadCandidates(nodes())).toEqual([
      expect.objectContaining({ id: 'chief', name: 'Chief of staff', role: '', checked: false }),
      expect.objectContaining({ id: 'cmo', name: '@cmo · marketing', role: 'cmo', checked: true }),
      expect.objectContaining({ id: 'ops', name: '@ops', role: 'ops', checked: false }),
    ]);
  });

  it('makes every ticked pane a lead with its role and keeps only failures to retry', async () => {
    useStore.setState({ panes: Object.fromEntries(panes.map(p => [p.id, p])) });
    const toast = vi.fn();
    useStore.setState({ toast } as never);
    const setPaneTeam = vi.spyOn(api, 'setPaneTeam').mockImplementation(async (id, _kind, role) => {
      if (id === 'chief') throw new Error('pane is busy');
      return { origin: { source: 'user', at: 1, teamKind: 'lead', leadRole: role } };
    });
    const onClose = vi.fn();
    render(<MakeLeadsDialog workspace="Launch" nodes={nodes()} onClose={onClose} />);
    const submit = () => screen.getByRole('button', { name: /^Make \d+ leads?$/ });
    expect(submit()).toHaveTextContent('Make 1 lead');
    // A ticked pane needs a role before anything is sent.
    fireEvent.click(screen.getByRole('checkbox', { name: 'Chief of staff' }));
    expect(submit()).toBeDisabled();
    fireEvent.change(screen.getByRole('textbox', { name: 'Role for Chief of staff' }), { target: { value: '@chief' } });
    fireEvent.click(screen.getByRole('checkbox', { name: '@ops' }));
    expect(submit()).toHaveTextContent('Make 3 leads');
    await act(async () => { fireEvent.click(submit()); });
    expect(setPaneTeam.mock.calls.map(([id, kind, role]) => [id, kind, role])).toEqual([
      ['chief', 'lead', 'chief'], ['cmo', 'lead', 'cmo'], ['ops', 'lead', 'ops'],
    ]);
    expect(useStore.getState().panes.cmo.origin).toMatchObject({ teamKind: 'lead', leadRole: 'cmo' });
    expect(toast).toHaveBeenCalledWith('Made 2 leads in Launch', expect.anything());
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByText(/Chief of staff: pane is busy/)).toBeInTheDocument();
    expect(screen.getAllByRole('checkbox').map(box => box.getAttribute('aria-label') ?? (box.closest('label')?.textContent))).toEqual(['Chief of staff']);
  });

  it('closes and stops sending when the app moves elsewhere while it is open', async () => {
    useStore.setState({ panes: Object.fromEntries(panes.map(p => [p.id, p])), navigationGeneration: 0 });
    const setPaneTeam = vi.spyOn(api, 'setPaneTeam').mockImplementation(async (_id, _kind, role) => {
      // The person switches machine or view while the first call is in flight.
      useStore.setState({ navigationGeneration: 1 });
      return { origin: { source: 'user', at: 1, teamKind: 'lead', leadRole: role } };
    });
    const onClose = vi.fn();
    render(<MakeLeadsDialog workspace="Launch" nodes={nodes()} onClose={onClose} />);
    fireEvent.click(screen.getByRole('checkbox', { name: '@ops' }));
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Make 2 leads' })); });
    expect(setPaneTeam).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalled();
    // The reply belongs to the old view: the store is left alone.
    expect(useStore.getState().panes.cmo.origin).toBeUndefined();
  });
});
