import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import type { Health } from '../types';

// --- Mocks (mirror of AgentSetupBanner.test.tsx, muse-flavored) ---------
let healthValue: Health | null = null;
const refreshDaemonHealth = vi.fn();
vi.mock('../lib/useDaemonHealth', () => ({
  useDaemonHealth: () => ({ health: healthValue, lastOkMs: 0, polling: false }),
  refreshDaemonHealth: () => refreshDaemonHealth(),
}));

let desktop = true;
vi.mock('../cloudPair', () => ({
  isDesktopShell: () => desktop,
}));

const museRecheck = vi.fn().mockResolvedValue({
  museChecked: true, museInstalled: false, museVersion: '', museAuthed: false,
});
const museInstallCommand = vi.fn().mockResolvedValue({
  supported: true,
  cmd: ['bash', '-lc', 'curl -fsSL https://dev.meta.ai/install.sh | sh'],
  display: 'curl -fsSL https://dev.meta.ai/install.sh | sh',
  note: 'Installs the Muse Code CLI (no admin needed).',
});
vi.mock('../api', () => ({
  api: {
    museRecheck: () => museRecheck(),
    museRecheckOn: () => museRecheck(),
    museInstallCommand: () => museInstallCommand(),
  },
}));

// Stub the store the install hook reaches via getState().
const storeMock = vi.hoisted(() => {
  const toast = vi.fn();
  const createPane = vi.fn().mockResolvedValue(undefined);
  type StoreState = {
    activeBackend: 'wsl' | 'win' | null;
    panes: Record<string, unknown>;
    toast: typeof toast;
    createPane: typeof createPane;
  };
  let state: StoreState = {
    activeBackend: null,
    panes: {},
    toast,
    createPane,
  };
  const useStore = vi.fn((selector: (s: StoreState) => unknown) => selector(state)) as
    ReturnType<typeof vi.fn> & { getState: () => StoreState };
  useStore.getState = () => state;
  const reset = () => {
    state = {
      activeBackend: null,
      panes: {},
      toast,
      createPane,
    };
  };
  const setActiveBackend = (activeBackend: 'wsl' | 'win' | null) => {
    state = { ...state, activeBackend };
  };
  return { toast, createPane, useStore, reset, setActiveBackend };
});
vi.mock('../store', () => ({
  useStore: storeMock.useStore,
}));

import { MuseInstallBanner } from './MuseInstallBanner';

// Router-mode drive (single-backend by default).
let routerMode = false;
let urlBackend: 'wsl' | 'win' = 'wsl';
vi.mock('../dualBackend', () => ({
  isRouterMode: () => routerMode,
  activeBackendFromUrl: () => urlBackend,
}));

function health(over: Partial<Health>): Health {
  return { status: 'ok', version: '0.0.0', ...over } as Health;
}

