import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { DialogHost } from './DialogHost';
import { recordingDialog } from '../lib/dialog';
import { recordingChoiceOf, type RecordingDialogSpec } from './RecordingDialogBody';

// ST-317: the start dialog names each default being overridden in plain words,
// per pane; a hard rule always skips; one confirm; nothing is remembered.

afterEach(cleanup);

const spec = (panes: RecordingDialogSpec['panes'], workspace = false): RecordingDialogSpec => ({
  panes, hours: [1, 2, 4, 8, 12, 24], defaultHours: 8, deleteAfterDays: 7, workspace,
});

describe('recordingChoiceOf', () => {
  it('records a pane only when every default on it is overridden', () => {
    const s = spec([
      { id: 'a', name: 'A', defaults: [] },
      { id: 'b', name: 'B', defaults: ['shell', 'handle'] },
      { id: 'c', name: 'C', defaults: [], skip: 'never recorded' },
    ], true);
    expect(recordingChoiceOf(s, 8, {})).toEqual({ hours: 8, panes: [{ id: 'a', override: [] }] });
    expect(recordingChoiceOf(s, 4, { b: ['shell'] })).toEqual({ hours: 4, panes: [{ id: 'a', override: [] }] });
    expect(recordingChoiceOf(s, 4, { b: ['shell', 'handle'] })?.panes).toEqual([{ id: 'a', override: [] }, { id: 'b', override: ['shell', 'handle'] }]);
    expect(recordingChoiceOf(spec([{ id: 'c', name: 'C', defaults: [], skip: 'x' }]), 8, {})).toBeNull();
  });
});

describe('the recording dialog', () => {
  it('needs each default ticked before one pane can start, then resolves the choice', async () => {
    render(<DialogHost />);
    let result: unknown = 'pending';
    act(() => {
      void recordingDialog({
        title: 'Record this pane?', message: 'Saved on this computer only, deleted after 7 days, never sent to the cloud.', confirmLabel: 'Start recording',
        recording: spec([{ id: 'p', name: '@ops shell', defaults: ['handle', 'shell'] }]),
      }).then((r) => { result = r; });
    });
    const startBtn = screen.getByRole('button', { name: 'Start recording' });
    expect(startBtn).toBeDisabled();
    // The shell warning, verbatim, and the handle default, named.
    expect(screen.getByText(/Shells print secrets \(tokens, env, cat\)\. Switch recording off before a login or sudo\./)).toBeInTheDocument();
    expect(screen.getByText(/handle is on the do-not-record list/)).toBeInTheDocument();
    const boxes = screen.getAllByRole('checkbox');
    fireEvent.click(boxes[0]);
    expect(startBtn).toBeDisabled();
    fireEvent.click(boxes[1]);
    expect(startBtn).toBeEnabled();
    fireEvent.change(screen.getByLabelText('Time limit'), { target: { value: '24' } });
    // No unlimited choice.
    expect([...screen.getByLabelText<HTMLSelectElement>('Time limit').options].map((o) => o.value)).toEqual(['1', '2', '4', '8', '12', '24']);
    await act(async () => { fireEvent.click(startBtn); });
    expect(result).toEqual({ hours: 24, panes: [{ id: 'p', override: ['handle', 'shell'] }] });
  });

  it('lists skipped panes with the reason in the workspace confirm', async () => {
    render(<DialogHost />);
    let result: unknown = 'pending';
    act(() => {
      void recordingDialog({
        title: 'Record every pane in this workspace?', message: 'm', confirmLabel: 'Start recording',
        recording: spec([
          { id: 'a', name: 'Agent', defaults: [] },
          { id: 's', name: 'Shell', defaults: ['shell'] },
          { id: 'k', name: 'Keys', defaults: [], skip: 'This pane works in a folder that is never recorded' },
        ], true),
      }).then((r) => { result = r; });
    });
    expect(screen.getByText(/Skipped: This pane works in a folder that is never recorded/)).toBeInTheDocument();
    expect(screen.getAllByText(/— skipped/)).toHaveLength(2);
    expect(screen.getByText(/— will be recorded/)).toBeInTheDocument();
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Start recording' })); });
    expect(result).toEqual({ hours: 8, panes: [{ id: 'a', override: [] }] });
  });

  it('never starts from Enter on the time limit or a box', async () => {
    render(<DialogHost />);
    let result: unknown = 'pending';
    act(() => {
      void recordingDialog({ title: 't', message: 'm', confirmLabel: 'Start recording', recording: spec([{ id: 'a', name: 'A', defaults: [] }]) })
        .then((r) => { result = r; });
    });
    const select = screen.getByLabelText('Time limit');
    await act(async () => { fireEvent.keyDown(select, { key: 'Enter' }); });
    expect(result).toBe('pending');
    expect(screen.getByRole('button', { name: 'Start recording' })).toBeEnabled();
  });

  it('names each folder root on its own', async () => {
    render(<DialogHost />);
    act(() => {
      void recordingDialog({ title: 't', message: 'm', confirmLabel: 'Start recording',
        recording: spec([{ id: 'p', name: 'P', defaults: ['folder:config', 'folder:launch_deploy'] }]) });
    });
    expect(screen.getByText(/It works in ~\/\.config, where other tools keep their tokens/)).toBeInTheDocument();
    expect(screen.getByText(/It works in launch\/deploy/)).toBeInTheDocument();
    expect(screen.getAllByRole('checkbox')).toHaveLength(2);
  });

  it('opens with Cancel focused, so an immediate Enter starts nothing (one pane and a workspace)', async () => {
    for (const workspace of [false, true]) {
      render(<DialogHost />);
      let result: unknown = 'pending';
      act(() => {
        void recordingDialog({ title: 't', message: 'm', confirmLabel: 'Start recording',
          recording: spec([{ id: 'a', name: 'A', defaults: [] }, { id: 'b', name: 'B', defaults: [] }], workspace) })
          .then((r) => { result = r; });
      });
      await act(async () => { await new Promise((r) => requestAnimationFrame(() => r(null))); });
      const cancel = screen.getByRole('button', { name: 'Cancel' });
      expect(document.activeElement).toBe(cancel);
      await act(async () => { fireEvent.keyDown(document.activeElement!, { key: 'Enter' }); });
      expect(result === 'pending' || result === null).toBe(true);
      cleanup();
    }
  });

  it('resolves null on cancel', async () => {
    render(<DialogHost />);
    let result: unknown = 'pending';
    act(() => {
      void recordingDialog({ title: 't', message: 'm', confirmLabel: 'Start recording', recording: spec([{ id: 'a', name: 'A', defaults: [] }]) })
        .then((r) => { result = r; });
    });
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Cancel' })); });
    expect(result).toBeNull();
  });
});
