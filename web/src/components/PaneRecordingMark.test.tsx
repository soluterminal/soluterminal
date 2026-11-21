import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { PaneInfo } from '../types';
import { sameRecording, useStore } from '../store';
import { PaneRecordingMark, RecordingStatusChip } from './PaneRecordingMark';
import { PaneHeaderBadges } from './PaneHeaderBadges';
import { paneRecordable, startPaneRecording, startWorkspaceRecording } from '../lib/paneRecording';
import { api } from '../api';

const pane: PaneInfo = {
  id: 'rec-pane', cwd: '/project', cmd: ['claude'], title: 'Build', agent: 'claude',
  status: 'running', cols: 80, rows: 24, pid: 0,
  lastActivityMs: 0, hasClaude: true, hasCodex: false,
  state: 'idle', createdAt: '2026-11-19T00:00:00Z',
};

const original = useStore.getState();
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  useStore.setState({ panes: original.panes, openDialog: original.openDialog, toast: original.toast } as never);
});

describe('PaneRecordingMark', () => {
  it('shows nothing while off', () => {
    const { container } = render(<PaneRecordingMark pane={pane} place="header" />);
    expect(container.textContent).toBe('');
  });

  it('shows REC while the daemon says the pane is recording', () => {
    render(<PaneRecordingMark pane={{ ...pane, recording: { on: true, startedAt: 1 } }} place="header" />);
    expect(screen.getByTestId('pane-rec-header').textContent).toBe('REC');
    expect(screen.getByTestId('pane-rec-header').getAttribute('title')).toContain('Saved on this computer only');
    expect(screen.getByTestId('pane-rec-header').getAttribute('title')).toContain('deleted after 7 days');
  });

  it('names the deletion age and time limit the daemon reports', () => {
    render(<PaneRecordingMark pane={{ ...pane, recording: { on: true, startedAt: 1, hours: 4, deleteAfterDays: 21 } }} place="header" />);
    const title = screen.getByTestId('pane-rec-header').getAttribute('title');
    expect(title).toContain('deleted after 21 days');
    expect(title).toContain('after 4 hours');
  });

  it('shows the stop reason until dismissed', () => {
    render(<PaneRecordingMark pane={{ ...pane, recording: { on: false, stoppedReason: 'disk cap', stoppedAt: 2 } }} place="header" />);
    expect(screen.getByTestId('pane-rec-stopped-header').textContent).toContain('recording stopped: disk cap');
    expect(screen.getByRole('button', { name: 'Dismiss' })).toBeTruthy();
  });

  it('is on the always-visible header section, never folded into details', () => {
    const recording = { ...pane, recording: { on: true, startedAt: 1 } };
    const { unmount } = render(<PaneHeaderBadges pane={recording} paneIndex={1} section="details" />);
    expect(screen.queryByTestId('pane-rec-header')).toBeNull();
    unmount();
    render(<PaneHeaderBadges pane={recording} paneIndex={1} section="attention" />);
    expect(screen.getByTestId('pane-rec-header')).toBeTruthy();
  });
});

describe('RecordingStatusChip', () => {
  it('counts recording panes in every workspace and hides at zero', () => {
    useStore.setState({ panes: { a: pane } } as never);
    const { container, rerender } = render(<RecordingStatusChip />);
    expect(container.textContent).toBe('');
    act(() => {
      useStore.setState({
        panes: {
          a: { ...pane, recording: { on: true } },
          b: { ...pane, id: 'b', title: 'Other', recording: { on: true } },
          c: { ...pane, id: 'c', recording: { on: false, stoppedReason: 'disk cap' } },
        },
      } as never);
    });
    rerender(<RecordingStatusChip />);
    expect(screen.getByTestId('statusbar-recording').textContent).toBe('2 recording');
  });
});

