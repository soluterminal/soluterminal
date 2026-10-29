import { render, screen, waitFor, fireEvent, cleanup } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import { ShellUpdateBanner } from './ShellUpdateBanner';

const realApplyAllUpdates = useStore.getState().applyAllUpdates;

// Controllable tauri-browser mock: the banner + hook read isDesktopApp(),
// getShellVersion() (local shell version), and openExternalWindow() (apply).
const mock = vi.hoisted(() => ({
  isDesktop: true,
  pingVersion: '0.0.803' as string | null,
  opened: [] as string[],
  nativeOk: false, // whether apply_shell_update (one-click) is available/succeeds
  nativeResult: null as { ok: boolean; staged?: string; error?: string; downgradeRefused?: boolean; installedVersion?: string; installerVersion?: string } | null,
  nativeCalls: 0,
  nativeForces: [] as boolean[],
  revealed: [] as string[],
  invoked: [] as Array<[string, unknown]>,
}));
vi.mock('../tauri-browser', () => ({
  isDesktopApp: () => mock.isDesktop,
  tauriInvoke: () => (mock.isDesktop
    ? (command: string, args?: unknown) => {
      mock.invoked.push([command, args]);
      return Promise.resolve(undefined);
    }
    : null),
  browserPanePing: () => Promise.resolve(mock.pingVersion),
  getShellVersion: () => Promise.resolve(mock.pingVersion),
  openExternalWindow: (url: string) => {
    mock.opened.push(url);
    return Promise.resolve(true);
  },
  openInSystemBrowser: (url: string) => {
    mock.opened.push(url);
    return Promise.resolve(true);
  },
  applyShellUpdateNative: (forceDowngrade = false) => {
    mock.nativeCalls += 1;
    mock.nativeForces.push(forceDowngrade);
    return Promise.resolve(mock.nativeResult ?? (mock.nativeOk ? { ok: true } : { ok: false, error: 'unknown command' }));
  },
  revealShellInstallerNative: (path: string) => {
    mock.revealed.push(path);
    return Promise.resolve(true);
  },
}));

function setLaunchMode(mode: 'cloud' | 'local' | undefined) {
  if (mode === undefined) {
    delete (window as unknown as { __SOLU_BACKEND__?: unknown }).__SOLU_BACKEND__;
    return;
  }
  Object.defineProperty(window, '__SOLU_BACKEND__', {
    value: { launchMode: mode },
    configurable: true,
  });
}

