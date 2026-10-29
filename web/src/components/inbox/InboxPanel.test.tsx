import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest';
import { useEntitlements } from '../../entitlements';
import { useStore } from '../../store';
import { api } from '../../api';
import { InboxHost, openInbox, openInboxAt } from './InboxPanel';
import { __resetFeedbackInboxForTests } from '../../lib/useFeedbackInbox';
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
  vi.restoreAllMocks();
});

function openHost() {
  render(<InboxHost />);
  act(() => openInbox());
}

describe('InboxHost / feed', () => {
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
    fireEvent.click(screen.getByRole('tab', { name: /Feedback 1/ }));
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
  });

  it('a fixed thread hides the reply box', async () => {
    vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    vi.spyOn(api, 'feedbackThread').mockResolvedValue({
      number: 38,
      title: 'Dark mode flashes white',
      filedAt: '2026-07-05T10:00:00Z',
      events: [
        { event: 'filed', message: 'Report filed', at: '2026-07-05T10:00:00Z' },
        { event: 'fixed', message: 'Your report was fixed', at: '2026-07-10T09:00:00Z' },
      ],
    });
    __resetFeedbackInboxForTests([fbEvent({ number: 38, title: 'Dark mode flashes white', event: 'fixed', message: 'Your report was fixed' })]);
    openHost();
    fireEvent.click(screen.getAllByText(/report was fixed/)[0]);
    await screen.findByText(/This report is fixed/);
    expect(screen.queryByPlaceholderText(/Add details/)).toBeNull();
  });
});

describe('InboxHost / compose', () => {
  it('✎ opens the composer; cancel returns to the feed', () => {
    useStore.setState({ schedules: [sched()] });
    openHost();
    fireEvent.click(screen.getByRole('button', { name: 'Send feedback' }));
    expect(screen.getByLabelText('Title')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.getByText(/approve PR #7/)).toBeInTheDocument();
  });

  it('setFeedbackOpen (Ctrl+K path) opens the panel on compose', () => {
    render(<InboxHost />);
    act(() => {
      useStore.getState().setFeedbackOpen(true);
    });
    expect(screen.getByRole('button', { name: 'Send feedback' })).toBeInTheDocument();
    expect(screen.getByLabelText('Title')).toBeInTheDocument();
  });
});
