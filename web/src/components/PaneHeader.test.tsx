import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { PaneInfo, Schedule } from '../types';
import { PaneHeader } from './PaneHeader';
import { PaneHeaderBadges } from './PaneHeaderBadges';
import { DialogHost } from './DialogHost';
import { MEASURE_SENTINEL } from './PaneHeaderOverflow';
import { useStore } from '../store';

// Focus is retired by default (lib/focusAvailability). These suites cover the
// Focus code paths that still exist behind SOLU_NATIVE_RENDERER=1, so they run
// with it on unless a test flips focusFlag.on.
const focusFlag = vi.hoisted(() => ({ on: true }));

vi.mock('../lib/focusAvailability', () => ({ useFocusEnabled: () => focusFlag.on }));


const pane: PaneInfo = {
  id: 'pane-1',
  cwd: '/home/user/project',
  cmd: ['bash'],
  title: 'Build',
  status: 'running',
  createdAt: '2026-05-18T00:00:00.000Z',
  cols: 80,
  rows: 24,
  pid: 1234,
  lastActivityMs: 0,
  state: 'idle',
  hasClaude: false,
};

function renderHeader(onClose = vi.fn()) {
  render(
    <>
      <PaneHeader
        pane={pane}
        paneIndex={1}
        activityClass="idle"
        editingTitle={false}
        titleDraft={pane.title}
        unread={false}
        connStatus="open"
        setTitleDraft={vi.fn()}
        setEditingTitle={vi.fn()}
        onDragStart={vi.fn()}
        onDragEnd={vi.fn()}
        onRename={vi.fn()}
        onClose={onClose}
        isZoomed={false}
        canZoom={false}
        onToggleZoom={vi.fn()}
      />
      {/* Close now uses the in-app confirm dialog (native confirm() no-ops
          in Tauri's WebView2), so the modal must be mounted to interact. */}
      <DialogHost />
    </>,
  );
  return onClose;
}

describe('PaneHeader close button', () => {
  afterEach(() => {
    useStore.setState({ dialog: null });
    vi.restoreAllMocks();
  });

  it('does not close the session when the confirmation is cancelled', async () => {
    const onClose = renderHeader();

    fireEvent.click(screen.getByRole('button', { name: 'Close session' }));

    // In-app confirm modal renders; cancelling must not close the session.
    await screen.findByText('Close "Build"?');
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    await waitFor(() =>
      expect(screen.queryByText('Close "Build"?')).not.toBeInTheDocument(),
    );
    expect(onClose).not.toHaveBeenCalled();
  });

  it('closes the session after confirmation', async () => {
    const onClose = renderHeader();

    fireEvent.click(screen.getByRole('button', { name: 'Close session' }));

    await screen.findByText('Close "Build"?');
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));

    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });
});

describe('PaneHeader status label', () => {
  it('uses reconnecting status styling while the session is still running', () => {
    render(
      <PaneHeader
        pane={pane}
        paneIndex={1}
        activityClass="idle"
        editingTitle={false}
        titleDraft={pane.title}
        unread={false}
        connStatus="reconnecting"
        setTitleDraft={vi.fn()}
        setEditingTitle={vi.fn()}
        onDragStart={vi.fn()}
        onDragEnd={vi.fn()}
        onRename={vi.fn()}
        onClose={vi.fn()}
        isZoomed={false}
        canZoom={false}
        onToggleZoom={vi.fn()}
      />,
    );

    expect(screen.getByText('reconnecting…')).toHaveClass('reconnecting');
    expect(screen.getByText('reconnecting…')).not.toHaveClass('running');
  });
});