describe('recording state', () => {
  it('ignores byte counts when deciding whether a pane changed', () => {
    expect(sameRecording({ on: true, startedAt: 1, bytesRecorded: 5 }, { on: true, startedAt: 1, bytesRecorded: 9 })).toBe(true);
    expect(sameRecording(undefined, { on: true, startedAt: 1 })).toBe(false);
    expect(sameRecording({ on: false, stoppedReason: 'disk cap' }, undefined)).toBe(false);
    expect(sameRecording(undefined, undefined)).toBe(true);
  });

  it('applies the daemon event to the pane', () => {
    useStore.setState({ panes: { [pane.id]: pane } } as never);
    useStore.getState().applyPaneRecording(pane.id, { on: true, startedAt: 3 });
    expect(useStore.getState().panes[pane.id].recording?.on).toBe(true);
    useStore.getState().applyPaneRecording(pane.id, { on: false });
    expect(useStore.getState().panes[pane.id].recording).toBeUndefined();
  });

  it('offers recording only on running Claude and Codex panes outside a Windows daemon', () => {
    expect(paneRecordable(pane)).toBe(true);
    expect(paneRecordable({ ...pane, cmd: ['codex'], agent: 'codex' })).toBe(true);
    expect(paneRecordable({ ...pane, cmd: ['bash'], agent: undefined, hasClaude: false })).toBe(false);
    expect(paneRecordable({ ...pane, status: 'exited' })).toBe(false);
    expect(paneRecordable({ ...pane, backend: 'win' })).toBe(false);
  });
});

describe('startPaneRecording', () => {
  it('asks first, and starts only if the pane is still the one agreed to', async () => {
    const set = vi.spyOn(api, 'setPaneRecording').mockResolvedValue({ on: true, startedAt: 4 });
    const toast = vi.fn();
    useStore.setState({
      panes: { [pane.id]: pane },
      openDialog: (d: { resolve: (v: unknown) => void }) => d.resolve(true),
      toast,
    } as never);
    await startPaneRecording(pane, undefined, () => false);
    expect(set).not.toHaveBeenCalled();
    expect(toast).toHaveBeenCalled();
    await startPaneRecording(pane, undefined, () => true);
    expect(set).toHaveBeenCalledWith(pane.id, true, undefined, undefined);
    expect(useStore.getState().panes[pane.id].recording?.on).toBe(true);
  });

  it('does nothing when the person cancels', async () => {
    const set = vi.spyOn(api, 'setPaneRecording');
    useStore.setState({ openDialog: (d: { resolve: (v: unknown) => void }) => d.resolve(false) } as never);
    await startPaneRecording(pane);
    expect(set).not.toHaveBeenCalled();
  });
});

describe('startPaneRecording with a v2 daemon (ST-317)', () => {
  it('sends the time limit and the overrides the person ticked', async () => {
    const set = vi.spyOn(api, 'setPaneRecording').mockResolvedValue({ on: true, startedAt: 4, hours: 4, overrides: ['shell'] });
    vi.spyOn(api, 'getRecordingSettings').mockResolvedValue({ deleteAfterDays: 3, capMB: 2000, bounds: {} as never });
    let opened: { kind: string; message: string; recording?: { panes: { defaults: string[] }[]; deleteAfterDays: number } } | undefined;
    useStore.setState({
      panes: { [pane.id]: pane },
      openDialog: (d: typeof opened & { resolve: (v: unknown) => void }) => {
        opened = d;
        d.resolve(JSON.stringify({ hours: 4, panes: [{ id: pane.id, override: ['shell'] }] }));
      },
    } as never);
    await startPaneRecording(pane, undefined, () => true, { check: { defaults: ['shell'] } });
    expect(opened?.kind).toBe('recording');
    expect(opened?.message).toContain('deleted after 3 days');
    expect(opened?.recording?.panes[0].defaults).toEqual(['shell']);
    expect(set).toHaveBeenCalledWith(pane.id, true, undefined, { hours: 4, override: ['shell'] });
  });

  it('does nothing when the person cancels the dialog', async () => {
    const set = vi.spyOn(api, 'setPaneRecording');
    vi.spyOn(api, 'getRecordingSettings').mockResolvedValue({ deleteAfterDays: 7, capMB: 2000, bounds: {} as never });
    useStore.setState({ openDialog: (d: { resolve: (v: unknown) => void }) => d.resolve(null) } as never);
    await startPaneRecording(pane, undefined, () => true, { check: {} });
    expect(set).not.toHaveBeenCalled();
  });

  it('asks nothing and starts nothing when the deletion age cannot be read', async () => {
    const set = vi.spyOn(api, 'setPaneRecording');
    vi.spyOn(api, 'getRecordingSettings').mockRejectedValue(new Error('x'));
    const openDialog = vi.fn();
    const toast = vi.fn();
    useStore.setState({ openDialog, toast } as never);
    await startPaneRecording(pane, undefined, () => true, { check: {} });
    expect(openDialog).not.toHaveBeenCalled();
    expect(set).not.toHaveBeenCalled();
    expect(toast).toHaveBeenCalled();
  });
});

