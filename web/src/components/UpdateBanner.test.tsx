import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import type { UpdateStatus } from '../types';
import { UpdateBanner } from './UpdateBanner';
import { useCloud } from '../cloud';

const realApplyAllUpdates = useStore.getState().applyAllUpdates;

const shellStatus = (): UpdateStatus => ({
  current: '1.0.0',
  available: false,
  shellCurrent: '1.0.0',
  shellAvailable: true,
  shellLatest: '2.0.0',
  shellAutoApplyPaused: false,
});

describe('UpdateBanner shell auto-restart states', () => {
  beforeEach(() => {
    localStorage.clear();
    useStore.setState({
      updateStatus: shellStatus(),
      updateSettings: { autoApply: 'off', shellAutoApply: 'off' },
      deferredUpdate: null,
      shellAutoRestart: null,
      restartShellNowForUpdate: vi.fn().mockResolvedValue(undefined) as never,
      postponeShellAutoRestart: vi.fn(),
      applyAllUpdates: realApplyAllUpdates,
    });
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    delete (window as unknown as { __SOLU_TAURI__?: unknown }).__SOLU_TAURI__;
    delete (window as unknown as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
    localStorage.clear();
    vi.useRealTimers();
    useStore.setState({
      updateStatus: null,
      shellAutoRestart: null,
      versionPopoverOpen: false,
      applyAllUpdates: realApplyAllUpdates,
    });
  });

  it('always shows the armed countdown and both deferral actions despite dismissal', () => {
    const armedAt = Date.now();
    vi.spyOn(Date, 'now').mockReturnValue(armedAt);
    localStorage.setItem('solu.update.banner.dismissed', '2.0.0');
    useStore.setState({
      updateSettings: { autoApply: 'off', shellAutoApply: 'when-idle' },
      shellAutoRestart: { version: '2.0.0', deadlineMs: armedAt + 60_000 },
    });
    render(<UpdateBanner />);

    expect(screen.getByText('Desktop app update ready')).toBeInTheDocument();
    expect(screen.getByText(/restarting in 60s/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Restart now' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Hold 1 hour' })).toBeInTheDocument();
  });

  it('hides a shell-only loud banner while when-idle owns the update but is not armed', () => {
    useStore.setState({ updateSettings: { autoApply: 'off', shellAutoApply: 'when-idle' } });
    const { container } = render(<UpdateBanner />);
    expect(container).toBeEmptyDOMElement();
  });

  it('shows the ordinary installer banner when the silent restart is paused by control', () => {
    useStore.setState({
      updateSettings: { autoApply: 'off', shellAutoApply: 'when-idle' },
      updateStatus: { ...shellStatus(), shellAutoApplyPaused: true, shellAutoApplyPausedReason: 'automatic desktop restarts are not enabled for this channel yet' },
    });
    render(<UpdateBanner />);
    expect(screen.getByText('Desktop installer update ready')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Update now' })).toBeInTheDocument();
  });

  it('keeps the manual-only copy and Update now action unchanged when off', () => {
    render(<UpdateBanner />);
    expect(screen.getByText('Desktop installer update ready')).toBeInTheDocument();
    expect(screen.getByText('runs the desktop installer to finish')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Update now' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Tonight' })).toBeInTheDocument();
  });

  it('offers no installer update in a browser on an install without the desktop app', () => {
    useStore.setState({
      updateStatus: { ...shellStatus(), shellCurrent: '', notes: '- Keep one desktop shell and trim setup copy' },
    });
    const { container } = render(<UpdateBanner />);
    expect(container).toBeEmptyDOMElement();
  });

  it('never shows the daemon release notes on an installer-only banner', () => {
    useStore.setState({ updateStatus: { ...shellStatus(), notes: '- Keep one desktop shell and trim setup copy' } });
    render(<UpdateBanner />);
    expect(screen.getByText('Desktop installer update ready')).toBeInTheDocument();
    expect(screen.queryByText(/Keep one desktop shell/)).toBeNull();
  });

  it('restores the manual banner after a silent attempt fails on an older shell', async () => {
    Object.defineProperty(window, '__SOLU_TAURI__', { value: true, configurable: true });
    Object.defineProperty(window, '__TAURI_INTERNALS__', {
      value: { invoke: vi.fn().mockResolvedValue('1.0.0') },
      configurable: true,
    });
    localStorage.setItem('solu.shellUpdate.autoApplied', JSON.stringify({
      version: '2.0.0', at: Date.now(), attempts: 1,
    }));
    useStore.setState({
      updateStatus: { ...shellStatus(), shellAvailable: false },
      updateSettings: { autoApply: 'off', shellAutoApply: 'when-idle' },
    });
    render(<UpdateBanner />);
    await waitFor(() => expect(screen.getByText('Desktop installer update ready')).toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Update now' })).toBeInTheDocument();
  });

  it('keeps update details reachable from the banner when the status bar is hidden', () => {
    const previousWidth = window.innerWidth;
    Object.defineProperty(window, 'innerWidth', { value: 420, configurable: true });
    useStore.setState({
      updateStatus: {
        current: '1.0.0',
        latest: '2.0.0',
        available: true,
        workflowInflight: 2,
        workflowDeferState: 'backoff',
        workflowNextRetryMs: Date.now() + 120_000,
      },
      updateSettings: { autoApply: 'off', shellAutoApply: 'off' },
      deferredUpdate: null,
    });

    render(<UpdateBanner />);
    fireEvent.click(screen.getByRole('button', { name: /soluterminal v2.0.0 is ready/ }));
    expect(screen.getByRole('dialog', { name: 'Update status' })).toBeInTheDocument();

    Object.defineProperty(window, 'innerWidth', { value: previousWidth, configurable: true });
  });

  it('shows a live status-only workflow-drain countdown despite legacy applying, dismissal, and when-idle ownership', () => {
    vi.useFakeTimers();
    const now = Date.parse('2026-09-04T01:15:00Z');
    vi.setSystemTime(now);
    localStorage.setItem('solu.update.banner.dismissed', '0.0.1049');
    useStore.setState({
      updateSettings: { autoApply: 'when-idle', shellAutoApply: 'off' },
      deferredUpdate: null,
      updateStatus: {
        current: '0.0.1048', latest: '0.0.1049', available: true,
        applying: true,
        workflowDeferred: true, workflowDeferState: 'draining',
        workflowInflight: 6, workflowDeferDeadlineMs: now + 6 * 60_000 + 56_000,
      },
    });

    const { container } = render(<UpdateBanner />);
    expect(screen.getByText('Update 0.0.1049 ready')).toBeInTheDocument();
    expect(screen.getByText('Waiting for 6 running workers to finish · will postpone in 6:56 if still busy')).toBeInTheDocument();
    expect(container.querySelector('.update-banner-draining .update-banner-sub')).not.toBeNull();
    expect(screen.queryByRole('button', { name: 'Update now' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Tonight' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Dismiss' })).toBeNull();

    act(() => vi.advanceTimersByTime(1000));
    expect(screen.getByText('Waiting for 6 running workers to finish · will postpone in 6:55 if still busy')).toBeInTheDocument();
  });

  it('uses singular worker copy for an immediate typed workflow hold', () => {
    vi.useFakeTimers();
    const now = Date.parse('2026-09-04T01:15:00Z');
    vi.setSystemTime(now);
    useStore.setState({
      deferredUpdate: {
        cause: 'workflow', since: now, auto: false,
        workflowInflight: 1, workflowDeferDeadlineMs: now + 61_000,
      },
      updateStatus: {
        current: '0.0.1048', latest: '0.0.1049', available: true, applying: false,
      },
    });
    render(<UpdateBanner />);
    expect(screen.getByText('Waiting for 1 running worker to finish · will postpone in 1:01 if still busy')).toBeInTheDocument();
  });

  it('does not present a stale local workflow hold as draining while the release train publishes', () => {
    const now = Date.parse('2026-09-04T01:15:00Z');
    useStore.setState({
      deferredUpdate: {
        cause: 'workflow', since: now, auto: false,
        workflowInflight: 2, workflowDeferDeadlineMs: now + 60_000,
      },
      updateStatus: {
        current: '0.0.1048', latest: '0.0.1049', available: true, applying: false,
        workflowDeferred: true, workflowDeferState: 'publish',
      },
    });

    const { container } = render(<UpdateBanner />);
    expect(container.querySelector('.update-banner-draining')).toBeNull();
    expect(screen.queryByText('Update 0.0.1049 ready')).toBeNull();
  });

  it('routes a combined update through the monotonic store flow', async () => {
    const applyAllUpdates = vi.fn().mockResolvedValue(undefined);
    useStore.setState({
      updateStatus: {
        current: '0.0.900', latest: '0.0.932', available: true,
        shellCurrent: '0.0.900', shellLatest: '0.0.916', shellAvailable: true,
      },
      applyAllUpdates: applyAllUpdates as never,
    });
    render(<UpdateBanner />);
    fireEvent.click(screen.getByRole('button', { name: 'Update now' }));
    await waitFor(() => expect(applyAllUpdates).toHaveBeenCalledOnce());
  });

  it('does not open a release fallback after native downgrade refusal', async () => {
    const invoke = vi.fn(async (command: string) => {
      if (command === 'apply_shell_update') {
        return {
          launched: false,
          path: 'C:\\setup-0.0.916.exe',
          error: 'installed daemon 0.0.932 is newer than bundled daemon 0.0.916',
          downgradeRefused: true,
          installedVersion: '0.0.932',
          installerVersion: '0.0.916',
        };
      }
      return '0.0.900';
    });
    Object.defineProperty(window, '__SOLU_TAURI__', { value: true, configurable: true });
    Object.defineProperty(window, '__TAURI_INTERNALS__', { value: { invoke }, configurable: true });
    localStorage.setItem('solu.shellUpdate.autoApplied', JSON.stringify({
      version: '0.0.916', at: Date.now(), attempts: 1,
    }));
    useStore.setState({
      updateStatus: {
        current: '0.0.900', available: false, shellLatest: '0.0.916', shellAvailable: true,
      },
    });
    render(<UpdateBanner />);
    fireEvent.click(screen.getByRole('button', { name: 'Update now' }));

    await waitFor(() => expect(invoke).toHaveBeenCalledWith('apply_shell_update', { forceDowngrade: false }));
    expect(invoke.mock.calls.some(([command]) => command === 'open_external_window')).toBe(false);
  });

  it('does not offer a shell-only installer that carries an older daemon', async () => {
    const invoke = vi.fn(async (command: string) => {
      if (command === 'browser_pane_ping') return '0.0.900';
      if (command === 'apply_shell_update') throw new Error('native admission must not be reached');
      return undefined;
    });
    const toast = vi.fn();
    Object.defineProperty(window, '__SOLU_TAURI__', { value: true, configurable: true });
    Object.defineProperty(window, '__TAURI_INTERNALS__', { value: { invoke }, configurable: true });
    localStorage.setItem('solu.shellUpdate.autoApplied', JSON.stringify({
      version: '0.0.916', at: Date.now(), attempts: 1,
    }));
    useStore.setState({
      updateStatus: {
        current: '0.0.932', available: false, shellLatest: '0.0.916', shellAvailable: true,
      },
      toast,
      applyAllUpdates: realApplyAllUpdates,
    });
    render(<UpdateBanner />);
    // Every launch path refuses an installer that carries an older daemon, so
    // the banner does not offer it (lib/installerUpdate.ts).
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 20)); });
    expect(screen.queryByRole('button', { name: 'Update now' })).not.toBeInTheDocument();
    expect(toast).not.toHaveBeenCalled();
    expect(invoke.mock.calls.some(([command]) => command === 'apply_shell_update')).toBe(false);
    expect(invoke.mock.calls.some(([command]) => command === 'open_external_window')).toBe(false);
  });

  // 0.0.1199 shell → 0.0.1202 (2026-10-22): the native step downloaded the
  // installer, failed before launching it, and the banner opened GitHub.
  function nativeFailure(nativeResult: () => unknown) {
    const invoke = vi.fn(async (command: string, _args?: unknown) => {
      if (command === 'apply_shell_update') return nativeResult();
      if (command === 'browser_pane_ping') return '0.0.1199';
      return undefined;
    });
    Object.defineProperty(window, '__SOLU_TAURI__', { value: true, configurable: true });
    Object.defineProperty(window, '__TAURI_INTERNALS__', { value: { invoke }, configurable: true });
    localStorage.setItem('solu.shellUpdate.autoApplied', JSON.stringify({
      version: '0.0.1202', at: Date.now(), attempts: 1,
    }));
    return invoke;
  }

  it('retries through the local daemon when the native installer step fails', async () => {
    const invoke = nativeFailure(() => {
      throw new Error('installer update failed (exit 10): could not read the downloaded installer');
    });
    const applyShellUpdate = vi.fn().mockResolvedValue(undefined);
    useStore.setState({
      updateStatus: { current: '0.0.1202', available: false, shellLatest: '0.0.1202', shellAvailable: true },
      applyShellUpdate: applyShellUpdate as never,
      applyAllUpdates: realApplyAllUpdates,
    });
    render(<UpdateBanner />);
    fireEvent.click(screen.getByRole('button', { name: 'Update now' }));

    await waitFor(() => expect(applyShellUpdate).toHaveBeenCalledOnce());
    expect(invoke.mock.calls.some(([command]) => command === 'plugin:opener|open_url')).toBe(false);
    expect(JSON.stringify(invoke.mock.calls)).not.toContain('github.com');
  });

  // The app's own version can show an installer the daemon does not flag
  // (shellAvailable false: a WSL daemon still believes the older shell). The
  // click must use the same route as the popover, not the daemon-only action.
  it('installs a client-detected installer through the app updater', async () => {
    const invoke = vi.fn(async (command: string, _args?: unknown) => {
      if (command === 'apply_shell_update') return { launched: true, path: 'C:\\setup-2.0.0.exe' };
      if (command === 'browser_pane_ping') return '1.0.0';
      return undefined;
    });
    Object.defineProperty(window, '__SOLU_TAURI__', { value: true, configurable: true });
    Object.defineProperty(window, '__TAURI_INTERNALS__', { value: { invoke }, configurable: true });
    const applyShellUpdate = vi.fn().mockResolvedValue(undefined);
    useStore.setState({
      updateStatus: { current: '1.0.0', available: false, shellLatest: '2.0.0', shellAvailable: false },
      applyShellUpdate: applyShellUpdate as never,
      applyAllUpdates: realApplyAllUpdates,
    });
    render(<UpdateBanner />);
    fireEvent.click(await screen.findByRole('button', { name: 'Update now' }));

    await waitFor(() => expect(invoke).toHaveBeenCalledWith('apply_shell_update', { forceDowngrade: false }));
    expect(applyShellUpdate).not.toHaveBeenCalled();
  });

  // A local daemon built with sign-in serves /api/config, so useCloud reads
  // mode 'cloud' (isLocal). On the team channel the installer comes from that
  // daemon alone; the app's own updater reads public releases only.
  it('on the team channel, installs through the local daemon without the app updater', async () => {
    useCloud.setState({ mode: 'cloud', isLocal: true });
    try {
      const invoke = nativeFailure(() => {
        throw new Error('the app updater must not run on the team channel');
      });
      const applyShellUpdate = vi.fn().mockResolvedValue(undefined);
      useStore.setState({
        updateStatus: {
          current: '0.0.1202', available: false, shellLatest: '0.0.1202', shellAvailable: true, updateChannel: 'team',
        },
        applyShellUpdate: applyShellUpdate as never,
        applyAllUpdates: realApplyAllUpdates,
      });
      render(<UpdateBanner />);
      fireEvent.click(screen.getByRole('button', { name: 'Update now' }));

      await waitFor(() => expect(applyShellUpdate).toHaveBeenCalledOnce());
      expect(invoke.mock.calls.some(([command]) => command === 'apply_shell_update')).toBe(false);
      expect(invoke.mock.calls.some(([command]) => command === 'plugin:opener|open_url')).toBe(false);
    } finally {
      useCloud.setState({ mode: 'detecting', isLocal: false });
    }
  });

  it('falls back to the local daemon even when it serves the sign-in config', async () => {
    useCloud.setState({ mode: 'cloud', isLocal: true });
    try {
      nativeFailure(() => {
        throw new Error('update manifest is not for the public build');
      });
      const applyShellUpdate = vi.fn().mockResolvedValue(undefined);
      useStore.setState({
        updateStatus: { current: '0.0.1202', available: false, shellLatest: '0.0.1202', shellAvailable: true },
        applyShellUpdate: applyShellUpdate as never,
        applyAllUpdates: realApplyAllUpdates,
      });
      render(<UpdateBanner />);
      fireEvent.click(screen.getByRole('button', { name: 'Update now' }));

      await waitFor(() => expect(applyShellUpdate).toHaveBeenCalledOnce());
    } finally {
      useCloud.setState({ mode: 'detecting', isLocal: false });
    }
  });

  it('opens the public download page with the reason when every automatic path fails', async () => {
    const invoke = nativeFailure(() => {
      throw new Error('installer update failed (exit 10): could not read the downloaded installer');
    });
    const toast = vi.fn();
    useStore.setState({
      updateStatus: { current: '0.0.1202', available: false, shellLatest: '0.0.1202', shellAvailable: true },
      applyShellUpdate: vi.fn().mockRejectedValue(new Error('daemon download failed')) as never,
      applyAllUpdates: realApplyAllUpdates,
      toast,
    });
    render(<UpdateBanner />);
    fireEvent.click(screen.getByRole('button', { name: 'Update now' }));

    await waitFor(() => expect(invoke).toHaveBeenCalledWith(
      'plugin:opener|open_url', { url: 'https://soluterminal.com/download/' },
    ));
    expect(toast).toHaveBeenCalledWith(
      expect.stringContaining('could not read the downloaded installer'),
      expect.anything(),
    );
    expect(JSON.stringify(invoke.mock.calls)).not.toContain('github.com');
  });

  it('shows the downloaded installer when it was staged but did not open', async () => {
    const staged = 'C:\\Users\\x\\AppData\\Local\\soluterminal\\shell-update\\soluterminal_0.0.1202_x64-setup.exe';
    const invoke = nativeFailure(() => ({
      launched: false,
      path: staged,
      error: 'installer downloaded but did not start — open it manually',
    }));
    const applyShellUpdate = vi.fn().mockResolvedValue(undefined);
    useStore.setState({
      updateStatus: { current: '0.0.1202', available: false, shellLatest: '0.0.1202', shellAvailable: true },
      applyShellUpdate: applyShellUpdate as never,
      applyAllUpdates: realApplyAllUpdates,
    });
    render(<UpdateBanner />);
    fireEvent.click(screen.getByRole('button', { name: 'Update now' }));

    await waitFor(() => expect(invoke).toHaveBeenCalledWith('reveal_shell_installer', { path: staged }));
    expect(applyShellUpdate).not.toHaveBeenCalled();
    expect(invoke.mock.calls.some(([command]) => command === 'plugin:opener|open_url')).toBe(false);
  });
});