describe('PaneHeader Codex Focus toggle', () => {
  const renderCodex = (overrides: Partial<PaneInfo>) => render(
    <PaneHeader
      pane={{ ...pane, agent: 'codex', cmd: ['codex'], ...overrides }}
      paneIndex={1}
      activityClass="idle"
      editingTitle={false}
      titleDraft={pane.title}
      unread={false}
      connStatus="open"
      setTitleDraft={vi.fn()}
      setEditingTitle={vi.fn()}
      onDragStart={vi.fn()}
      onDragEnd={vi.fn()}
      onRename={vi.fn()}
      onClose={vi.fn()}
      isZoomed={false}
      canZoom={false}
      onToggleZoom={vi.fn()}
      onSwitchMode={vi.fn()}
    />,
  );

  it('when Focus is retired, hides the toggle on terminal panes but keeps the way back on native ones', () => {
    focusFlag.on = false;
    try {
      const term = renderCodex({ kind: '', codexThreadId: 'thread-1' });
      expect(screen.queryByRole('group', { name: 'Renderer mode' })).toBeNull();
      term.unmount();
      renderCodex({ kind: 'native', codexThreadId: 'thread-1' });
      expect(screen.getByRole('group', { name: 'Renderer mode' })).toBeInTheDocument();
    } finally {
      focusFlag.on = true;
    }
  });

  it('shows for Codex Focus and known-thread Terminal panes', () => {
    const first = renderCodex({ kind: 'native', codexThreadId: 'thread-1' });
    expect(screen.getByRole('group', { name: 'Renderer mode' })).toBeInTheDocument();
    first.unmount();
    renderCodex({ kind: '', codexThreadId: 'thread-1' });
    expect(screen.getByRole('group', { name: 'Renderer mode' })).toBeInTheDocument();
  });

  it('enables Focus immediately for a fresh Terminal Codex before its first thread', () => {
    renderCodex({ kind: '', hasCodex: true, codexThreadId: undefined });
    const toggle = screen.getByRole('group', { name: 'Renderer mode' });
    expect(toggle).not.toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'Focus renderer' })).toBeEnabled();
  });

  it('shows for shell-launched Codex once its thread id is discovered', () => {
    renderCodex({ agent: undefined, cmd: ['bash'], hasCodex: true, kind: '', codexThreadId: 'thread-shell' });
    expect(screen.getByRole('group', { name: 'Renderer mode' })).toBeInTheDocument();
  });

  it('locks Focus while foreground provider ownership is ambiguous', () => {
    renderCodex({ agent: undefined, cmd: ['bash'], hasClaude: true, hasCodex: true, identityBlocked: true });
    const toggle = screen.getByRole('group', { name: 'Renderer mode' });
    expect(toggle).toHaveAttribute('aria-disabled', 'true');
    expect(toggle).toHaveAttribute('title', expect.stringContaining('conversation ownership is ambiguous'));
  });
});

describe('PaneHeaderBadges Codex Focus auto-approval', () => {
  const heldReason = 'composer has text (unsent input in the pane; auto-approval waits until 10:09:50)';

  it.each(['auto-pending', 'needs-user'] as const)('shows the input hold during %s, then clears it when the prompt resolves', (attention) => {
    const heldPane: PaneInfo = { ...pane, hasCodex: true, autoMode: 'always', state: 'choice-prompt', attention, attentionReason: heldReason };
    const { rerender } = render(<PaneHeaderBadges pane={heldPane} paneIndex={1} />);
    expect(screen.getByText('waiting for input')).toHaveAttribute('title', `Auto-approval is waiting: ${heldReason}`);
    expect(screen.queryByText('auto-approving…')).not.toBeInTheDocument();
    rerender(<PaneHeaderBadges pane={{ ...heldPane, attention: 'auto-pending', attentionReason: undefined }} paneIndex={1} />);
    expect(screen.queryByText('waiting for input')).not.toBeInTheDocument();
    expect(screen.getByText('auto-approving…')).toBeInTheDocument();
    rerender(<PaneHeaderBadges pane={{ ...heldPane, state: 'busy', attention: 'none' }} paneIndex={1} />);
    expect(screen.queryByText('waiting for input')).not.toBeInTheDocument();
    expect(screen.queryByText('auto-approving…')).not.toBeInTheDocument();
  });

  it('does not show a stale input hold after disabling auto mode', () => {
    render(<PaneHeaderBadges pane={{ ...pane, hasCodex: true, autoMode: '', state: 'choice-prompt', attention: 'needs-user', attentionReason: heldReason }} paneIndex={1} />);
    expect(screen.queryByText('waiting for input')).not.toBeInTheDocument();
  });

  it('shows an input hold instead of an older armed countdown, then resumes the countdown', () => {
    useStore.setState({ ariaAutoPending: [{ confirmId: 'hold-countdown', paneId: pane.id, paneTitle: pane.title, input: '\\r', orderId: 'test', startedAt: Date.now(), deadlineAt: Date.now() + 5000, countdownMs: 5000, status: 'pending' }] });
    try {
      const heldPane: PaneInfo = { ...pane, hasCodex: true, autoMode: 'cautious', state: 'choice-prompt', attention: 'auto-pending', attentionReason: 'user typing' };
      const { rerender } = render(<PaneHeaderBadges pane={heldPane} paneIndex={1} />);
      expect(screen.getByText('waiting for input')).toBeInTheDocument();
      expect(screen.queryByText(/approving in/)).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Cancel the pending auto-approval' })).not.toBeInTheDocument();
      rerender(<PaneHeaderBadges pane={{ ...heldPane, attentionReason: undefined }} paneIndex={1} />);
      expect(screen.queryByText('waiting for input')).not.toBeInTheDocument();
      expect(screen.getByText(/approving in/)).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Cancel the pending auto-approval' })).toBeInTheDocument();
    } finally {
      useStore.setState({ ariaAutoPending: [] });
    }
  });

  it('shows the shared auto-approve control by default', () => {
    render(
      <PaneHeaderBadges
        pane={{ ...pane, agent: 'codex', kind: 'native', cmd: ['codex'], autoMode: '' }}
        paneIndex={1}
      />,
    );
    expect(screen.getByRole('button', { name: /Auto-approve:/ })).toBeInTheDocument();
  });

  it('keeps the transient auto-approving chip immediately right of the auto badge', () => {
    const { container } = render(
      <PaneHeaderBadges
        pane={{ ...pane, hasClaude: true, autoMode: 'cautious', attention: 'auto-pending' }}
        paneIndex={1}
        showAutoMode
      />,
    );

    expect(container.querySelector('.pane-automode-badge')?.nextElementSibling)
      .toHaveClass('pane-autoapproving-chip');
  });
});

