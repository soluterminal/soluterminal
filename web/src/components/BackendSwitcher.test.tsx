import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { useCloud } from '../cloud';
import { ContextPill } from './ContextPill';
import { noteRelayConfigured, resetRelayConfiguredForTests } from '../lib/relayConfigured';

// Mock the shared daemon-health poller so tests can drive the relayed host
// backend that cloud mode reads. `current` is reset to null in beforeEach.
const healthRef = vi.hoisted(() => ({
  current: null as { backendID?: string; shellPlatform?: string } | null,
}));
vi.mock('../lib/useDaemonHealth', () => ({
  useDaemonHealth: () => ({ health: healthRef.current, lastOkMs: 0, polling: false }),
  refreshDaemonHealth: () => {},
  getDaemonHealthState: () => ({
    health: healthRef.current,
    polling: false,
    bundleState: { kind: 'ok', runningBundleVersion: 'dev', shellDaemonVersion: '', shellWebVersion: '', targetVersion: '', legacyStaleVersion: '' },
  }),
  subscribeDaemonHealthState: () => () => {},
}));

const basePane: PaneInfo = {
  id: 'pane-1',
  cwd: '/home/user/project',
  cmd: ['bash'],
  title: 'Shell',
  status: 'running',
  createdAt: '2026-06-29T00:00:00.000Z',
  cols: 80,
  rows: 24,
  pid: 100,
  lastActivityMs: 0,
  state: 'idle',
  hasClaude: false,
};

function setDesktopBackend(backend: {
  requested?: 'wsl' | 'host' | 'dual';
  actual?: 'wsl' | 'host' | 'dual';
  fallback?: boolean;
  hostPlatform?: 'windows' | 'macos' | 'linux';
  launchMode?: 'local' | 'cloud';
}) {
  Object.defineProperty(window, '__SOLU_TAURI__', { value: true, configurable: true });
  Object.defineProperty(window, '__SOLU_BACKEND__', {
    value: { hostPlatform: 'windows', launchMode: 'local', ...backend },
    configurable: true,
  });
  Object.defineProperty(window, '__TAURI_INTERNALS__', {
    value: { invoke: vi.fn().mockResolvedValue(undefined) },
    configurable: true,
  });
}

// The controls live behind the top bar's hosting chip (ContextPill); open
// the popover to reach them. The chip's accessible name starts with
// "Environment".
function openEnv() {
  fireEvent.click(screen.getByRole('button', { name: /^Environment/ }));
}

