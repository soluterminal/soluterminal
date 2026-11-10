import { currentRoutingScope } from '../api';
import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RoutingScope, WorkflowTaskSummary } from '../api';
import type { PaneInfo, Port, Workspace } from '../types';
import { useStore } from '../store';
import { StatusBar, plainSystemText, systemReasonDetail, systemWithoutWorkflow } from './StatusBar';
import { useEntitlements } from '../entitlements';

const workflowTasksMock = vi.hoisted(() => ({
  current: { tasks: null as WorkflowTaskSummary[] | null, activeCount: 0, awaitingCount: 0, listStale: false, scope: null as RoutingScope | null, generation: null },
}));
const apiMock = vi.hoisted(() => ({
  health: vi.fn().mockResolvedValue({ version: '1.2.3' }),
  workflowInbox: vi.fn().mockResolvedValue({ decide: [], recovering: [], waiting: [], history: [], counts: { decide: 0 } }),
}));

vi.mock('../api', () => ({
  onRoutingScopeChange: vi.fn(() => () => {}),
  currentRoutingScope: vi.fn(() => ({ prefix: '', backendPrefix: '', key: '|' })),
  currentPrefix: vi.fn(() => ''),
  api: apiMock,
}));

const pane: PaneInfo = {
  id: 'pane-1',
  cwd: '/home/user/project',
  cmd: ['bash'],
  title: 'Shell',
  status: 'running',
  createdAt: '2026-05-18T00:00:00.000Z',
  cols: 80,
  rows: 24,
  pid: 0,
  lastActivityMs: 0,
  state: 'idle',
  hasClaude: false,
};

function port(over: Partial<Port>): Port {
  return {
    port: 5199,
    addr: '127.0.0.1',
    pid: 42,
    process: 'node',
    cmdline: 'node vite',
    cwd: '/home/user/project',
    firstAt: '2026-07-10T00:00:00.000Z',
    proto: 'tcp',
    scheme: 'http',
    webStatus: 'ready',
    workspace: '_current',
    ...over,
  };
}

