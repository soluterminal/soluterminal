import { act, fireEvent, render, renderHook, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setApiPrincipalAccess, setBackend } from '../api';
import { setCloudDualBackend } from '../dualBackend';
import { useStore } from '../store';
import type { PaneInfo, Workspace } from '../types';
import { DialogHost } from './DialogHost';
import { usePaneContextMenu } from './usePaneContextMenu';

const pane = { id: 'moving', pid: 11, cmd: ['bash'], title: 'My session', status: 'running' } as PaneInfo;
const workspace = (name: string, backend?: 'wsl' | 'win'): Workspace => ({ name, backend, layout: null, paneIds: [], activeAt: '' });
const movePaneToWorkspace = useStore.getState().movePaneToWorkspace;

beforeEach(() => {
  setCloudDualBackend(false); setBackend(null); setApiPrincipalAccess(true);
  useStore.setState({ movePaneToWorkspace, activeWorkspace: 'Source', activeBackend: 'wsl', navigationGeneration: 0,
    panes: { moving: pane }, workspaces: [workspace('Source', 'wsl'), workspace('A destination', 'wsl'),
      workspace('Z destination', 'wsl'), workspace('Windows only', 'win')], dialog: null });
});
afterEach(() => { useStore.getState().closeDialog(null); vi.restoreAllMocks(); });

function hook() {
  return renderHook(() => usePaneContextMenu({ pane, killPane: vi.fn(), setFocus: vi.fn(),
    setTitleDraft: vi.fn(), setEditingTitle: vi.fn(), setFlagMemoDraft: vi.fn(), setEditingFlagMemo: vi.fn() }));
}

describe('workspace move destination picker', () => {
  it('selects a named workspace on the same backend and sends that choice', async () => {
    const move = vi.spyOn(useStore.getState(), 'movePaneToWorkspace').mockResolvedValue();
    const { result } = hook(); render(<DialogHost />);
    let pending!: Promise<void>;
    act(() => { pending = result.current.actions.moveToWorkspace(); });
    await screen.findByRole('dialog', { name: 'Move pane to workspace' });
    expect(screen.getAllByRole('option').map(el => el.textContent)).toEqual(['A destination', 'Z destination']);
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'Z destination' } });
    fireEvent.click(screen.getByRole('button', { name: 'Move' }));
    await act(async () => { await pending; });
    expect(move).toHaveBeenCalledWith('moving', 'Z destination', expect.objectContaining({ key: expect.any(String) }));
  });

  it('cancels without moving', async () => {
    const move = vi.spyOn(useStore.getState(), 'movePaneToWorkspace').mockResolvedValue();
    const { result } = hook(); render(<DialogHost />);
    let pending!: Promise<void>;
    act(() => { pending = result.current.actions.moveToWorkspace(); });
    await screen.findByRole('dialog');
    fireEvent.keyDown(screen.getByRole('combobox'), { key: 'Escape' });
    await act(async () => { await pending; });
    expect(move).not.toHaveBeenCalled();
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('ignores a stale picker after the user navigates elsewhere', async () => {
    const move = vi.spyOn(useStore.getState(), 'movePaneToWorkspace').mockResolvedValue();
    const { result } = hook(); render(<DialogHost />);
    let pending!: Promise<void>;
    act(() => { pending = result.current.actions.moveToWorkspace(); });
    await screen.findByRole('dialog');
    act(() => useStore.setState({ navigationGeneration: 1, activeWorkspace: 'Elsewhere' }));
    fireEvent.click(screen.getByRole('button', { name: 'Move' }));
    await act(async () => { await pending; });
    await waitFor(() => expect(move).not.toHaveBeenCalled());
  });
});

describe('Make lead from the pane menu', () => {
  it('sends the role to the pane\'s own daemon and updates its origin', async () => {
    const { api } = await import('../api');
    const origin = { source: 'user' as const, at: 1, teamKind: 'lead' as const, leadRole: 'security' };
    const set = vi.spyOn(api, 'setPaneTeam').mockResolvedValue({ origin });
    const { result } = hook(); render(<DialogHost />);
    let pending!: Promise<void>;
    act(() => { pending = result.current.actions.setTeamKind('lead'); });
    await screen.findByRole('dialog', { name: 'Make lead' });
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '@security ' } });
    fireEvent.click(screen.getByRole('button', { name: 'Make lead' }));
    await act(async () => { await pending; });
    expect(set).toHaveBeenCalledWith('moving', 'lead', 'security', expect.objectContaining({ key: expect.any(String) }));
    expect(useStore.getState().panes.moving.origin).toEqual(origin);
  });

  it('drops a pending role after the user navigates elsewhere', async () => {
    const { api } = await import('../api');
    const set = vi.spyOn(api, 'setPaneTeam').mockResolvedValue({ origin: { source: 'user', at: 1 } });
    const { result } = hook(); render(<DialogHost />);
    let pending!: Promise<void>;
    act(() => { pending = result.current.actions.setTeamKind('lead'); });
    await screen.findByRole('dialog');
    act(() => useStore.setState({ navigationGeneration: 1 }));
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'qa' } });
    fireEvent.click(screen.getByRole('button', { name: 'Make lead' }));
    await act(async () => { await pending; });
    expect(set).not.toHaveBeenCalled();
  });
});