describe('BackendControls via the hosting chip', () => {
  beforeEach(() => {
    window.history.replaceState(null, '', '/');
    healthRef.current = null;
    useCloud.setState({ mode: 'local' });
    useStore.setState({
      activeBackend: null,
      panes: {},
      switchBackend: vi.fn().mockResolvedValue(undefined),
    });
  });

  afterEach(() => {
    delete (window as unknown as { __SOLU_TAURI__?: boolean }).__SOLU_TAURI__;
    delete (window as unknown as { __SOLU_BACKEND__?: unknown }).__SOLU_BACKEND__;
    delete (window as unknown as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
    useCloud.setState({ mode: 'detecting' });
    resetRelayConfiguredForTests();
    vi.restoreAllMocks();
  });

  it('offers no Cloud hosting when the daemon has no relay (a build without a hosted cloud)', () => {
    setDesktopBackend({ requested: 'host', actual: 'host' });
    noteRelayConfigured(false);
    render(<ContextPill />);
    openEnv();
    expect(screen.queryByRole('button', { name: 'Cloud' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Local' })).toBeNull();
    // The Windows startup controls are unaffected.
    expect(screen.getByRole('button', { name: 'Linux' })).toBeInTheDocument();
  });

  it('keeps the hosting switch in Cloud launch mode, the in-app way back to Local', () => {
    setDesktopBackend({ launchMode: 'cloud' });
    useCloud.setState({ mode: 'cloud' });
    noteRelayConfigured(false);
    render(<ContextPill />);
    openEnv();
    expect(screen.getByRole('button', { name: 'Local' })).not.toBeDisabled();
    expect(screen.getByRole('button', { name: 'Cloud' })).toBeDisabled();
  });

  it('explains a refused Cloud switch instead of blaming an old shell', async () => {
    setDesktopBackend({ requested: 'host', actual: 'host' });
    const invoke = vi.fn().mockRejectedValue('This build has no hosted cloud configured (SOLU_CLOUD_URL), so it only runs on this computer.');
    Object.defineProperty(window, '__TAURI_INTERNALS__', { value: { invoke }, configurable: true });
    const toast = vi.fn();
    useStore.setState({
      toast,
      openDialog: (d: { resolve: (v: unknown) => void }) => d.resolve(true),
    } as never);
    vi.spyOn(console, 'warn').mockImplementation(() => {});

    render(<ContextPill />);
    openEnv();
    fireEvent.click(screen.getByRole('button', { name: 'Cloud' }));

    await vi.waitFor(() => expect(toast).toHaveBeenCalled());
    expect(toast.mock.calls[0][0]).toMatch(/no hosted cloud configured/);
    expect(toast.mock.calls[0][0]).not.toMatch(/version of the desktop app/);
  });

  it('shows the saved desktop startup mode as segments only (no label rows)', () => {
    setDesktopBackend({ requested: 'host', actual: 'host' });

    render(<ContextPill />);
    openEnv();

    // No "Mode"/"Side" caption rows — the active segment carries the state.
    expect(screen.queryByText('Mode')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Linux' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Dual' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Windows' })).toBeDisabled();
    // Hosting row: local launch mode active, cloud offered.
    expect(screen.getByRole('button', { name: 'Local' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Cloud' })).not.toBeDisabled();
  });

  it('collapses to a single chip summarizing the current environment', () => {
    setDesktopBackend({ requested: 'host', actual: 'host' });

    render(<ContextPill />);

    // Only the chip renders until it's opened — the segments are hidden.
    const chip = screen.getByRole('button', { name: /^Environment/ });
    expect(chip).toHaveAccessibleName(/Local · Windows/);
    expect(screen.queryByRole('button', { name: 'Linux' })).not.toBeInTheDocument();
  });

  it('switching hosting confirms, then invokes desktop_set_launch_mode', async () => {
    setDesktopBackend({ requested: 'host', actual: 'host' });
    const invoke = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(window, '__TAURI_INTERNALS__', {
      value: { invoke },
      configurable: true,
    });
    // Auto-confirm the restart dialog (confirmDialog routes through the store).
    useStore.setState({
      openDialog: (d: { resolve: (v: unknown) => void }) => d.resolve(true),
    } as never);

    render(<ContextPill />);
    openEnv();
    fireEvent.click(screen.getByRole('button', { name: 'Cloud' }));

    await vi.waitFor(() =>
      expect(invoke).toHaveBeenCalledWith('desktop_set_launch_mode', { mode: 'cloud' }),
    );
  });

  it('hides Windows startup controls outside Windows local desktop launch mode', () => {
    setDesktopBackend({ requested: 'host', actual: 'host', hostPlatform: 'macos' });
    const { rerender } = render(<ContextPill />);
    openEnv();

    expect(screen.queryByRole('button', { name: 'Windows' })).not.toBeInTheDocument();

    setDesktopBackend({ requested: 'host', actual: 'host', launchMode: 'cloud' });
    rerender(<ContextPill />);

    expect(screen.queryByRole('button', { name: 'Windows' })).not.toBeInTheDocument();
  });

  it('keeps Windows startup controls visible when the cloud relay is connected', () => {
    setDesktopBackend({ requested: 'host', actual: 'host', launchMode: 'local' });
    useCloud.setState({ mode: 'cloud' });

    render(<ContextPill />);
    openEnv();

    expect(screen.getByRole('button', { name: 'Linux' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Windows' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Dual' })).toBeInTheDocument();
  });

  it('marks the saved mode on fallback, lets it retry, and lets Windows be chosen for good', async () => {
    // Saved Dual, WSL didn't start, running on Windows. "Windows" used to look
    // selected and be disabled, so the saved mode could never change and every
    // start tried WSL again.
    setDesktopBackend({ requested: 'dual', actual: 'host', fallback: true });
    const invoke = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(window, '__TAURI_INTERNALS__', { value: { invoke }, configurable: true });
    useStore.setState({
      openDialog: (d: { resolve: (v: unknown) => void }) => d.resolve(true),
    } as never);

    render(<ContextPill />);
    openEnv();

    const windows = screen.getByRole('button', { name: 'Windows' });
    const dual = screen.getByRole('button', { name: 'Dual' });
    expect(windows).not.toBeDisabled();
    expect(windows.className).not.toContain('active');
    expect(dual).not.toBeDisabled();
    expect(dual.className).toContain('active');
    expect(screen.getByRole('note').textContent).toBe(
      'Saved mode: Dual. It didn’t start, so this session is running on Windows. Choose Windows to stop trying Dual at each start.',
    );

    fireEvent.click(windows);
    await vi.waitFor(() =>
      expect(invoke).toHaveBeenCalledWith('desktop_set_backend_mode', { mode: 'host' }),
    );
  });

  it('retries the saved mode after a fallback when it is picked again', async () => {
    setDesktopBackend({ requested: 'wsl', actual: 'host', fallback: true });
    const invoke = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(window, '__TAURI_INTERNALS__', { value: { invoke }, configurable: true });
    useStore.setState({
      openDialog: (d: { resolve: (v: unknown) => void }) => d.resolve(true),
    } as never);

    render(<ContextPill />);
    openEnv();
    fireEvent.click(screen.getByRole('button', { name: 'Linux' }));

    await vi.waitFor(() =>
      expect(invoke).toHaveBeenCalledWith('desktop_set_backend_mode', { mode: 'wsl' }),
    );
  });

  it('without a fallback the saved mode is the selected, disabled one and there is no note', () => {
    setDesktopBackend({ requested: 'dual', actual: 'dual' });

    render(<ContextPill />);
    openEnv();

    expect(screen.getByRole('button', { name: 'Dual' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Windows' })).not.toBeDisabled();
    expect(screen.queryByRole('note')).toBeNull();
  });

  it('switches the active side in router dual mode without using the legacy URL swap', () => {
    window.history.replaceState(null, '', '/?solu_router=1&b=wsl');
    const switchBackend = vi.fn().mockResolvedValue(undefined);
    useStore.setState({
      activeBackend: 'wsl',
      switchBackend,
      panes: {
        'pane-1': { ...basePane, id: 'pane-1', backend: 'wsl' },
        'pane-2': { ...basePane, id: 'pane-2', backend: 'win' },
      },
    });

    render(<ContextPill />);
    openEnv();

    fireEvent.click(screen.getByRole('button', { name: 'PowerShell' }));

    expect(switchBackend).toHaveBeenCalledWith('win');
    expect(screen.getByTitle('1 WSL pane')).toBeDisabled();
  });

  it('cloud desktop client shows the hosting switch with Cloud active', () => {
    // Cloud desktop client: startup pills hidden (you can't restart a remote
    // host's shell), but the hosting row lets you flip back to Local.
    setDesktopBackend({ actual: 'host', launchMode: 'cloud' });
    useCloud.setState({ mode: 'cloud' });

    render(<ContextPill />);
    openEnv();

    expect(screen.getByRole('button', { name: 'Cloud' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Local' })).not.toBeDisabled();
    // No backend startup pills, no captions.
    expect(screen.queryByRole('button', { name: 'Linux' })).not.toBeInTheDocument();
    expect(screen.queryByText('Mode')).not.toBeInTheDocument();
  });

  it('shows the hosting state display-only in a browser tab (no desktop shell)', () => {
    // No __SOLU_BACKEND__/__SOLU_TAURI__ — a browser tab on the cloud URL.
    // The page origin decides hosting, so it renders as text, not a switch.
    useCloud.setState({ mode: 'cloud' });

    render(<ContextPill />);

    expect(screen.getByTitle(/^Environment: Cloud/)).toBeInTheDocument();
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });

  it('shows a local browser tab as Local display-only', () => {
    useCloud.setState({ mode: 'local' });

    render(<ContextPill />);

    expect(screen.getByTitle(/^Environment: Local/)).toBeInTheDocument();
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });

  it('shows no hosting element while cloud detection is still in flight (browser tab)', () => {
    useCloud.setState({ mode: 'detecting' });

    render(<ContextPill />);

    expect(screen.queryByTitle(/^Environment/)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^Environment/ })).not.toBeInTheDocument();
  });
});