describe('PaneHeader managed-harness auto-approval capability', () => {
  const renderPane = (overrides: Partial<PaneInfo>) => render(
    <PaneHeader
      pane={{ ...pane, ...overrides }}
      paneIndex={1}
      activityClass="idle"
      editingTitle={false}
      titleDraft={pane.title}
      unread={false}
      connStatus="open"
      setTitleDraft={vi.fn()}
      setEditingTitle={vi.fn()}
      onDragStart={vi.fn()}
      onDragEnd={vi.fn()}
      onRename={vi.fn()}
      onClose={vi.fn()}
      isZoomed={false}
      canZoom={false}
      onToggleZoom={vi.fn()}
    />,
  );

  it('offers auto-approval for a managed Pi pane', () => {
    renderPane({ cmd: ['node', 'launch.mjs', 'pi'], hasPi: true });
    expect(screen.getByRole('button', { name: /Auto-approve:/ })).toBeInTheDocument();
  });

  it('offers auto-approval for a DeepSeek terminal client pane', () => {
    renderPane({ cmd: ['bash'], hasDeepseek: true });
    expect(screen.getByRole('button', { name: /Auto-approve:/ })).toBeInTheDocument();
  });

  it('omits auto-approval for a plain shell pane', () => {
    renderPane({ cmd: ['bash'], hasPi: false });
    expect(screen.queryByRole('button', { name: /Auto-approve:/ })).not.toBeInTheDocument();
  });
});

