import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import { api } from '../api';
import { ScheduleDialog } from './ScheduleDialog';
import { useEntitlements } from '../entitlements';
import { DialogHost } from './DialogHost';
import type { Health, Schedule } from '../types';


function pane(id: string, cwd: string, kind?: '' | 'native') {
  return {
    id,
    cmd: ['bash'],
    cwd,
    liveCwd: cwd,
    status: 'running',
    title: id,
    kind,
  } as never;
}

function open(initialPaneId?: string, fromPaneMenu = false, onClose: () => void = () => {}) {
  return render(
    <>
      <ScheduleDialog
        open
        onClose={onClose}
        initialPaneId={initialPaneId}
        fromPaneMenu={fromPaneMenu}
      />
      <DialogHost />
    </>,
  );
}

function saved(over: Partial<Schedule>): Schedule {
  return {
    id: 's1',
    name: 'Nightly',
    cron: '0 0 * * *',
    enabled: true,
    createdAt: '2026-11-01T00:00:00Z',
    fireCount: 0,
    skipCount: 0,
    errorCount: 0,
    ...over,
  };
}

function edit(schedule: Schedule, onClose: () => void = () => {}) {
  return render(
    <>
      <ScheduleDialog open onClose={onClose} editing={schedule} />
      <DialogHost />
    </>,
  );
}

async function scratchForm() {
  fireEvent.click(screen.getByText(/Start from scratch/));
  await waitFor(() => expect(screen.getByText('Job')).toBeInTheDocument());
}

