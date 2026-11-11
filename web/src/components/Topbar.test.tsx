import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AgentReport } from '../api';
import { setExtensionsForTests } from '../extensions';
import { Topbar } from './Topbar';
import { useEntitlements } from '../entitlements';
import { useStore } from '../store';
import { __resetFeedbackInboxForTests } from '../lib/useFeedbackInbox';
import type { Schedule } from '../types';

function schedule(kind: Schedule['kind']): Schedule {
  return {
    id: `s-${kind}`, name: 'Morning digest', kind, enabled: true, createdAt: '2026-07-01T00:00:00Z',
    fireCount: 0, skipCount: 0, errorCount: 0,
  } as Schedule;
}

afterEach(() => {
  useStore.setState({ schedules: [] });
  __resetFeedbackInboxForTests([]);
  setExtensionsForTests(null);
  useEntitlements.setState({ loaded: false, team: false, operator: false, account: false });
  delete (window as unknown as { __SOLU_TAURI__?: unknown }).__SOLU_TAURI__;
  delete (window as unknown as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
  vi.unstubAllGlobals();
});

describe('Topbar window controls', () => {
  it('badges a pending managed-agent decision in the inbox bubble', () => {
    const report: AgentReport = {
      agentId: 'agt_board_steward',
      agentName: 'Board Steward',
      runId: 'run_needs_you',
      at: '2026-08-27T09:00:00Z',
      title: 'Needs you — decide the task disposition',
      items: [{ taskId: 'wft_abc123', decision: 'decide-next-step' }],
    };
    setExtensionsForTests([{
      inboxReports: { useReports: () => [report], acknowledge: vi.fn(), acknowledgeAll: vi.fn() },
    }]);

    render(<Topbar onOpenHelp={() => {}} />);

    const inbox = screen.getByRole('button', { name: /Inbox — 1 unread, some need you/i });
    expect(inbox.querySelector('.topbar-inbox-badge')).toHaveClass('is-needs');
    expect(inbox.querySelector('.topbar-inbox-badge')).toHaveTextContent('1');
  });

  it('keeps New window before the standard controls in the shared cluster', () => {
    vi.stubGlobal('__SOLU_TAURI__', true);
    vi.stubGlobal('__TAURI_INTERNALS__', { invoke: vi.fn().mockResolvedValue(undefined) });

    const { container } = render(<Topbar onOpenHelp={() => {}} />);
    const controls = container.querySelector('.window-controls');
    expect(controls).toHaveAttribute('data-tauri-drag-region', 'false');
    expect(
      [...controls!.querySelectorAll('button')].map((button) => button.getAttribute('aria-label')),
    ).toEqual(['New window', 'Minimize', 'Maximize', 'Close']);
  });
});

describe('Topbar Inbox button', () => {
  it('is hidden on the desktop while the Inbox has nothing and nothing can reach it', () => {
    useStore.setState({ schedules: [schedule('prompt')] });
    render(<Topbar onOpenHelp={() => {}} />);
    expect(screen.queryByRole('button', { name: /^Inbox/ })).toBeNull();
  });

  it('shows once an automation that runs an agent exists, before its first report', () => {
    useStore.setState({ schedules: [schedule('task')] });
    render(<Topbar onOpenHelp={() => {}} />);
    expect(screen.getByRole('button', { name: /^Inbox/ })).toBeInTheDocument();
  });

  it('shows for a feedback update, read or not', () => {
    __resetFeedbackInboxForTests([{
      number: 42, title: 'Paste fails on Windows', event: 'received',
      message: "Thanks, we've got it. Report #42 is with the team.",
      at: '2026-11-10T08:00:00Z', readAt: '2026-11-10T08:00:00Z',
    }]);
    render(<Topbar onOpenHelp={() => {}} />);
    const inbox = screen.getByRole('button', { name: /^Inbox/ });
    expect(inbox.querySelector('.topbar-inbox-badge')).toBeNull();
  });

  // Phones reach the form from the drawer's "Send feedback" row now, so an
  // empty Inbox hides there too, and shows like the desktop's once it can hold
  // something.
  it('on phones, hides while empty and shows once the Inbox can hold something', () => {
    const width = window.innerWidth;
    Object.defineProperty(window, 'innerWidth', { configurable: true, value: 390 });
    try {
      const { unmount } = render(<Topbar onOpenHelp={() => {}} />);
      expect(screen.queryByRole('button', { name: /^Inbox/ })).toBeNull();
      unmount();
      useStore.setState({ schedules: [schedule('task')] });
      render(<Topbar onOpenHelp={() => {}} />);
      expect(screen.getByRole('button', { name: /^Inbox/ })).toBeInTheDocument();
    } finally {
      Object.defineProperty(window, 'innerWidth', { configurable: true, value: width });
    }
  });
});

describe('Topbar team-only controls', () => {
  it('hides the dictation mic and Task Owners for a public install', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false });
    const { container } = render(<Topbar onOpenHelp={() => {}} />);
    expect(container.querySelector('.voice-btn')).toBeNull();
    expect(screen.queryByRole('button', { name: /Task Owners/ })).toBeNull();
  });

  it('keeps them hidden while the verdict is still loading', () => {
    useEntitlements.setState({ loaded: false, team: false, operator: false });
    const { container } = render(<Topbar onOpenHelp={() => {}} />);
    expect(container.querySelector('.voice-btn')).toBeNull();
  });

});
