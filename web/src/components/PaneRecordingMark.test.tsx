import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { PaneInfo } from '../types';
import { sameRecording, useStore } from '../store';
import { PaneRecordingMark, RecordingStatusChip } from './PaneRecordingMark';
import { PaneHeaderBadges } from './PaneHeaderBadges';
import { paneRecordable, startPaneRecording } from '../lib/paneRecording';
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
    expect(set).toHaveBeenCalledWith(pane.id, true, undefined);
    expect(useStore.getState().panes[pane.id].recording?.on).toBe(true);
  });

  it('does nothing when the person cancels', async () => {
    const set = vi.spyOn(api, 'setPaneRecording');
    useStore.setState({ openDialog: (d: { resolve: (v: unknown) => void }) => d.resolve(false) } as never);
    await startPaneRecording(pane);
    expect(set).not.toHaveBeenCalled();
  });
});