describe('ShellUpdateBanner', () => {
  beforeEach(() => {
    mock.isDesktop = true;
    mock.pingVersion = '0.0.803';
    mock.opened = [];
    mock.nativeOk = false;
    mock.nativeResult = null;
    mock.nativeCalls = 0;
    mock.nativeForces = [];
    mock.revealed = [];
    mock.invoked = [];
    localStorage.clear();
    setLaunchMode('cloud');
    useStore.setState({ updateStatus: { shellLatest: '0.0.835' } as never, applyAllUpdates: realApplyAllUpdates });
  });
  afterEach(() => {
    cleanup();
    setLaunchMode(undefined);
    useStore.setState({ updateStatus: null, applyAllUpdates: realApplyAllUpdates });
  });

  it('shows in the cloud desktop shell when the local shell is behind', async () => {
    render(<ShellUpdateBanner />);
    await waitFor(() => expect(screen.getByText(/newer desktop app \(v0\.0\.835\)/)).toBeInTheDocument());
    expect(screen.getByText(/you have v0\.0\.803/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Update now' })).toBeInTheDocument();
  });

  it('one-click: uses the native apply command when available (no browser fallback)', async () => {
    mock.nativeOk = true;
    render(<ShellUpdateBanner />);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Update now' })).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: 'Update now' }));
    await waitFor(() => expect(mock.nativeCalls).toBe(1));
    expect(mock.opened).toEqual([]); // native handled it — no release-page fallback
    expect(mock.nativeForces).toEqual([false]);
  });

  it('falls back to the public download page when the native command is unavailable (old shell)', async () => {
    mock.nativeOk = false;
    render(<ShellUpdateBanner />);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Update now' })).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: 'Update now' }));
    await waitFor(() => expect(mock.opened).toEqual([
      'https://soluterminal.com/download/',
    ]));
    expect(mock.nativeCalls).toBe(1);
  });

  it('shows staged launch recovery without opening the release page', async () => {
    const staged = 'C:\\Users\\x\\AppData\\Local\\soluterminal\\shell-update\\setup.exe';
    mock.nativeResult = { ok: false, staged, error: 'not observed' };
    render(<ShellUpdateBanner />);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Update now' })).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: 'Update now' }));

    await waitFor(() => expect(screen.getByText(/Installer downloaded but didn't open/)).toBeInTheDocument());
    expect(screen.getByText(staged)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Show in folder' })).toBeInTheDocument();
    expect(mock.opened).toEqual([]);
    fireEvent.click(screen.getByRole('button', { name: 'Show in folder' }));
    await waitFor(() => expect(mock.revealed).toEqual([staged]));
  });

  it('opens the public download page, never GitHub, for a pre-stage native failure', async () => {
    mock.nativeResult = { ok: false, error: 'unknown command' };
    render(<ShellUpdateBanner />);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Update now' })).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: 'Update now' }));
    await waitFor(() => expect(mock.opened).toEqual([
      'https://soluterminal.com/download/',
    ]));
  });

  it('keeps a downgrade refusal visible and never opens the stale installer release', async () => {
    mock.nativeResult = {
      ok: false,
      staged: 'C:\\setup-0.0.916.exe',
      error: 'installed daemon 0.0.932 is newer than bundled daemon 0.0.916',
      downgradeRefused: true,
      installedVersion: '0.0.932',
      installerVersion: '0.0.916',
    };
    render(<ShellUpdateBanner />);
    fireEvent.click(await screen.findByRole('button', { name: 'Update now' }));

    expect(await screen.findByText(/installed daemon v0\.0\.932 is newer than bundled v0\.0\.916/)).toBeInTheDocument();
    expect(mock.opened).toEqual([]);
    expect(mock.revealed).toEqual([]);
    expect(mock.nativeForces).toEqual([false]);
    expect(screen.queryByRole('button', { name: 'Show in folder' })).not.toBeInTheDocument();
  });

  it('uses daemon status to hold a shell-only downgrade before native invocation', async () => {
    mock.pingVersion = '0.0.900';
    mock.nativeOk = true;
    useStore.setState({
      updateStatus: {
        current: '0.0.932', available: false, shellLatest: '0.0.916', shellAvailable: false,
      } as never,
      applyAllUpdates: realApplyAllUpdates,
    });
    render(<ShellUpdateBanner />);
    fireEvent.click(await screen.findByRole('button', { name: 'Update now' }));

    await waitFor(() => expect(mock.nativeCalls).toBe(1));
    expect(mock.nativeForces).toEqual([false]);
    expect(screen.queryByText(/installed daemon v0\.0\.932 is newer than bundled v0\.0\.916/)).not.toBeInTheDocument();
    expect(mock.opened).toEqual([]);
    expect(mock.revealed).toEqual([]);
  });

  it('treats an empty staged path as a pre-stage failure', async () => {
    mock.nativeResult = { ok: false, staged: '', error: 'staged path missing' };
    render(<ShellUpdateBanner />);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Update now' })).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: 'Update now' }));
    await waitFor(() => expect(mock.opened).toEqual([
      'https://soluterminal.com/download/',
    ]));
    expect(screen.queryByRole('button', { name: 'Show in folder' })).not.toBeInTheDocument();
  });

  // 2026-10-29: a public 0.0.1225 app in cloud launch mode viewing a team
  // computer (team 0.0.1226). "Update now" ran the app's updater, which reads
  // public releases only, and fell back to the public download page.
  it('on a team computer, offers Local mode and never runs the app updater or the download page', async () => {
    const realOpenDialog = useStore.getState().openDialog;
    useStore.setState({
      updateStatus: { current: '0.0.1226', shellLatest: '0.0.1226', shellAvailable: true, updateChannel: 'team' } as never,
      openDialog: (d: { resolve: (v: unknown) => void }) => d.resolve(true),
    } as never);
    mock.pingVersion = '0.0.1225';
    try {
      render(<ShellUpdateBanner />);
      expect(await screen.findByText(/Update this computer's desktop app in Local mode/)).toBeInTheDocument();
      expect(screen.getByText(/the installer shown here belongs to the computer you're viewing/)).toBeInTheDocument();
      expect(screen.queryByText(/newer desktop app/)).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Update now' })).not.toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: 'Switch to Local' }));

      await waitFor(() => expect(mock.invoked).toEqual([['desktop_set_launch_mode', { mode: 'local' }]]));
      expect(mock.nativeCalls).toBe(0);
      expect(mock.opened).toEqual([]);
    } finally {
      useStore.setState({ openDialog: realOpenDialog });
    }
  });

  it('on a public computer, keeps the app updater', async () => {
    mock.nativeOk = true;
    useStore.setState({ updateStatus: { shellLatest: '0.0.835', updateChannel: 'public' } as never });
    render(<ShellUpdateBanner />);
    fireEvent.click(await screen.findByRole('button', { name: 'Update now' }));
    await waitFor(() => expect(mock.nativeCalls).toBe(1));
    expect(screen.queryByRole('button', { name: 'Switch to Local' })).not.toBeInTheDocument();
  });

  it('does NOT show in local mode (the daemon handles the badge there)', async () => {
    setLaunchMode('local');
    const { container } = render(<ShellUpdateBanner />);
    await Promise.resolve();
    expect(container).toBeEmptyDOMElement();
  });

  it('does NOT show in a browser tab (no desktop shell)', async () => {
    mock.isDesktop = false;
    const { container } = render(<ShellUpdateBanner />);
    await Promise.resolve();
    expect(container).toBeEmptyDOMElement();
  });

  it('does NOT show when the local shell already matches the manifest', async () => {
    mock.pingVersion = '0.0.835';
    const { container } = render(<ShellUpdateBanner />);
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it('stays dismissed for that version once the user closes it', async () => {
    const { container, rerender } = render(<ShellUpdateBanner />);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Dismiss' })).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(container).toBeEmptyDOMElement();
    // Re-mount: still dismissed (persisted per target version).
    rerender(<ShellUpdateBanner />);
    await Promise.resolve();
    expect(screen.queryByText(/newer desktop app/)).not.toBeInTheDocument();
  });
});
