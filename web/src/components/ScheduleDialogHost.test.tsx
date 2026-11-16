import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import { api } from '../api';
import { ScheduleDialogHost } from './ScheduleDialogHost';
import { useEntitlements } from '../entitlements';

function pane(id: string, cwd: string) {
  return { id, cmd: ['bash'], cwd, liveCwd: cwd, status: 'running', title: id } as never;
}

function openFromEvent(detail?: { paneId?: string; fromPaneMenu?: boolean }) {
  act(() => {
    window.dispatchEvent(new CustomEvent('solu:open-schedule-dialog', { detail }));
  });
}

describe('ScheduleDialogHost', () => {
  beforeEach(() => {
    useStore.setState({
      panes: { p1: pane('p1', '/home/me/one'), p2: pane('p2', '/home/me/two') },
      focusedPaneId: 'p1',
    });
    useEntitlements.setState({ loaded: true, team: false, operator: false });
    vi.spyOn(api, 'listScheduleAgents').mockResolvedValue([]);
    vi.spyOn(api, 'previewCron').mockResolvedValue({ fires: [] });
    vi.spyOn(api, 'githubAppStatus').mockResolvedValue({ configured: false, connected: false } as never);
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ claudeChecked: true, claudeInstalled: true } as never);
  });

  afterEach(() => {
    useStore.setState({ panes: {}, focusedPaneId: null });
    vi.restoreAllMocks();
  });

  // The focused pane changes on background events (a pane opening, an agent
  // moving focus). The form was seeded from it on every change, so whatever
  // the person had typed was wiped and the recipe list came back.
  it('keeps what was typed when the focused pane changes while the form is open', async () => {
    render(<ScheduleDialogHost />);
    openFromEvent();
    fireEvent.click(screen.getByText(/Start from scratch/));
    const task = await screen.findByPlaceholderText(/What should Claude do each run/);
    fireEvent.change(task, { target: { value: 'my careful prompt' } });
    expect(screen.getByLabelText('Working directory')).toHaveValue('/home/me/one');

    act(() => { useStore.setState({ focusedPaneId: 'p2' }); });

    await waitFor(() => {
      expect(screen.queryByText('Start with a recipe')).not.toBeInTheDocument();
    });
    expect(screen.getByPlaceholderText(/What should Claude do each run/)).toHaveValue('my careful prompt');
    // Seeded once, from the pane that was focused when the form opened.
    expect(screen.getByLabelText('Working directory')).toHaveValue('/home/me/one');
  });

  it('seeds the next open from the pane focused at that moment', async () => {
    render(<ScheduleDialogHost />);
    openFromEvent();
    fireEvent.click(screen.getByText(/Start from scratch/));
    expect(await screen.findByLabelText('Working directory')).toHaveValue('/home/me/one');
    fireEvent.click(screen.getByText('Cancel'));
    await waitFor(() => expect(screen.queryByText('New automation')).not.toBeInTheDocument());

    act(() => { useStore.setState({ focusedPaneId: 'p2' }); });
    openFromEvent();
    fireEvent.click(screen.getByText(/Start from scratch/));
    expect(await screen.findByLabelText('Working directory')).toHaveValue('/home/me/two');
  });
});