describe('the deletion age stated in the dialog', () => {
  it('starts nothing if it changed while the person decided', async () => {
    const set = vi.spyOn(api, 'setPaneRecording');
    vi.spyOn(api, 'getRecordingSettings')
      .mockResolvedValueOnce({ deleteAfterDays: 7, capMB: 2000, bounds: {} as never })
      .mockResolvedValueOnce({ deleteAfterDays: 30, capMB: 2000, bounds: {} as never });
    const toast = vi.fn();
    useStore.setState({
      panes: { [pane.id]: pane }, toast,
      openDialog: (d: { resolve: (v: unknown) => void }) => d.resolve(JSON.stringify({ hours: 8, panes: [{ id: pane.id, override: [] }] })),
    } as never);
    await startPaneRecording(pane, undefined, () => true, { check: {} });
    expect(set).not.toHaveBeenCalled();
    expect(toast.mock.calls[0][0]).toContain('changed while you were deciding');
  });

  it('sends the pane process the person confirmed', async () => {
    const set = vi.spyOn(api, 'setPaneRecording').mockResolvedValue({ on: true });
    vi.spyOn(api, 'getRecordingSettings').mockResolvedValue({ deleteAfterDays: 7, capMB: 2000, bounds: {} as never });
    const withPid = { ...pane, pid: 4321 };
    useStore.setState({
      panes: { [pane.id]: withPid },
      openDialog: (d: { resolve: (v: unknown) => void }) => d.resolve(JSON.stringify({ hours: 8, panes: [{ id: pane.id, override: [] }] })),
    } as never);
    await startPaneRecording(withPid, undefined, () => true, { check: {} });
    expect(set).toHaveBeenCalledWith(pane.id, true, undefined, { hours: 8, override: [], pid: 4321 });
  });
});

describe('startWorkspaceRecording (ST-317)', () => {
  it('drops a pane restarted while the person decided', async () => {
    const other = { ...pane, id: 'rec-other', pid: 7 };
    vi.spyOn(api, 'getPaneRecording').mockResolvedValue({ on: false, check: {} });
    vi.spyOn(api, 'getRecordingSettings').mockResolvedValue({ deleteAfterDays: 7, capMB: 2000, bounds: {} as never });
    const send = vi.spyOn(api, 'setWorkspaceRecording').mockResolvedValue({ recorded: [pane.id], skipped: [] });
    const toast = vi.fn();
    useStore.setState({
      panes: { [pane.id]: pane, [other.id]: other },
      toast,
      openDialog: (d: { resolve: (v: unknown) => void }) => {
        // While the dialog is open the second pane restarts in place.
        useStore.setState({ panes: { [pane.id]: pane, [other.id]: { ...other, pid: 8 } } } as never);
        d.resolve(JSON.stringify({ hours: 8, panes: [{ id: pane.id, override: [] }, { id: other.id, override: [] }] }));
      },
    } as never);
    await startWorkspaceRecording('Team', [pane, other], undefined, () => true);
    expect(send).toHaveBeenCalledWith({ workspace: 'Team', on: true, hours: 8, panes: [{ id: pane.id, override: [] }] }, undefined);
    expect(toast.mock.calls[0][0]).toContain("won't be recorded");
  });
});