describe('PaneHeader Claude resume badge', () => {
  it('uses the Codex thread id for a resumable Codex pane', () => {
    render(
      <PaneHeader
        pane={{ ...pane, agent: 'codex', kind: 'native', codexThreadId: 'thread-codex-1', resumeReady: true }}
        paneIndex={1}
        activityClass="idle"
        editingTitle={false}
        titleDraft={pane.title}
        unread={false}
        connStatus="open"
        setTitleDraft={vi.fn()}
        setEditingTitle={vi.fn()}
        onDragStart={vi.fn()}
        onDragEnd={vi.fn()}
        onRename={vi.fn()}
        onClose={vi.fn()}
        isZoomed={false}
        canZoom={false}
        onToggleZoom={vi.fn()}
      />,
    );
    expect(screen.getByText('Copy ID')).toHaveAttribute('aria-label', 'Copy Codex thread id thread-codex-1');
  });

  it('shows the same pill for a shell-launched Codex after discovery', () => {
    render(
      <PaneHeader
        pane={{ ...pane, cmd: ['bash'], hasCodex: true, codexThreadId: 'thread-shell-1', resumeReady: true }}
        paneIndex={1}
        activityClass="idle"
        editingTitle={false}
        titleDraft={pane.title}
        unread={false}
        connStatus="open"
        setTitleDraft={vi.fn()}
        setEditingTitle={vi.fn()}
        onDragStart={vi.fn()}
        onDragEnd={vi.fn()}
        onRename={vi.fn()}
        onClose={vi.fn()}
        isZoomed={false}
        canZoom={false}
        onToggleZoom={vi.fn()}
      />,
    );
    expect(screen.getByText('Copy ID')).toHaveAttribute('aria-label', 'Copy Codex thread id thread-shell-1');
  });

  it('uses the Muse session id for a resumable Muse pane', () => {
    render(
      <PaneHeader
        pane={{ ...pane, agent: 'muse', kind: 'native', museSessionId: 'muse-session-1', resumeReady: true }}
        paneIndex={1}
        activityClass="idle"
        editingTitle={false}
        titleDraft={pane.title}
        unread={false}
        connStatus="open"
        setTitleDraft={vi.fn()}
        setEditingTitle={vi.fn()}
        onDragStart={vi.fn()}
        onDragEnd={vi.fn()}
        onRename={vi.fn()}
        onClose={vi.fn()}
        isZoomed={false}
        canZoom={false}
        onToggleZoom={vi.fn()}
      />,
    );
    expect(screen.getByText('Copy ID')).toHaveAttribute('aria-label', 'Copy Muse session id muse-session-1');
  });

  it('renders a compact click-to-copy resume badge with the full id in its tooltip', () => {
    render(
      <PaneHeader
        pane={{ ...pane, claudeSessionId: '5e6473fa-90f1-4c60-bdf7-690a6dcc61be', resumeReady: true }}
        paneIndex={1}
        activityClass="idle"
        editingTitle={false}
        titleDraft={pane.title}
        unread={false}
        connStatus="open"
        setTitleDraft={vi.fn()}
        setEditingTitle={vi.fn()}
        onDragStart={vi.fn()}
        onDragEnd={vi.fn()}
        onRename={vi.fn()}
        onClose={vi.fn()}
        isZoomed={false}
        canZoom={false}
        onToggleZoom={vi.fn()}
      />,
    );

    // Compact label that says what a click does (it copies; it does not
    // resume anything), with no shortened id baked into the visible text…
    const badge = screen.getByText('Copy ID');
    expect(screen.queryByText('resume')).toBeNull();
    // …but the full id is recoverable via the tooltip + aria-label.
    expect(badge).toHaveAttribute(
      'title',
      expect.stringContaining('5e6473fa-90f1-4c60-bdf7-690a6dcc61be'),
    );
    expect(badge.getAttribute('title')).toMatch(/^Copy the Claude session id/);
    expect(badge).toHaveAttribute(
      'aria-label',
      'Copy Claude session id 5e6473fa-90f1-4c60-bdf7-690a6dcc61be',
    );
  });

  it('hides the resume badge until the session transcript exists (resumeReady)', () => {
    // The daemon pins claudeSessionId at SessionStart, before the transcript is
    // written — the badge must stay hidden until resumeReady flips true.
    render(
      <PaneHeader
        pane={{ ...pane, claudeSessionId: '5e6473fa-90f1-4c60-bdf7-690a6dcc61be' }}
        paneIndex={1}
        activityClass="idle"
        editingTitle={false}
        titleDraft={pane.title}
        unread={false}
        connStatus="open"
        setTitleDraft={vi.fn()}
        setEditingTitle={vi.fn()}
        onDragStart={vi.fn()}
        onDragEnd={vi.fn()}
        onRename={vi.fn()}
        onClose={vi.fn()}
        isZoomed={false}
        canZoom={false}
        onToggleZoom={vi.fn()}
      />,
    );
    expect(screen.queryByText('Copy ID')).toBeNull();
  });
});

