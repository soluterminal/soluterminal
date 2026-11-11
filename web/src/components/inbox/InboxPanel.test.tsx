import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest';
import { useEntitlements } from '../../entitlements';
import { useStore } from '../../store';
import { api, ApiError } from '../../api';
import { openFeedback } from '../../lib/feedbackReport';
import { InboxHost, openInbox, openInboxAt } from './InboxPanel';
import { __resetFeedbackInboxForTests } from '../../lib/useFeedbackInbox';
import { __resetMyReportsForTests } from '../../lib/myReports';
import { setExtensionsForTests } from '../../extensions';
import type { Schedule } from '../../types';
import type { AgentReport, FeedbackEvent } from '../../api';

// LOCAL yesterday at noon — "now - 26h" is timezone/time-of-day flaky (at
// 00:31 UTC it lands two calendar days back and the "Yesterday" group never
// renders; that exact flake failed CI on 2026-07-11).
function yesterdayNoon(): string {
  const d = new Date();
  d.setDate(d.getDate() - 1);
  d.setHours(12, 0, 0, 0);
  return d.toISOString();
}

function sched(over: Partial<Schedule> = {}): Schedule {
  return {
    id: 's1',
    name: 'Morning digest',
    kind: 'task',
    enabled: true,
    createdAt: '2026-07-01T00:00:00Z',
    fireCount: 1,
    skipCount: 0,
    errorCount: 0,
    runs: [
      { at: new Date(Date.now() - 60_000).toISOString(), status: 'fired', summary: '## Needs you\n- approve PR #7', logFile: 'run-0.md' },
      { at: yesterdayNoon(), status: 'fired', summary: 'Nothing needs you today.' },
    ],
    ...over,
  } as Schedule;
}

function fbEvent(over: Partial<FeedbackEvent> = {}): FeedbackEvent {
  return {
    number: 42,
    title: 'Paste fails on Windows',
    event: 'needs-info',
    message: 'Needs more info — which browser?',
    at: new Date(Date.now() - 1800_000).toISOString(),
    ...over,
  };
}

beforeEach(() => useEntitlements.setState({ loaded: true, team: true, operator: true }));

afterEach(() => {
  useEntitlements.setState({ loaded: false, team: false, operator: false });
  useStore.setState({ schedules: [] });
  __resetFeedbackInboxForTests([]);
  __resetMyReportsForTests([]);
  vi.restoreAllMocks();
});

function openHost() {
  render(<InboxHost />);
  act(() => openInbox());
}