describe('ScheduleDialog', () => {
  beforeEach(() => {
    useStore.setState({ panes: { p1: pane('p1', '/home/me/project') } });
    useEntitlements.setState({ loaded: true, team: true, operator: true });
    vi.spyOn(api, 'listScheduleAgents').mockResolvedValue([]);
    vi.spyOn(api, 'previewCron').mockResolvedValue({ fires: [] });
    vi.spyOn(api, 'githubAppStatus').mockResolvedValue({ configured: false, connected: false } as never);
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ claudeChecked: true, claudeInstalled: true } as never);
  });

  // Some tests replace these store actions; put the real ones back after each.
  const { updateSchedule: realUpdateSchedule, toast: realToast } = useStore.getState();

  afterEach(() => {
    useStore.getState().closeDialog(false);
    useStore.setState({ panes: {}, updateSchedule: realUpdateSchedule, toast: realToast });
    vi.restoreAllMocks();
  });

  describe('without Claude Code installed', () => {
    beforeEach(() => {
      vi.spyOn(api, 'healthScoped').mockResolvedValue({ claudeChecked: true, claudeInstalled: false } as Health & { features?: string[] });
    });

    it('defaults a new schedule to a prompt and greys out the Claude task kind', async () => {
      open('p1');
      fireEvent.click(screen.getByText('Start from scratch →'));
      const task = screen.getByLabelText<HTMLInputElement>('Run a Claude task', { exact: false });
      // The health answer disables the task kind in one render; an effect then
      // moves the selection to a prompt in the next. Wait for both, or a slow
      // runner can check in between and still find the task selected.
      await waitFor(() => {
        expect(task).toBeDisabled();
        expect(task).not.toBeChecked();
      });
      expect(screen.getByLabelText<HTMLInputElement>('Prompt into a pane', { exact: false })).toBeChecked();
      expect(screen.getByText('· Needs Claude Code')).toBeInTheDocument();
      expect(screen.getByText('Types into a pane you already have open, such as a Codex pane. If that pane is busy, or you are typing in it, it waits up to an hour.')).toBeInTheDocument();
    });

    it('disables the Claude task recipes but keeps Remind me', async () => {
      open('p1');
      const digest = screen.getByText('Morning digest').closest('button')!;
      await waitFor(() => expect(digest).toBeDisabled());
      expect(digest).toHaveTextContent('Needs Claude Code');
      expect(screen.getByText('Remind me…').closest('button')).not.toBeDisabled();
    });

    it('keeps an existing Claude task editable and says why it cannot run', async () => {
      render(
        <ScheduleDialog
          open
          onClose={() => {}}
          editing={{ id: 's1', name: 'Nightly', kind: 'task', cron: '0 0 * * *', cwd: '/home/me/project', prompt: 'x', permission: 'plan', enabled: true } as never}
        />,
      );
      const task = screen.getByLabelText<HTMLInputElement>('Run a Claude task', { exact: false });
      expect(task).toBeChecked();
      expect(task).not.toBeDisabled();
      expect(await screen.findByText("Claude Code isn't installed on this computer any more, so this task can't run until it is.")).toBeInTheDocument();
    });
  });

  it('asks the selected machine and backend, not the shared health poll', async () => {
    const scoped = vi.spyOn(api, 'healthScoped');
    open('p1');
    await waitFor(() => expect(scoped).toHaveBeenCalledTimes(1));
    expect(scoped.mock.calls[0][0]).toHaveProperty('key');
  });

  it('still defaults to a Claude task while the first Claude check is pending', async () => {
    const scoped = vi.spyOn(api, 'healthScoped').mockResolvedValue({ claudeChecked: false, claudeInstalled: false } as never);
    open('p1');
    fireEvent.click(screen.getByText('Start from scratch →'));
    await waitFor(() => expect(scoped).toHaveBeenCalled());
    const task = screen.getByLabelText<HTMLInputElement>('Run a Claude task', { exact: false });
    expect(task).toBeChecked();
    expect(task).not.toBeDisabled();
  });

  it('opens on the recipe shelf and prefills from a template', async () => {
    // Production's global entry still supplies the focused pane so cwd can be
    // seeded; that must not be mistaken for pane-context-menu intent.
    open('p1');
    expect(screen.getByText('Start with a recipe')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Morning digest'));
    await waitFor(() => expect(screen.getByText('Job')).toBeInTheDocument());
    // Task kind selected, name + prompt + cwd prefilled.
    expect(screen.getByLabelText<HTMLInputElement>('Run a Claude task', { exact: false })).toBeChecked();
    expect(screen.getByDisplayValue('Morning digest')).toBeInTheDocument();
    expect(screen.getByLabelText('Working directory')).toHaveValue('/home/me/project');
    // The digest reads git history only.
    expect(screen.getByLabelText<HTMLInputElement>('Read & plan only', { exact: false })).toBeChecked();
  });

  it('gives the Nightly test run recipe the check preset and says what it means', async () => {
    const create = vi.fn().mockResolvedValue(undefined);
    useStore.setState({ createSchedule: create } as never);
    open('p1');
    fireEvent.click(screen.getByText('Nightly test run'));
    await waitFor(() => expect(screen.getByText('Job')).toBeInTheDocument());
    expect(screen.getByLabelText<HTMLInputElement>('Read & run checks', { exact: false })).toBeChecked();
    expect(screen.getByText("This runs your project's tests and dependency checks, which execute code from the repository, unattended. Pick it only for a project you trust. It doesn't load your Claude settings files, so a Bedrock, Vertex or API-key-helper login set up there isn't used and the run fails.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /^Create/ }));
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(create.mock.calls[0][0]).toMatchObject({ kind: 'task', permission: 'check' });
  });

  it('fills the public Morning digest without the removed todo tools', async () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false });
    open('p1');
    fireEvent.click(screen.getByText('Morning digest'));
    await waitFor(() => expect(screen.getByText('Job')).toBeInTheDocument());
    const prompt = [...document.querySelectorAll('textarea')].map((el) => el.value).join('\n');
    expect(prompt).toContain('Summarize what changed in this repo overnight');
    expect(prompt).not.toContain('list_todos');
  });

  it('opens a pane context-menu schedule directly in prompt mode with that pane selected', async () => {
    open('p1', true);
    expect(screen.queryByText('Start with a recipe')).not.toBeInTheDocument();
    expect(screen.getByLabelText<HTMLInputElement>('Prompt into a pane', { exact: false })).toBeChecked();
    expect(screen.getByRole('combobox', { name: 'Target pane' })).toHaveValue('p1');
  });

  it('labels native target panes as Focus', () => {
    useStore.setState({ panes: { focus: pane('focus', '/home/me/project', 'native') } });
    open('focus', true);
    const target = screen.getByRole('combobox', { name: 'Target pane' });
    expect(screen.getByRole('option', { name: /· Focus/ })).toBeInTheDocument();
    expect(target).toHaveValue('focus');
  });

  // Regression: `panes` re-identifies on every daemon poll. If the reset
  // effect depends on it, a poll mid-typing wipes the form.
  it('does not reset the form when the panes map updates', async () => {
    open();
    fireEvent.click(screen.getByText(/Start from scratch/));
    await waitFor(() => expect(screen.getByText('Job')).toBeInTheDocument());

    const task = screen.getByPlaceholderText(/What should Claude do each run/);
    fireEvent.change(task, { target: { value: 'my careful prompt' } });
    expect(task).toHaveValue('my careful prompt');

    // Simulate a poll: same panes, brand-new object identity.
    useStore.setState({ panes: { p1: pane('p1', '/home/me/project') } });

    await waitFor(() => {
      expect(screen.getByPlaceholderText(/What should Claude do each run/)).toHaveValue('my careful prompt');
    });
    // And we did not get bounced back to the recipe shelf.
    expect(screen.queryByText('Start with a recipe')).not.toBeInTheDocument();
  });

  it('defaults the phone notification ON for one-time runs and OFF for recurring', async () => {
    open();
    fireEvent.click(screen.getByText(/Start from scratch/));
    await waitFor(() => expect(screen.getByText('Job')).toBeInTheDocument());

    const notify = screen.getByLabelText<HTMLInputElement>('Notify my phone when this runs', { exact: false });
    expect(notify).not.toBeChecked(); // recurring default

    const runs = screen.getByRole('combobox', { name: 'Runs' });
    fireEvent.change(runs, { target: { value: '__once__' } });
    await waitFor(() => expect(notify).toBeChecked());

    // Once the user decides, mode changes stop overriding them.
    fireEvent.click(notify);
    expect(notify).not.toBeChecked();
    fireEvent.change(runs, { target: { value: '0 * * * *' } });
    await waitFor(() => expect(screen.getByText('End condition')).toBeInTheDocument());
    expect(notify).not.toBeChecked();
  });

  it('keeps the notification without phone or Aria wording on a public install', async () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false });
    open();
    fireEvent.click(screen.getByText(/Start from scratch/));
    await waitFor(() => expect(screen.getByText('Job')).toBeInTheDocument());
    expect(screen.getByLabelText('Notify me when this runs', { exact: false })).toBeInTheDocument();
    expect(screen.queryByText(/phone/i)).toBeNull();
    expect(screen.queryByText(/Aria chat/)).toBeNull();
  });

  it('typed natural language drives the picker', async () => {
    open();
    fireEvent.click(screen.getByText(/Start from scratch/));
    await waitFor(() => expect(screen.getByText('Job')).toBeInTheDocument());

    fireEvent.change(screen.getByPlaceholderText(/every weekday at 9am/), {
      target: { value: 'every weekday at 9am' },
    });
    await waitFor(() => {
      expect(screen.getByRole('combobox', { name: 'Runs' })).toHaveValue('0 9 * * 1-5');
    });
  });

  // Regression: the apply-once key used the raw text, so appending a space
  // re-applied the parse and clobbered a picker change made in between.
  it('cosmetic edits to the NL box do not clobber a manual picker change', async () => {
    open();
    fireEvent.click(screen.getByText(/Start from scratch/));
    await waitFor(() => expect(screen.getByText('Job')).toBeInTheDocument());

    const nl = screen.getByPlaceholderText(/every weekday at 9am/);
    const runs = screen.getByRole('combobox', { name: 'Runs' });

    fireEvent.change(nl, { target: { value: 'in 30 min' } });
    await waitFor(() => expect(runs).toHaveValue('__once__'));

    // User changes their mind via the picker...
    fireEvent.change(runs, { target: { value: '0 * * * *' } });
    expect(runs).toHaveValue('0 * * * *');

    // ...then types a trailing space in the NL box. Same meaning → no re-apply.
    fireEvent.change(nl, { target: { value: 'in 30 min ' } });
    await waitFor(() => expect(runs).toHaveValue('0 * * * *'));
  });

  // The host passes the pane focused at open. A different pane arriving
  // later (focus moved in the background) must not re-seed the form.
  it('does not reset the form when the seed pane changes while it is open', async () => {
    useStore.setState({ panes: { p1: pane('p1', '/home/me/project'), p2: pane('p2', '/home/me/other') } });
    const { rerender } = render(<ScheduleDialog open onClose={() => {}} initialPaneId="p1" />);
    await scratchForm();
    fireEvent.change(screen.getByPlaceholderText(/What should Claude do each run/), { target: { value: 'my careful prompt' } });

    rerender(<ScheduleDialog open onClose={() => {}} initialPaneId="p2" />);

    expect(screen.queryByText('Start with a recipe')).not.toBeInTheDocument();
    expect(screen.getByPlaceholderText(/What should Claude do each run/)).toHaveValue('my careful prompt');
    expect(screen.getByLabelText('Working directory')).toHaveValue('/home/me/project');
  });

  it('starts a from-scratch automation daily, not hourly', async () => {
    open('p1');
    await scratchForm();
    expect(screen.getByRole('combobox', { name: 'Runs' })).toHaveValue('0 9 * * *');
  });

  describe('closing', () => {
    it('is a labelled modal dialog and takes focus when it opens', () => {
      open('p1');
      const dialog = screen.getByRole('dialog', { name: 'New automation' });
      expect(dialog).toHaveAttribute('aria-modal', 'true');
      expect(dialog.contains(document.activeElement)).toBe(true);
    });

    it('Escape closes an untouched form without asking', async () => {
      const onClose = vi.fn();
      open('p1', false, onClose);
      await scratchForm();
      fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' });
      await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
      expect(screen.queryByText('Discard this automation?')).not.toBeInTheDocument();
    });

    it('a recipe that was only picked closes without asking', async () => {
      const onClose = vi.fn();
      open('p1', false, onClose);
      fireEvent.click(screen.getByText('Morning digest'));
      await waitFor(() => expect(screen.getByText('Job')).toBeInTheDocument());
      fireEvent.click(screen.getByText('Cancel'));
      await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    });

    it('asks before a click outside discards a changed form, and keeps it on "Keep editing"', async () => {
      const onClose = vi.fn();
      open('p1', false, onClose);
      await scratchForm();
      fireEvent.change(screen.getByPlaceholderText(/What should Claude do each run/), { target: { value: 'my careful prompt' } });

      fireEvent.mouseDown(document.querySelector('.dialog-backdrop')!);
      expect(await screen.findByText('Discard this automation?')).toBeInTheDocument();
      expect(onClose).not.toHaveBeenCalled();

      fireEvent.click(screen.getByText('Keep editing'));
      await waitFor(() => expect(screen.queryByText('Discard this automation?')).not.toBeInTheDocument());
      expect(onClose).not.toHaveBeenCalled();
      expect(screen.getByPlaceholderText(/What should Claude do each run/)).toHaveValue('my careful prompt');
    });

    it.each([
      ['Escape', () => fireEvent.keyDown(document.body, { key: 'Escape' })],
      ['the × button', () => fireEvent.click(screen.getByLabelText('Close'))],
      ['Cancel', () => fireEvent.click(screen.getByText('Cancel'))],
    ])('asks on %s too, and closes on Discard', async (_name, leave) => {
      const onClose = vi.fn();
      open('p1', false, onClose);
      await scratchForm();
      fireEvent.change(screen.getByPlaceholderText(/What should Claude do each run/), { target: { value: 'my careful prompt' } });

      leave();
      expect(await screen.findByText('Discard this automation?')).toBeInTheDocument();
      expect(onClose).not.toHaveBeenCalled();
      fireEvent.click(screen.getByText('Discard'));
      await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    });

    it('Escape on the discard question keeps the form open', async () => {
      const onClose = vi.fn();
      open('p1', false, onClose);
      await scratchForm();
      fireEvent.change(screen.getByPlaceholderText(/What should Claude do each run/), { target: { value: 'my careful prompt' } });
      fireEvent.click(screen.getByText('Cancel'));
      const question = await screen.findByText('Discard this automation?');

      fireEvent.keyDown(question, { key: 'Escape' });
      await waitFor(() => expect(screen.queryByText('Discard this automation?')).not.toBeInTheDocument());
      expect(onClose).not.toHaveBeenCalled();
      expect(screen.getByRole('dialog', { name: 'New automation' })).toBeInTheDocument();
    });

    // The question takes focus a frame after it appears. An Escape pressed
    // before that lands outside it, and must still answer it.
    it('Escape answers the discard question even before the question has focus', async () => {
      const onClose = vi.fn();
      open('p1', false, onClose);
      await scratchForm();
      const task = screen.getByPlaceholderText(/What should Claude do each run/);
      fireEvent.change(task, { target: { value: 'my careful prompt' } });
      fireEvent.keyDown(task, { key: 'Escape' });
      await screen.findByText('Discard this automation?');

      fireEvent.keyDown(task, { key: 'Escape' });
      await waitFor(() => expect(screen.queryByText('Discard this automation?')).not.toBeInTheDocument());
      expect(onClose).not.toHaveBeenCalled();
      expect(task).toHaveValue('my careful prompt');

      // And the form still asks the next time.
      fireEvent.keyDown(task, { key: 'Escape' });
      expect(await screen.findByText('Discard this automation?')).toBeInTheDocument();
    });

    // If the save fails, what was typed must still be there to try again.
    it('stays open while a save is on its way, whatever is pressed', async () => {
      let fail: (e: Error) => void = () => {};
      const updateSchedule = vi.fn().mockReturnValue(new Promise((_resolve, reject) => { fail = reject; }));
      const onClose = vi.fn();
      useStore.setState({ updateSchedule, toast: vi.fn() } as never);
      edit(saved({ kind: 'task', cwd: '/home/me/project', prompt: 'x', permission: 'plan' }), onClose);
      fireEvent.change(screen.getByDisplayValue('Nightly'), { target: { value: 'Nightly tests' } });
      fireEvent.click(screen.getByText('Save'));
      await screen.findByText('Saving…');

      fireEvent.keyDown(document.body, { key: 'Escape' });
      fireEvent.mouseDown(document.querySelector('.dialog-backdrop')!);
      fireEvent.click(screen.getByLabelText('Close'));
      expect(onClose).not.toHaveBeenCalled();
      expect(screen.queryByText('Discard your changes?')).not.toBeInTheDocument();

      await act(async () => { fail(new Error('daemon unreachable')); });
      await screen.findByText('Save');
      expect(onClose).not.toHaveBeenCalled();
      expect(screen.getByDisplayValue('Nightly tests')).toBeInTheDocument();
    });

    it('names the automation when an edit is about to be discarded', async () => {
      edit(saved({ kind: 'task', cwd: '/home/me/project', prompt: 'x', permission: 'plan' }));
      fireEvent.change(screen.getByDisplayValue('Nightly'), { target: { value: 'Nightly tests' } });
      fireEvent.click(screen.getByText('Cancel'));
      expect(await screen.findByText('Discard your changes?')).toBeInTheDocument();
      expect(screen.getByText('Your changes to "Nightly" have not been saved.')).toBeInTheDocument();
    });
  });

  describe('editing', () => {
    it('keeps the model a task was created with', async () => {
      const updateSchedule = vi.fn().mockResolvedValue({});
      useStore.setState({ updateSchedule } as never);
      edit(saved({ kind: 'task', cwd: '/home/me/project', prompt: 'run the tests', permission: 'plan', model: 'claude-test-model' }));

      fireEvent.change(screen.getByDisplayValue('Nightly'), { target: { value: 'Nightly tests' } });
      fireEvent.click(screen.getByText('Save'));

      await waitFor(() => expect(updateSchedule).toHaveBeenCalledTimes(1));
      expect(updateSchedule.mock.calls[0][0]).toBe('s1');
      expect(updateSchedule.mock.calls[0][1]).toMatchObject({ kind: 'task', name: 'Nightly tests', model: 'claude-test-model' });
    });

    it('sends no model for a task that never had one', async () => {
      const updateSchedule = vi.fn().mockResolvedValue({});
      useStore.setState({ updateSchedule } as never);
      edit(saved({ kind: 'task', cwd: '/home/me/project', prompt: 'run the tests', permission: 'plan' }));
      fireEvent.click(screen.getByText('Save'));
      await waitFor(() => expect(updateSchedule).toHaveBeenCalledTimes(1));
      expect(updateSchedule.mock.calls[0][1].model).toBeUndefined();
    });

    it('shows a closed pane as such and blocks Save until another pane is picked', async () => {
      const updateSchedule = vi.fn().mockResolvedValue({});
      useStore.setState({ updateSchedule } as never);
      edit(saved({ kind: 'prompt', paneId: 'gone', prompt: 'summarize' }));

      const target = screen.getByRole('combobox', { name: 'Target pane' });
      expect(target).toHaveValue('gone');
      expect(screen.getByRole('option', { name: '(closed pane)' })).toBeInTheDocument();
      expect(screen.getByRole('alert')).toHaveTextContent('The pane this automation types into is closed. Pick another pane to save it.');
      expect(screen.getByText('Save')).toBeDisabled();

      fireEvent.change(target, { target: { value: 'p1' } });
      expect(screen.queryByRole('option', { name: '(closed pane)' })).not.toBeInTheDocument();
      expect(screen.getByText('Save')).not.toBeDisabled();
      fireEvent.click(screen.getByText('Save'));
      await waitFor(() => expect(updateSchedule).toHaveBeenCalledTimes(1));
      expect(updateSchedule.mock.calls[0][1]).toMatchObject({ paneId: 'p1', prompt: 'summarize' });
    });

    it('blocks Save when the pane closes while the form is open', async () => {
      edit(saved({ kind: 'prompt', paneId: 'p1', prompt: 'summarize' }));
      expect(screen.getByText('Save')).not.toBeDisabled();
      act(() => { useStore.setState({ panes: {} }); });
      await waitFor(() => expect(screen.getByText('Save')).toBeDisabled());
      expect(screen.getByRole('option', { name: '(closed pane)' })).toBeInTheDocument();
    });
  });

  it('leaves the pane unpicked when the pane it was opened from is not running', () => {
    useStore.setState({ panes: { dead: { ...(pane('dead', '/home/me/project') as object), status: 'exited' } as never } });
    open('dead', true);
    expect(screen.getByRole('combobox', { name: 'Target pane' })).toHaveValue('');
    expect(screen.queryByRole('option', { name: '(closed pane)' })).not.toBeInTheDocument();
  });
});