describe('PaneHeaderBadges next-schedule chip', () => {
  const mkSchedule = (over: Partial<Schedule> = {}): Schedule => ({
    id: 's1',
    name: 'Morning digest',
    cron: '0 9 * * *',
    paneId: pane.id,
    prompt: 'summarise the overnight runs',
    enabled: true,
    createdAt: '2026-07-01T00:00:00.000Z',
    fireCount: 0,
    skipCount: 0,
    errorCount: 0,
    nextFire: new Date(Date.now() + 12 * 60_000).toISOString(),
    ...over,
  });

  afterEach(() => useStore.setState({ schedules: [] }));

  it('announces an upcoming scheduled prompt bound to this pane', () => {
    useStore.setState({ schedules: [mkSchedule()] });
    render(<PaneHeaderBadges pane={pane} paneIndex={1} />);
    expect(screen.getByText(/in 12m/)).toBeInTheDocument();
    // The prompt rides the tooltip so the header stays terse.
    expect(screen.getByTitle(/Morning digest/)).toBeInTheDocument();
  });

  it('ignores schedules bound to a DIFFERENT pane', () => {
    useStore.setState({ schedules: [mkSchedule({ paneId: 'pane-other' })] });
    render(<PaneHeaderBadges pane={pane} paneIndex={1} />);
    expect(screen.queryByText(/in 12m/)).toBeNull();
  });

  it('renders nothing when a disabled schedule has no nextFire', () => {
    // The daemon omits nextFire for a disabled schedule — that absence is the gate.
    useStore.setState({ schedules: [mkSchedule({ enabled: false, nextFire: undefined })] });
    const { container } = render(<PaneHeaderBadges pane={pane} paneIndex={1} />);
    expect(container.querySelector('.pane-schedule-chip')).toBeNull();
  });

  it('says "snoozed" instead of naming a run that will be skipped', () => {
    // The daemon's nextFire deliberately ignores snoozeUntil, so this occurrence
    // is advertised but suppressed — the chip must not promise it.
    useStore.setState({
      schedules: [mkSchedule({ snoozeUntil: new Date(Date.now() + 60 * 60_000).toISOString() })],
    });
    render(<PaneHeaderBadges pane={pane} paneIndex={1} />);
    expect(screen.getByText(/snoozed/)).toBeInTheDocument();
    expect(screen.queryByText(/in 12m/)).toBeNull();
  });

  it('prefers the soonest schedule it can state truthfully', () => {
    useStore.setState({
      schedules: [
        mkSchedule({ id: 'later', nextFire: new Date(Date.now() + 45 * 60_000).toISOString() }),
        mkSchedule({
          id: 'sooner-but-snoozed',
          nextFire: new Date(Date.now() + 5 * 60_000).toISOString(),
          snoozeUntil: new Date(Date.now() + 60 * 60_000).toISOString(),
        }),
      ],
    });
    render(<PaneHeaderBadges pane={pane} paneIndex={1} />);
    expect(screen.getByText(/in 45m/)).toBeInTheDocument();
  });
});

describe('PaneHeaderBadges orchestrator role chip', () => {
  it('marks an orchestrator pane in the header cluster', () => {
    render(<PaneHeaderBadges pane={{ ...pane, cmd: ['claude'], role: 'orchestrator' }} paneIndex={1} />);
    const chip = screen.getByText('orch');
    expect(chip).toBeInTheDocument();
    expect(chip).toHaveAttribute('title', expect.stringContaining('Claude'));
  });

  it('names Codex in the orchestrator identity', () => {
    render(<PaneHeaderBadges pane={{ ...pane, agent: 'codex', cmd: ['codex'], role: 'orchestrator' }} paneIndex={1} />);
    expect(screen.getByText('orch')).toHaveAttribute('title', expect.stringContaining('Codex'));
  });

  it('shows nothing for a plain pane', () => {
    render(<PaneHeaderBadges pane={pane} paneIndex={1} />);
    expect(screen.queryByText('orch')).toBeNull();
  });
});

describe('PaneHeaderBadges context pack chip', () => {
  for (const status of ['injected', 'cwd', 'unsupported', 'missing'] as const) {
    it(`shows the ${status} state`, () => {
      render(
        <PaneHeaderBadges
          pane={{ ...pane, contextPack: '/packs/strategy', contextPackStatus: status }}
          paneIndex={1}
        />,
      );
      const chip = screen.getByText('pack');
      expect(chip).toHaveClass(`pane-pack-${status}`);
      expect(chip.getAttribute('title')).toContain('strategy');
    });
  }

  it('shows no chip without a context pack', () => {
    render(<PaneHeaderBadges pane={pane} paneIndex={1} />);
    expect(screen.queryByText('pack')).toBeNull();
  });
});