describe('MuseInstallBanner', () => {
  beforeEach(() => {
    window.history.replaceState(null, '', '/');
    window.localStorage.removeItem('solu.hideMuseSetupBanner');
    healthValue = null;
    desktop = true;
    storeMock.reset();
    vi.clearAllMocks();
    museRecheck.mockResolvedValue({
      museChecked: true, museInstalled: false, museVersion: '', museAuthed: false,
    });
    museInstallCommand.mockResolvedValue({
      supported: true,
      cmd: ['bash', '-lc', 'curl -fsSL https://dev.meta.ai/install.sh | sh'],
      display: 'curl -fsSL https://dev.meta.ai/install.sh | sh',
      note: 'Installs the Muse Code CLI (no admin needed).',
    });
  });
  afterEach(() => {
    cleanup();
    window.localStorage.removeItem('solu.hideMuseSetupBanner');
    vi.restoreAllMocks();
  });

  it('shows when the daemon reports muse checked + NOT installed', () => {
    healthValue = health({ museChecked: true, museInstalled: false });
    render(<MuseInstallBanner />);
    expect(screen.getByText(/Muse Code isn’t installed/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Review install$/i })).toBeInTheDocument();
  });

  it('is hidden while the boot probe is still in flight (museChecked false)', () => {
    healthValue = health({ museChecked: false, museInstalled: false });
    render(<MuseInstallBanner />);
    expect(screen.queryByText(/Muse Code isn’t installed/i)).toBeNull();
  });

  it('is hidden once muse is installed AND signed in', () => {
    healthValue = health({ museChecked: true, museInstalled: true, museAuthed: true });
    render(<MuseInstallBanner />);
    expect(screen.queryByText(/Muse Code isn’t installed/i)).toBeNull();
    expect(screen.queryByText(/isn’t signed in/i)).toBeNull();
  });

  it('shows the sign-in nudge when installed but NOT signed in', () => {
    healthValue = health({ museChecked: true, museInstalled: true, museAuthed: false });
    render(<MuseInstallBanner />);
    expect(screen.getByText(/Muse Code isn’t signed in/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Sign in$/i })).toBeInTheDocument();
  });

  it('“Don’t show again” persists across remounts', () => {
    healthValue = health({ museChecked: true, museInstalled: false });
    const { unmount } = render(<MuseInstallBanner />);
    expect(screen.getByText(/Muse Code isn’t installed/i)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /Don’t show again/i }));
    expect(screen.queryByText(/Muse Code isn’t installed/i)).toBeNull();
    expect(window.localStorage.getItem('solu.hideMuseSetupBanner')).toBe('1');

    // A fresh mount (next launch) stays hidden while the flag persists.
    unmount();
    render(<MuseInstallBanner />);
    expect(screen.queryByText(/Muse Code isn’t installed/i)).toBeNull();
  });

  it('stays hidden when the flag was set by an earlier session', () => {
    window.localStorage.setItem('solu.hideMuseSetupBanner', '1');
    healthValue = health({ museChecked: true, museInstalled: false });
    render(<MuseInstallBanner />);
    expect(screen.queryByText(/Muse Code isn’t installed/i)).toBeNull();
  });

  // Dual-mode prompt matrix: each side reflects only its own daemon.
  describe('dual mode', () => {
    beforeEach(() => {
      routerMode = true;
      urlBackend = 'wsl';
    });
    afterEach(() => {
      routerMode = false;
      urlBackend = 'wsl';
    });

    it('PowerShell side routes at WSL instead of a doomed install', () => {
      storeMock.setActiveBackend('win');
      urlBackend = 'win';
      healthValue = health({
        backendID: 'win',
        shellPlatform: 'windows-amd64',
        museChecked: true,
        museInstalled: false,
      });
      render(<MuseInstallBanner />);
      expect(screen.getByText(/Muse Code isn’t installed for PowerShell/i)).toBeInTheDocument();
      expect(screen.getByText(/macOS\/Linux-only; switch to the WSL side/i)).toBeInTheDocument();
    });

    it('unsupported install on the Windows side toasts the WSL route', () => {
      storeMock.setActiveBackend('win');
      urlBackend = 'win';
      museInstallCommand.mockResolvedValue({ supported: false, cmd: [], display: '', note: '' });
      healthValue = health({
        backendID: 'win',
        shellPlatform: 'windows-amd64',
        museChecked: true,
        museInstalled: false,
      });
      render(<MuseInstallBanner />);
      fireEvent.click(screen.getByRole('button', { name: /^Review install$/i }));
      return vi.waitFor(() => {
        expect(storeMock.toast).toHaveBeenCalledWith(
          expect.stringMatching(/switch to the WSL backend/i),
          expect.anything(),
        );
      });
    });

    it('WSL side shows the normal working install prompt', () => {
      storeMock.setActiveBackend('wsl');
      urlBackend = 'wsl';
      healthValue = health({
        backendID: 'wsl',
        shellPlatform: 'linux-amd64',
        museChecked: true,
        museInstalled: false,
      });
      render(<MuseInstallBanner />);
      expect(screen.getByText(/Muse Code isn’t installed for WSL/i)).toBeInTheDocument();
      expect(screen.queryByText(/macOS\/Linux-only/i)).toBeNull();
    });

    it('hides one backend’s prompt while viewing the other', () => {
      storeMock.setActiveBackend('win');
      urlBackend = 'win';
      healthValue = health({
        backendID: 'wsl',
        museChecked: true,
        museInstalled: false,
      });
      render(<MuseInstallBanner />);
      expect(screen.queryByText(/Muse Code isn’t installed/i)).toBeNull();
    });
  });
});