describe('StatusBar', () => {
  beforeEach(() => {
    workflowTasksMock.current = { tasks: null, activeCount: 0, awaitingCount: 0, listStale: false, scope: null, generation: null };
    useEntitlements.setState({ loaded: true, team: true, operator: true });
    apiMock.workflowInbox.mockResolvedValue({ decide: [], recovering: [], waiting: [], history: [], counts: { decide: 0 } });
    useStore.setState({
      panes: { 'pane-1': pane },
      focusedPaneId: 'pane-1',
      mainPresentation: {kind:'workspace'},
      activeWorkspace: '_current',
      ports: [],
      updateStatus: null,
      deferredUpdate: null,
      updateApplyRequestInFlight: false,
    });
  });

  afterEach(() => {
    useStore.setState({
      panes: {},
      layout: null,
      workspaces: [],
      activeBackend: null,
      focusedPaneId: null,
      activeWorkspace: '_current',
      ports: [],
      updateStatus: null,
      deferredUpdate: null,
      updateApplyRequestInFlight: false,
    });
    vi.restoreAllMocks();
  });

  it('shows the viewed owner while preserving ordinary workspace focus', async ()=>{
    const owner={...pane,id:'owner',title:'Owner task',pid:123};
    useStore.setState({panes:{'pane-1':pane,owner},mainPresentation:{kind:'owner',taskId:'wft_owner',scope:currentRoutingScope(),history:false,loading:false,pane:owner}});
    render(<StatusBar/>);
    expect(screen.getByText('Owner task')).toBeInTheDocument();
    // The pid rides in the path tooltip, not in the bar.
    expect(screen.queryByText(/pid 123/)).not.toBeInTheDocument();
    // The bar shows the home-shortened path; the tooltip has it in full.
    fireEvent.focus(screen.getByText('~/project'));
    expect(await screen.findByRole('tooltip')).toHaveTextContent('/home/user/project · pid 123');
    expect(useStore.getState().focusedPaneId).toBe('pane-1');
  });

  it('the Feedback button opens the feedback form', () => {
    const opened = vi.fn();
    const onOpen = (e: Event) => opened((e as CustomEvent).detail);
    window.addEventListener('solu:open-inbox', onOpen);
    try {
      render(<StatusBar />);
      fireEvent.click(screen.getByRole('button', { name: 'Feedback' }));
      expect(opened).toHaveBeenCalledWith(expect.objectContaining({ view: 'compose' }));
    } finally {
      window.removeEventListener('solu:open-inbox', onOpen);
    }
  });

  it('omits pid from the path tooltip when the focused pane has no useful pid', async () => {
    render(<StatusBar />);

    expect(screen.getByText('Shell')).toBeInTheDocument();
    expect(screen.queryByText(/^pid\b/i)).not.toBeInTheDocument();
    // The bar shows the home-shortened path; the tooltip has it in full.
    fireEvent.focus(screen.getByText('~/project'));
    expect(await screen.findByRole('tooltip')).toHaveTextContent(/^\/home\/user\/project$/);
  });

  it('shows the pane state only when it is not idle', () => {
    const { rerender } = render(<StatusBar />);
    expect(screen.queryByText('idle')).not.toBeInTheDocument();
    expect(screen.queryByText('—')).not.toBeInTheDocument();

    act(() => useStore.setState({ panes: { 'pane-1': { ...pane, state: 'choice-prompt' } } }));
    rerender(<StatusBar />);
    expect(screen.getByText('approval needed')).toBeInTheDocument();

    act(() => useStore.setState({ panes: { 'pane-1': { ...pane, state: 'busy' } } }));
    rerender(<StatusBar />);
    expect(screen.getByText('busy')).toBeInTheDocument();
  });

  it('counts running panes here and in total, and reveals the exited breakdown in the popover', () => {
    useStore.setState({
      panes: {
        'pane-1': pane,
        'pane-2': { ...pane, id: 'pane-2', title: 'Two' },
        'pane-3': { ...pane, id: 'pane-3', title: 'Dead', status: 'exited' },
      },
      // Only pane-1 (and the exited pane-3) sit in this workspace's layout.
      layout: { type: 'split', direction: 'horizontal', ratio: 0.5,
        a: { type: 'pane', paneId: 'pane-1' }, b: { type: 'pane', paneId: 'pane-3' } },
    });
    render(<StatusBar />);

    const chip = screen.getByText('1 here · 2 total');
    expect(screen.getByRole('button', { name: /1 running in this workspace · 2 running in all · 1 exited/ })).toBeInTheDocument();
    fireEvent.click(chip);

    expect(screen.getByText('2 running · 1 exited')).toBeInTheDocument();
    expect(screen.getByText('Close 1 exited pane')).toBeInTheDocument();
  });

  it('counts a worker recovered beside its lead as here, though it has no saved tile', () => {
    const lead = { ...pane, id: 'lead', title: 'Lead' };
    const worker = { ...pane, id: 'w1', title: 'Worker', origin: { source: 'agent', byPaneId: 'lead', leadPaneId: 'lead', at: 1 } } as PaneInfo;
    const other = { ...pane, id: 'other', title: 'Other' };
    const leadTile = { type: 'pane', paneId: 'lead' } as const;
    useStore.setState({
      panes: { lead, w1: worker, other },
      layout: leadTile,
      workspaces: [
        { name: 'Project', layout: leadTile, paneIds: ['lead'] },
        { name: 'Elsewhere', layout: { type: 'pane', paneId: 'other' }, paneIds: ['other'] },
      ] as Workspace[],
      activeWorkspace: 'Project',
      activeBackend: null,
      focusedPaneId: 'lead',
    });
    render(<StatusBar />);

    expect(screen.getByText('2 here · 3 total')).toBeInTheDocument();
  });

  it('stays a pure pane counter when a pane waits on the user', () => {
    // "Who needs me?" moved to the topbar attention bell — the panes chip no
    // longer flips to "N need you". Both panes are running, so it reads "2
    // here · 2 total" regardless of the needs-user state.
    useStore.setState({
      panes: {
        'pane-1': { ...pane, state: 'choice-prompt', attention: 'needs-user' },
        'pane-2': { ...pane, id: 'pane-2', title: 'Calm' },
      },
      layout: { type: 'split', direction: 'horizontal', ratio: 0.5,
        a: { type: 'pane', paneId: 'pane-1' }, b: { type: 'pane', paneId: 'pane-2' } },
    });
    render(<StatusBar />);

    expect(screen.getByText('2 here · 2 total')).toBeInTheDocument();
    expect(screen.queryByText(/needs? you/)).not.toBeInTheDocument();
  });

  it('does not surface attention in the panes popover (that is the bell now)', () => {
    useStore.setState({
      panes: {
        'pane-1': { ...pane, state: 'choice-prompt', attention: 'needs-user', title: 'Deploy' },
      },
    });
    render(<StatusBar />);

    fireEvent.click(screen.getByText('0 here · 1 total'));

    const popover = screen.getByRole('dialog', { name: 'Panes overview' });
    expect(within(popover).getByText('1 running')).toBeInTheDocument();
    expect(within(popover).queryByText(/Waiting on you/)).not.toBeInTheDocument();
  });

  it('counts confirmed web services in the current workspace', () => {
    useStore.setState({
      ports: [
        port({ port: 5199, paneId: 'pane-1' }),
        port({ port: 9876, pid: 43, cmdline: 'unified-inbox', workspace: 'Other' }),
      ],
    });
    render(<StatusBar />);

    expect(screen.getByText('1 service')).toBeInTheDocument();
    expect(screen.queryByText(/\bports\b/)).not.toBeInTheDocument();
  });

  it('hides the dev-servers chip when no listener belongs to a pane', () => {
    useStore.setState({ ports: [port({ port: 9876, workspace: 'Other' })] });
    render(<StatusBar />);

    expect(screen.queryByText(/service/)).not.toBeInTheDocument();
  });

  it('opens the full ports panel from the chip', () => {
    useStore.setState({ ports: [port({ port: 5199, paneId: 'pane-1' })] });
    render(<StatusBar />);

    fireEvent.click(screen.getByText('1 service'));

    expect(screen.getByRole('combobox', {name:'Service scope'})).toBeInTheDocument();
    expect(screen.getByText(':5199')).toBeInTheDocument();
  });

  it('keeps one queued chip mounted while its typed cause changes', async () => {
    const realisticPaneId = 'pane_0123456789abcdef';
    useStore.setState({
      updateStatus: { current: '1.0.0', latest: '2.0.0', available: true },
      deferredUpdate: {
        cause: 'workflow', since: Date.now(), auto: false, workflowInflight: 2,
      },
    });
    render(<StatusBar />);

    const workflowText = await screen.findByText('Workflow drain (2)');
    const chip = workflowText.closest('button');
    expect(chip).not.toBeNull();
    expect(chip).toHaveClass('statusbar-version-queued');
    expect(chip?.textContent).not.toContain('Focus');

    act(() => {
      useStore.setState({
        deferredUpdate: {
          cause: 'focus', since: Date.now(), auto: false, paneId: realisticPaneId,
        },
      });
    });
    expect(screen.getByText(`Focus ${realisticPaneId} active`).closest('button')).toBe(chip);

    act(() => {
      useStore.setState({
        deferredUpdate: { cause: 'unknown', since: Date.now(), auto: false },
      });
    });
    expect(screen.getByText('Waiting on active work').closest('button')).toBe(chip);
  });

  it('surfaces quiet-window backoff with the next retry', async () => {
    useStore.setState({
      updateStatus: {
        current: '1.0.0', latest: '2.0.0', available: true,
        workflowDeferred: true, workflowDeferState: 'backoff',
        workflowInflight: 3, workflowDeferAttempts: 2,
        workflowNextRetryMs: Date.now() + 2 * 60_000,
      },
      deferredUpdate: null,
    });
    render(<StatusBar />);
    const label = await screen.findByText(/Update deferred — retry in 2m/);
    expect(label).toBeInTheDocument();
    expect(label.closest('button')).toHaveClass('statusbar-version-queued');
  });

  it('surfaces a typed workflow retry immediately before the next status GET', async () => {
    useStore.setState({
      updateStatus: {
        current: '1.0.0', latest: '2.0.0', available: true,
        workflowDeferred: true, workflowDeferState: 'draining',
        workflowInflight: 3,
      },
      deferredUpdate: {
        cause: 'workflow', since: Date.now(), auto: false,
        workflowInflight: 3, workflowNextRetryMs: Date.now() + 2 * 60_000,
      },
    });
    render(<StatusBar />);

    const label = await screen.findByText(/Update deferred — retry in 2m/);
    expect(label.closest('button')).toHaveClass('statusbar-version-queued');
  });

  it('lets authoritative workflow drain state override a stale local Focus hold', async () => {
    useStore.setState({
      updateStatus: {
        current: '0.0.1048', latest: '0.0.1049', available: true,
        workflowDeferred: true, workflowDeferState: 'draining',
        workflowInflight: 6, workflowDeferDeadlineMs: Date.now() + 6 * 60_000,
      },
      deferredUpdate: {
        cause: 'focus', since: Date.now(), auto: false, paneId: 'stale-focus',
      },
    });
    render(<StatusBar />);

    expect(screen.queryByText(/Focus stale-focus active/)).toBeNull();
    expect(await screen.findByText(/^Workflow drain \(6\) · \d+:\d{2}$/)).toBeInTheDocument();
  });

  it('renders a status-only drain as queued even with a legacy applying bit', async () => {
    useStore.setState({
      updateStatus: {
        current: '0.0.1048', latest: '0.0.1049', available: true,
        applying: true,
        workflowDeferred: true, workflowDeferState: 'draining',
        workflowInflight: 6, workflowDeferDeadlineMs: Date.now() + 6 * 60_000,
      },
      deferredUpdate: null,
    });
    render(<StatusBar />);

    expect(screen.queryByText('daemon updating…')).toBeNull();
    const label = await screen.findByText(/^Workflow drain \(6\) · \d+:\d{2}$/);
    expect(label.closest('button')).toHaveClass('statusbar-version-queued');
  });
});