describe('InboxHost / feed', () => {
  // Wording item 3.
  it('says what will show up when the Inbox is empty', () => {
    openHost();
    expect(screen.getByText('Nothing here yet')).toBeInTheDocument();
    expect(screen.getByText(
      'Reports from automations that run an agent show up here, and so do updates on feedback you send. Use New above to send feedback.',
    )).toBeInTheDocument();
  });

  // ST-162: My reports, with each report's live status in its row and thread.
  it('My reports: a status word per row and at the top of the thread; a quiet close reads Closed', async () => {
    const mine = vi.spyOn(api, 'feedbackMine').mockResolvedValue({ reports: [
      { number: 42, title: 'Paste fails on Windows', filedAt: '2026-11-01T10:00:00Z', status: 'needs-info', message: 'which browser?' },
      { number: 7, title: 'Old report, closed quietly', filedAt: '2026-10-01T10:00:00Z', status: 'closed', message: 'Closed' },
    ] });
    vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    vi.spyOn(api, 'feedbackThread').mockResolvedValue({ number: 7, title: 'Old report, closed quietly', filedAt: '2026-10-01T10:00:00Z', events: [], state: 'closed' });
    // A second kind of item, so the filter tabs show.
    useStore.setState({ schedules: [sched()] });
    __resetFeedbackInboxForTests([fbEvent()]);
    openHost();
    await waitFor(() => expect(mine).toHaveBeenCalledTimes(1));
    fireEvent.click(await screen.findByRole('tab', { name: /My reports 2/ }));
    const quiet = (await screen.findByText('Old report, closed quietly')).closest('button')!;
    expect(within(quiet).getByText('Closed')).toBeInTheDocument();
    expect(quiet).not.toHaveClass('is-unread');
    // Unread and needs-info, so pinned: its status word still shows there.
    const pinned = screen.getByText('Paste fails on Windows').closest('button')!;
    expect(pinned.closest('.inbox-pinned')).not.toBeNull();
    expect(within(pinned).getByText('Needs info')).toHaveClass('inbox-badge-chip');

    fireEvent.click(quiet);
    const head = (await screen.findByText(/Report #7 · filed/)).closest('.inbox-thread-head') as HTMLElement;
    expect(within(head).getByText('Closed')).toBeInTheDocument();
  });

  it('opens on the event, pins needs-you, groups the rest by day, Esc closes', () => {
    useStore.setState({ schedules: [sched(), sched({
      id: 's2', name: 'Nightly tests',
      runs: [{ at: yesterdayNoon(), status: 'fired', summary: 'Nothing new needs you.' }],
    })] });
    __resetFeedbackInboxForTests([fbEvent()]);
    openHost();

    expect(screen.getByRole('dialog', { name: 'Inbox' })).toBeInTheDocument();
    // Both unread needs-you items pinned under "Needs you"; the quiet older
    // report files under Yesterday.
    expect(screen.getByText('Needs you')).toBeInTheDocument();
    expect(screen.getByText('Yesterday')).toBeInTheDocument();
    expect(screen.getByText(/approve PR #7/)).toBeInTheDocument();
    expect(screen.getByText(/which browser/)).toBeInTheDocument();
    // Nothing selected yet — the reading pane shows the hint, not a report.
    expect(screen.getByText('Nothing open')).toBeInTheDocument();

    fireEvent.keyDown(window, { key: 'Escape' });
    expect(screen.queryByRole('dialog', { name: 'Inbox' })).toBeNull();
  });

  it('legacy solu:open-reports-feed still opens the feed', () => {
    useStore.setState({ schedules: [sched()] });
    render(<InboxHost />);
    act(() => {
      window.dispatchEvent(new CustomEvent('solu:open-reports-feed'));
    });
    expect(screen.getByRole('dialog', { name: 'Inbox' })).toBeInTheDocument();
  });

  it('filters to one source when both are present', () => {
    useStore.setState({ schedules: [sched()] });
    __resetFeedbackInboxForTests([fbEvent()]);
    openHost();
    fireEvent.click(screen.getByRole('tab', { name: /My reports 1/ }));
    expect(screen.queryByText(/approve PR #7/)).toBeNull();
    expect(screen.getByText(/which browser/)).toBeInTheDocument();
  });

  it('mark all read stamps reports and feedback', async () => {
    const schedRead = vi.spyOn(api, 'markScheduleRead').mockResolvedValue(undefined as never);
    const fbRead = vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    const s = sched();
    useStore.setState({ schedules: [s] });
    __resetFeedbackInboxForTests([fbEvent()]);
    openHost();

    fireEvent.click(screen.getByText('Mark all read'));
    await waitFor(() => expect(schedRead).toHaveBeenCalledWith('s1', s.runs![0].at));
    expect(fbRead).toHaveBeenCalledWith(42);
  });

  it('files one row per automation with "+N earlier", and needs you counts once', () => {
    const now = Date.now();
    const runs = Array.from({ length: 5 }, (_, i) => ({
      at: new Date(now - (i + 1) * 86_400_000).toISOString(),
      status: 'fired' as const,
      summary: `## Needs you\n- ask number ${i}`,
    }));
    useStore.setState({ schedules: [sched({ runs })] });
    openHost();
    // Only the newest report is on the face of the row; older ones fold.
    expect(screen.getByText(/ask number 0/)).toBeInTheDocument();
    expect(screen.queryByText(/ask number 1/)).toBeNull();
    expect(screen.getByText('+4 earlier')).toBeInTheDocument();
    expect(screen.getAllByText('needs you')).toHaveLength(1);
    expect(screen.getByTitle('Mark all 1 read')).toBeInTheDocument();
  });

  it('counts past the render cap read "50+", not a silent 50', () => {
    const now = Date.now();
    const schedules = Array.from({ length: 53 }, (_, i) => sched({
      id: `s${i}`, name: `Task ${i}`,
      runs: [{ at: new Date(now - i * 60_000).toISOString(), status: 'fired', summary: 'Nothing new needs you.' }],
    }));
    useStore.setState({ schedules });
    openHost();
    expect(screen.getByTitle('Mark all 50+ read')).toBeInTheDocument();
    // The window still renders only the newest 50 rows.
    expect(screen.getByText('Task 0')).toBeInTheDocument();
    expect(screen.queryByText('Task 52')).toBeNull();
  });

  // A built-in agent alert carries its own markdown: it opens without a run
  // log, and its links point at the held issues.
  it('a PR-reviewer or held-fixes alert opens its own text in the reading pane', async () => {
    const runLog = vi.spyOn(api, 'agentRunLog');
    const alert: AgentReport = {
      agentId: 'builtin:fix-agent', agentName: 'Fix agent', runId: 'held-abc',
      at: new Date(Date.now() - 60_000).toISOString(),
      title: 'Fix agent is holding 2 feedback fixes for you',
      preview: '#29, #36: it won\'t open a PR for these without a person.',
      items: [], kind: 'builtin',
      body: '## Fix agent is holding 2 feedback fixes for you\n\n- [#29](https://github.com/o/fb/issues/29): its reviewer held the change\n',
    };
    const acknowledge = vi.fn(async () => {});
    setExtensionsForTests([{ inboxReports: { useReports: () => [alert], acknowledge, acknowledgeAll: async () => {} } }]);
    try {
      openHost();
      expect(screen.getByText('Needs you')).toBeInTheDocument();
      fireEvent.click(screen.getByText('Fix agent'));
      const link = await screen.findByRole('link', { name: '#29' });
      expect(link.getAttribute('href')).toBe('https://github.com/o/fb/issues/29');
      expect(runLog).not.toHaveBeenCalled();
    } finally {
      setExtensionsForTests(null);
    }
  });

  it('report row opens the report in the reading pane and viewing marks it read', async () => {
    const logSpy = vi.spyOn(api, 'scheduleRunLog').mockResolvedValue('Full report body text here.');
    const readSpy = vi.spyOn(api, 'markScheduleRead').mockResolvedValue(undefined as never);
    const s = sched();
    useStore.setState({ schedules: [s] });
    openHost();

    fireEvent.click(screen.getByText(/approve PR #7/));
    // The reader mounts in the right pane — no dialog stacked on the window.
    expect(screen.getByText(/Report · Morning digest/)).toBeInTheDocument();
    await waitFor(() => expect(logSpy).toHaveBeenCalledWith('s1', 'run-0.md'));
    expect(await screen.findByText(/Full report body text here/)).toBeInTheDocument();
    // The feed stays visible next to it (two-pane), and viewing stamps read.
    expect(screen.getByText('Needs you')).toBeInTheDocument();
    await waitFor(() => expect(readSpy).toHaveBeenCalledWith('s1', s.runs![0].at));
  });

  it('↓ and j walk the visual order: pinned report first, then the feedback thread', async () => {
    vi.spyOn(api, 'scheduleRunLog').mockResolvedValue('Full report body text here.');
    vi.spyOn(api, 'markScheduleRead').mockResolvedValue(undefined as never);
    const fbRead = vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    vi.spyOn(api, 'feedbackThread').mockResolvedValue({
      number: 42,
      title: 'Paste fails on Windows',
      filedAt: '2026-07-09T10:00:00Z',
      events: [
        { event: 'filed', message: 'Report filed', at: '2026-07-09T10:00:00Z' },
        { event: 'needs-info', message: 'Needs more info — which browser?', at: '2026-07-10T09:00:00Z' },
      ],
    });
    useStore.setState({ schedules: [sched()] });
    __resetFeedbackInboxForTests([fbEvent()]);
    openHost();

    fireEvent.keyDown(window, { key: 'ArrowDown' });
    expect(await screen.findByText(/Full report body text here/)).toBeInTheDocument();

    fireEvent.keyDown(window, { key: 'j' });
    expect(await screen.findByText('Needs your info', { exact: false })).toBeInTheDocument();
    // Selecting the feedback row marked it read.
    expect(fbRead).toHaveBeenCalledWith(42);
  });
});

describe('InboxHost / notification targets', () => {
  it('a schedule target opens that automation\'s newest report', async () => {
    vi.spyOn(api, 'scheduleRunLog').mockResolvedValue('Digest body from the tap.');
    vi.spyOn(api, 'markScheduleRead').mockResolvedValue(undefined as never);
    useStore.setState({ schedules: [sched(), sched({ id: 's2', name: 'Nightly tests' })] });
    render(<InboxHost />);
    act(() => openInboxAt({ schedule: 's1' }));
    expect(screen.getByText(/Report · Morning digest/)).toBeInTheDocument();
    expect(await screen.findByText(/Digest body from the tap/)).toBeInTheDocument();
  });

  it('a target that is not loaded yet is selected as soon as it arrives', async () => {
    vi.spyOn(api, 'scheduleRunLog').mockResolvedValue('Late body.');
    vi.spyOn(api, 'markScheduleRead').mockResolvedValue(undefined as never);
    render(<InboxHost />);
    act(() => openInboxAt({ schedule: 's1' }));
    expect(screen.getByText('Nothing open')).toBeInTheDocument();
    act(() => useStore.setState({ schedules: [sched()] }));
    expect(await screen.findByText(/Late body/)).toBeInTheDocument();
  });

  it('an agent target opens and acknowledges that agent\'s pending decision', async () => {
    const report: AgentReport = {
      agentId: 'agt_1', agentName: 'Repo Medic', runId: 'run_9',
      at: new Date().toISOString(), title: 'Needs you — approve', preview: 'approve the fix', logFile: 'r9.md',
    };
    const acknowledge = vi.fn().mockResolvedValue(undefined);
    setExtensionsForTests([{ inboxReports: { useReports: () => [report], acknowledge, acknowledgeAll: vi.fn() } }]);
    vi.spyOn(api, 'agentRunLog').mockResolvedValue('# Medic\n\nApprove the fix please.');
    try {
      render(<InboxHost />);
      act(() => openInboxAt({ agent: 'agt_1' }));
      expect(await screen.findByText(/Approve the fix please/)).toBeInTheDocument();
      expect(acknowledge).toHaveBeenCalledWith(report);
    } finally {
      setExtensionsForTests(null);
    }
  });
});

describe('InboxHost / thread', () => {
  it('feedback row opens the conversation: timeline renders, reply sends and appends', async () => {
    vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    const threadSpy = vi.spyOn(api, 'feedbackThread').mockResolvedValue({
      number: 42,
      state: 'open',
      title: 'Paste fails on Windows',
      filedAt: '2026-07-09T10:00:00Z',
      events: [
        { event: 'filed', message: 'Report filed', at: '2026-07-09T10:00:00Z' },
        { event: 'needs-info', message: 'Needs more info — which browser?', at: '2026-07-10T09:00:00Z' },
      ],
    });
    const replySpy = vi.spyOn(api, 'feedbackReply').mockResolvedValue(undefined as never);
    __resetFeedbackInboxForTests([fbEvent()]);
    openHost();

    fireEvent.click(screen.getAllByText(/which browser/)[0]);
    await waitFor(() => expect(threadSpy).toHaveBeenCalledWith(42));
    expect(screen.getByText('Report #42 · filed', { exact: false })).toBeInTheDocument();
    expect(screen.getByText('Needs your info', { exact: false })).toBeInTheDocument();
    // Two-pane: the feed stays visible beside the conversation — the row
    // (now read, so re-filed under a day bucket) and the thread title both
    // render the report's name.
    expect(screen.getAllByText('Paste fails on Windows').length).toBe(2);

    fireEvent.change(screen.getByPlaceholderText(/Add details/), { target: { value: 'Chrome 126, Ctrl+Shift+V works' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send reply' }));
    await waitFor(() => expect(replySpy).toHaveBeenCalledWith(42, 'Chrome 126, Ctrl+Shift+V works'));
    // Optimistic append shows the reply bubble.
    expect(await screen.findByText(/Ctrl\+Shift\+V works/)).toBeInTheDocument();
    expect(screen.getByText('You replied', { exact: false })).toBeInTheDocument();
    expect(useStore.getState().toasts.at(-1)?.message).toBe('Reply added to report #42.');
    expect(screen.getByPlaceholderText('Add details to this report: what you tried, what happened, your version.')).toBeInTheDocument();
  });

  it.each(['fixed', 'cant-reproduce', 'not-planned', 'answered', 'closed'] as const)('a closed %s thread offers a prefilled follow-up, never a reply', async (event) => {
    vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    vi.spyOn(api, 'feedbackThread').mockResolvedValue({
      number: 38,
      state: 'closed',
      title: 'Dark mode flashes white',
      filedAt: '2026-07-05T10:00:00Z',
      events: [{ event, message: 'Closed report history', at: '2026-07-10T09:00:00Z' }],
    });
    const reply = vi.spyOn(api, 'feedbackReply');
    openHost();
    act(() => openInbox('feed', 38));
    const followUp = await screen.findByRole('button', { name: 'Send a follow-up' });
    expect(screen.queryByPlaceholderText(/Add details/)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Send reply' })).not.toBeInTheDocument();
    fireEvent.click(followUp);
    expect(await screen.findByText('This is a follow-up to report #38.')).toBeInTheDocument();
    expect(((await screen.findByLabelText('What happened?')) as HTMLTextAreaElement).value).toBe('');
    expect(reply).not.toHaveBeenCalled();
  });

  it('says so and offers Try again, never a reply, when the server cannot establish live state', async () => {
    const thread = { number: 42, title: 'Unknown state', filedAt: '2026-07-09T10:00:00Z', events: [] };
    const load = vi.spyOn(api, 'feedbackThread')
      .mockResolvedValueOnce({ ...thread, state: 'unknown' })
      .mockResolvedValue({ ...thread, state: 'open' });
    openHost();
    act(() => openInbox('feed', 42));
    await screen.findByText('Unknown state');
    expect(screen.queryByRole('button', { name: 'Send reply' })).not.toBeInTheDocument();
    expect(screen.getByText("We can't load this report right now. Try again in a minute.")).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('button', { name: 'Send reply' })).toBeInTheDocument();
    expect(load).toHaveBeenCalledTimes(2);
    expect(screen.queryByText(/We can't load this report/)).not.toBeInTheDocument();
  });

  it('a report that fails to load shows a plain sentence and Try again, never the HTTP status', async () => {
    const load = vi.spyOn(api, 'feedbackThread')
      .mockRejectedValueOnce(new ApiError(502, '502 Bad Gateway: could not load the report thread right now'))
      .mockResolvedValue({ number: 42, title: 'Loaded now', filedAt: '2026-07-09T10:00:00Z', events: [], state: 'open' });
    openHost();
    act(() => openInbox('feed', 42));
    expect(await screen.findByText("We can't load this report right now. Try again in a minute.")).toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(/502|Bad Gateway/);

    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText('Loaded now')).toBeInTheDocument();
    expect(load).toHaveBeenCalledTimes(2);
  });

  // The report closed after the thread loaded: the cloud answers 409 with the
  // sentence, and what was typed goes into the follow-up instead of being lost.
  it('a reply to a report that just closed says so and carries the text into a follow-up', async () => {
    vi.spyOn(api, 'feedbackThread').mockResolvedValue({ number: 42, title: 'Paste fails', filedAt: '2026-07-09T10:00:00Z', events: [], state: 'open' });
    vi.spyOn(api, 'feedbackReply').mockRejectedValue(
      new ApiError(409, "This report is closed, so it can't take replies. Send a follow-up instead."),
    );
    openHost();
    act(() => openInbox('feed', 42));
    fireEvent.change(await screen.findByPlaceholderText(/Add details/), { target: { value: 'Still broken in 1242' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send reply' }));

    const followUp = await screen.findByRole('button', { name: 'Send a follow-up' });
    expect(useStore.getState().toasts.at(-1)?.message).toBe("This report is closed, so it can't take replies. Send a follow-up instead.");
    expect(screen.queryByRole('button', { name: 'Send reply' })).not.toBeInTheDocument();
    fireEvent.click(followUp);
    expect(await screen.findByText('This is a follow-up to report #42.')).toBeInTheDocument();
    expect(((await screen.findByLabelText('What happened?')) as HTMLTextAreaElement).value).toBe('Still broken in 1242');
  });

  it('a reply that fails keeps the text and says so in plain words, never the HTTP status', async () => {
    vi.spyOn(api, 'feedbackThread').mockResolvedValue({ number: 42, title: 'Paste fails', filedAt: '2026-07-09T10:00:00Z', events: [], state: 'open' });
    vi.spyOn(api, 'feedbackReply').mockRejectedValue(new ApiError(502, '502 Bad Gateway: could not save the reply right now'));
    openHost();
    act(() => openInbox('feed', 42));
    const box = await screen.findByPlaceholderText(/Add details/) as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: 'Chrome 126' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send reply' }));

    await waitFor(() => expect(useStore.getState().toasts.at(-1)?.message).toBe("Couldn't add your reply. It's still in the box, so try again in a minute."));
    expect(box.value).toBe('Chrome 126');
    expect(screen.getByRole('button', { name: 'Send reply' })).toBeInTheDocument();
  });
});

describe('InboxHost / compose', () => {
  it('✎ opens the composer; cancel returns to the feed', () => {
    useStore.setState({ schedules: [sched()] });
    openHost();
    fireEvent.click(screen.getByRole('button', { name: 'Send feedback' }));
    expect(screen.getByLabelText('Title (optional)')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.getByText(/approve PR #7/)).toBeInTheDocument();
  });

  it('openFeedback (status bar, Help, Ctrl+K) opens the panel on compose', () => {
    render(<InboxHost />);
    act(() => {
      openFeedback();
    });
    expect(screen.getByRole('button', { name: 'Send' })).toBeInTheDocument();
    expect(screen.getByLabelText('What happened?')).toBeInTheDocument();
  });

  it('"Report this" opens the form filled in with the error', () => {
    localStorage.removeItem('solu.feedbackDraft.v2');
    render(<InboxHost />);
    act(() => {
      openFeedback({ type: 'bug', body: 'The Codex pane stopped with an error: boom' });
    });
    expect(screen.getByLabelText('What happened?')).toHaveValue('The Codex pane stopped with an error: boom');
    expect(screen.getByRole('button', { name: 'Bug' })).toHaveAttribute('aria-pressed', 'true');
  });

  // After a send the Inbox stays open on the new report. Its "Received" is
  // stamped read and fetched, so the row is in the feed and never unread.
  it('after a send, stays open on the new report with "Report #N sent" and no unread row', async () => {
    localStorage.removeItem('solu.feedbackDraft.v2');
    const toast = vi.fn();
    useStore.setState({ toast } as never);
    vi.spyOn(api, 'feedbackStatus').mockResolvedValue({ available: true });
    vi.spyOn(api, 'submitFeedback').mockResolvedValue({ ok: true, number: 812, url: 'u' });
    const read = vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    const inbox = vi.spyOn(api, 'feedbackInbox').mockResolvedValue({
      events: [fbEvent({ number: 812, title: 'Tabs vanish', event: 'received', message: 'Report received', readAt: new Date().toISOString() })],
    });
    const thread = vi.spyOn(api, 'feedbackThread').mockResolvedValue({
      number: 812, state: 'open', title: 'Tabs vanish', filedAt: new Date().toISOString(),
      events: [{ event: 'filed', message: 'Report filed', at: new Date().toISOString() }],
    });
    render(<InboxHost />);
    act(() => openFeedback());
    fireEvent.change(screen.getByLabelText('What happened?'), { target: { value: 'Tabs vanish' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));

    expect(await screen.findByText('Report #812 sent. Updates on it will show up here.')).toHaveAttribute('role', 'status');
    expect(screen.getByRole('dialog', { name: 'Inbox' })).toBeInTheDocument();
    await waitFor(() => expect(thread).toHaveBeenCalledWith(812));
    expect(await screen.findByText('Report #812 · filed', { exact: false })).toBeInTheDocument();
    expect(toast).not.toHaveBeenCalled();
    expect(read).toHaveBeenCalledWith(812);
    // The row arrives without waiting for the next poll, already read.
    const row = (await screen.findAllByText('Tabs vanish')).map((el) => el.closest('.inbox-row')).find(Boolean);
    expect(row).toBeTruthy();
    expect(row).not.toHaveClass('is-unread');
    expect(inbox.mock.invocationCallOrder.at(-1)).toBeGreaterThan(read.mock.invocationCallOrder[0]);
    expect(screen.queryByRole('button', { name: /Mark all/ })).not.toBeInTheDocument();
  });

  it('the "sent" line goes once another item is picked', async () => {
    localStorage.removeItem('solu.feedbackDraft.v2');
    useStore.setState({ toast: vi.fn() } as never);
    vi.spyOn(api, 'feedbackStatus').mockResolvedValue({ available: true });
    vi.spyOn(api, 'submitFeedback').mockResolvedValue({ ok: true, number: 812, url: 'u' });
    vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    vi.spyOn(api, 'feedbackInbox').mockResolvedValue({ events: [fbEvent()] });
    vi.spyOn(api, 'feedbackThread').mockImplementation(async (n) => ({
      number: n, state: 'open', title: `Report ${n}`, filedAt: new Date().toISOString(), events: [],
    }));
    render(<InboxHost />);
    act(() => openFeedback());
    fireEvent.change(screen.getByLabelText('What happened?'), { target: { value: 'Tabs vanish' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect(await screen.findByText(/^Report #812 sent\./)).toBeInTheDocument();
    fireEvent.click((await screen.findAllByText(/which browser/))[0]);
    expect(await screen.findByText('Report 42')).toBeInTheDocument();
    expect(screen.queryByText(/^Report #812 sent\./)).not.toBeInTheDocument();
  });
});
