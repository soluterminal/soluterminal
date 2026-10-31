import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import { api } from '../api';
import { ScheduleDialog } from './ScheduleDialog';
import { useEntitlements } from '../entitlements';
import type { Health } from '../types';


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

function open(initialPaneId?: string, fromPaneMenu = false) {
  return render(
    <ScheduleDialog
      open
      onClose={() => {}}
      initialPaneId={initialPaneId}
      fromPaneMenu={fromPaneMenu}
    />,
  );
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

  afterEach(() => {
    useStore.setState({ panes: {} });
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
      await waitFor(() => expect(task).toBeDisabled());
      expect(task).not.toBeChecked();
      expect(screen.getByLabelText<HTMLInputElement>('Prompt into a pane', { exact: false })).toBeChecked();
      expect(screen.getByText('· Needs Claude Code')).toBeInTheDocument();
      expect(screen.getByText('Types into a pane you already have open, such as a Codex pane. Skipped if that pane is busy.')).toBeInTheDocument();
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
    // Shipped recipes never write unattended.
    expect(screen.getByLabelText<HTMLInputElement>('Read & plan only', { exact: false })).toBeChecked();
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
});