describe('systemWithoutWorkflow', () => {
  it('drops workflow runtimes and engine areas and recomputes the verdict', () => {
    const out = systemWithoutWorkflow({
      level: 'warn',
      reason: 'runtimes: 10 unhealthy runtimes',
      reasons: [
        { key: 'runtimes', level: 'warn', text: 'runtimes: 10 unhealthy runtimes' },
        { key: 'engine', level: 'warn', text: 'engine: disk floor active' },
      ],
    });
    expect(out.level).toBe('ok');
    expect(out.reason).toBeUndefined();
  });

  it('keeps the other areas, worst first', () => {
    const out = systemWithoutWorkflow({
      level: 'critical',
      reason: 'runtimes: 3 unhealthy runtimes',
      reasons: [
        { key: 'runtimes', level: 'critical', text: 'runtimes: 3 unhealthy runtimes' },
        { key: 'disk', level: 'warn', text: 'disk: 2.0 GB free' },
      ],
    });
    expect(out).toMatchObject({ level: 'warn', reason: 'disk: 2.0 GB free' });
  });

  it('drops a workflow headline from an older daemon without per-area reasons', () => {
    expect(systemWithoutWorkflow({ level: 'warn', reason: 'runtimes: 1 unhealthy runtime' })).toMatchObject({ level: 'ok' });
    expect(systemWithoutWorkflow({ level: 'warn', reason: 'disk: 2.0 GB free' })).toMatchObject({ level: 'warn' });
  });
});