describe('renderer switch confirmation visibility', () => {
  afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

  function setup(available: number) {
    let resize = () => {};
    vi.stubGlobal('ResizeObserver', class { constructor(cb: () => void) { resize = cb; } observe() {} disconnect() {} });
    vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockImplementation(() => available);
    // Every chip is 80px wide and the non-collapsing rest of the header needs more than 320px.
    vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(80);
    vi.spyOn(HTMLElement.prototype, 'scrollWidth', 'get').mockReturnValue(MEASURE_SENTINEL + 800);
    const onSwitchMode = vi.fn();
    const switchPaneMode = vi.spyOn(useStore.getState(), 'switchPaneMode').mockResolvedValue(null);
    const terminalKeys = vi.fn();
    render(<>
      <PaneHeader pane={{ ...pane, hasCodex: true, cmd: ['codex'], state: 'busy' }}
        paneIndex={1} activityClass="busy" editingTitle={false} titleDraft={pane.title}
        unread={false} connStatus="open" setTitleDraft={vi.fn()} setEditingTitle={vi.fn()}
        onDragStart={vi.fn()} onDragEnd={vi.fn()} onRename={vi.fn()} onClose={vi.fn()}
        isZoomed={false} canZoom={false} onToggleZoom={vi.fn()} onSwitchMode={onSwitchMode} />
      <textarea aria-label="Terminal input" onKeyDown={e => terminalKeys(e.key, e.defaultPrevented)} />
    </>);
    return { onSwitchMode, switchPaneMode, terminalKeys, resize: (width: number) => act(() => { available = width; resize(); }) };
  }

  function typeInTerminal(terminalKeys: ReturnType<typeof vi.fn>) {
    const terminal = screen.getByRole('textbox', { name: 'Terminal input' });
    terminal.focus();
    expect(fireEvent.keyDown(terminal, { key: 'Enter' })).toBe(true);
    expect(fireEvent.keyDown(terminal, { key: 'Escape' })).toBe(true);
    expect(terminalKeys.mock.calls).toEqual([['Enter', false], ['Escape', false]]);
  }

  it('cancels an inline confirm when its trigger folds, without consuming terminal keys', async () => {
    const { resize, terminalKeys, switchPaneMode, onSwitchMode } = setup(1200);
    fireEvent.click(screen.getByRole('button', { name: 'Focus renderer' }));
    expect(screen.getByRole('alertdialog')).toBeVisible();
    resize(320);
    await screen.findByRole('button', { name: /more pane details/ });
    expect(screen.queryByRole('alertdialog', { hidden: true })).toBeNull();
    typeInTerminal(terminalKeys);
    expect(switchPaneMode).not.toHaveBeenCalled();
    expect(onSwitchMode).not.toHaveBeenCalled();
    resize(1200);
    await waitFor(() => expect(screen.queryByRole('button', { name: /more pane details/ })).toBeNull());
    expect(screen.queryByRole('alertdialog', { hidden: true })).toBeNull();
  });

  it('cancels a popover confirm on close and does not restore it when reopened', () => {
    const { terminalKeys, switchPaneMode, onSwitchMode } = setup(320);
    const more = screen.getByRole('button', { name: /more pane details/ });
    fireEvent.click(more);
    fireEvent.click(screen.getByRole('button', { name: 'Focus renderer' }));
    expect(screen.getByRole('alertdialog')).toBeVisible();
    fireEvent.click(more);
    expect(screen.queryByRole('alertdialog', { hidden: true })).toBeNull();
    typeInTerminal(terminalKeys);
    expect(switchPaneMode).not.toHaveBeenCalled();
    expect(onSwitchMode).not.toHaveBeenCalled();
    fireEvent.click(more);
    expect(screen.queryByRole('alertdialog', { hidden: true })).toBeNull();
  });

  it('handles Enter and Escape only while focus is inside the confirmation', () => {
    const { terminalKeys, switchPaneMode, onSwitchMode } = setup(1200);
    fireEvent.click(screen.getByRole('button', { name: 'Focus renderer' }));
    typeInTerminal(terminalKeys);
    expect(screen.getByRole('alertdialog')).toBeVisible();
    expect(switchPaneMode).not.toHaveBeenCalled();
    expect(onSwitchMode).not.toHaveBeenCalled();
    const confirm = screen.getByRole('button', { name: 'Switch when done' });
    confirm.focus();
    fireEvent.keyDown(confirm, { key: 'Escape' });
    expect(screen.queryByRole('alertdialog', { hidden: true })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Focus renderer' }));
    const reopened = screen.getByRole('button', { name: 'Switch when done' });
    reopened.focus();
    fireEvent.keyDown(reopened, { key: 'Enter' });
    expect(switchPaneMode).toHaveBeenCalledOnce();
    expect(screen.queryByRole('alertdialog', { hidden: true })).toBeNull();
  });
});