describe('plainSystemText', () => {
  it('stays quiet for warnings and speaks plainly when critical', () => {
    expect(plainSystemText({ level: 'warn', reason: 'hostPressure: memory PSI some10 12%' })).toBeNull();
    expect(plainSystemText({
      level: 'critical',
      reason: 'host disk: 6% free · VHDX 1010.7 GB physical, not sparse',
      reasons: [{ key: 'hostDisk', level: 'critical', text: 'host disk: 6% free' }],
    })).toBe('Disk almost full');
    expect(plainSystemText({ level: 'critical', reason: 'mystery: x' })).toBe('System needs attention');
  });

  it('stays quiet for a Windows-disk warning (the daemon only goes critical under ~5% and ~20 GB)', () => {
    expect(plainSystemText({
      level: 'warn',
      reason: 'host disk: 45.0 GB free on C:\\',
      reasons: [{ key: 'hostDisk', level: 'warn', text: 'host disk: 45.0 GB free on C:\\' }],
    })).toBeNull();
  });
});

describe('systemReasonDetail', () => {
  it('returns the headline reason detail for the tooltip, not another area', () => {
    const text = 'host disk: 102.9 GB free on C:\\';
    const detail = '5.4% of 1907.0 GB free · VHDX 1010.7 GB physical, not sparse · 55.8 GB (20000 files) in %TEMP% (measurement incomplete)';
    expect(systemReasonDetail({
      level: 'warn',
      reason: text,
      reasons: [{ key: 'hostDisk', level: 'warn', text, detail }, { key: 'disk', level: 'warn', text: 'disk: 2.0 GB free', detail: 'other' }],
    })).toBe(detail);
    expect(systemReasonDetail({ level: 'warn', reason: 'disk: 2.0 GB free', reasons: [{ key: 'disk', level: 'warn', text: 'disk: 2.0 GB free' }] })).toBeUndefined();
    expect(systemReasonDetail({ level: 'warn', reason: 'disk: 2.0 GB free' })).toBeUndefined();
  });
});
