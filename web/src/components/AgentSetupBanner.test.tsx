import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react';
import type { Health } from '../types';

// --- Mocks ---------------------------------------------------------------
// Controllable daemon-health value per test.
let healthValue: Health | null = null;
const refreshDaemonHealth = vi.fn();
vi.mock('../lib/useDaemonHealth', () => ({
  useDaemonHealth: () => ({ health: healthValue, lastOkMs: 0, polling: false }),
  getDaemonHealthState: () => ({ health: healthValue, lastOkMs: 0, polling: false }),
  refreshDaemonHealth: () => refreshDaemonHealth(),
}));

let desktop = true;
vi.mock('../cloudPair', () => ({
  isDesktopShell: () => desktop,
}));

const claudeRecheck = vi.fn().mockResolvedValue({
  claudeChecked: true, claudeInstalled: false, claudeVersion: '', claudeAuthed: false,
});
const claudeInstallCommand = vi.fn().mockResolvedValue({
  supported: true,
  cmd: ['bash', '-lc', 'curl -fsSL https://claude.ai/install.sh | bash'],
  display: 'curl -fsSL https://claude.ai/install.sh | bash',
  note: 'Installs the Claude Code CLI (no admin needed).',
});
vi.mock('../api', () => ({
  api: {
    claudeRecheck: () => claudeRecheck(),
    claudeInstallCommand: () => claudeInstallCommand(),
    // The sign-in panes start a recheck loop; a 404 ends it (older daemon).
    claudeRecheckOn: () => Promise.reject(Object.assign(new Error('not found'), { status: 404 })),
    codexRecheckOn: () => Promise.reject(Object.assign(new Error('not found'), { status: 404 })),
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
  const setPanes = (panes: Record<string, unknown>) => {
    state = { ...state, panes };
  };
  return { toast, createPane, useStore, reset, setActiveBackend, setPanes };
});
vi.mock('../store', () => ({
  useStore: storeMock.useStore,
}));

import { AgentSetupBanner } from './AgentSetupBanner';
import { useEntitlements } from '../entitlements';
import { setExtensionsForTests } from '../extensions';
import { agentNextStepPending, firstAgentStarted, setAgentNextStep } from '../lib/agentNextStep';
import { hideSetupBanner } from '../lib/setupBannerPrefs';

function health(over: Partial<Health>): Health {
  return { status: 'ok', version: '0.0.0', ...over } as Health;
}

describe('AgentSetupBanner', () => {
  beforeEach(() => {
    window.history.replaceState(null, '', '/');
    window.localStorage.clear();
    healthValue = null;
    desktop = true;
    storeMock.reset();
    vi.clearAllMocks();
    claudeRecheck.mockResolvedValue({
      claudeChecked: true, claudeInstalled: false, claudeVersion: '', claudeAuthed: false,
    });
    claudeInstallCommand.mockResolvedValue({
      supported: true,
      cmd: ['bash', '-lc', 'curl -fsSL https://claude.ai/install.sh | bash'],
      display: 'curl -fsSL https://claude.ai/install.sh | bash',
      note: 'Installs the Claude Code CLI (no admin needed).',
    });
  });
  afterEach(() => vi.restoreAllMocks());

  it('shows when the daemon reports claude checked + NOT installed', () => {
    healthValue = health({ claudeChecked: true, claudeInstalled: false });
    render(<AgentSetupBanner />);
    expect(screen.getByText(/Claude Code isn’t installed yet\./i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Install$/i })).toBeInTheDocument();
    // Plain words only: no command names, jargon or backticks.
    expect(document.body.textContent).not.toMatch(/CLI|this side|`/);
  });

  it('stays hidden while the setup checklist shows the same step', () => {
    healthValue = health({ claudeChecked: true, claudeInstalled: false });
    render(<AgentSetupBanner hidden />);
    expect(screen.queryByText(/Claude Code isn’t installed/i)).toBeNull();
  });

  it('is hidden while the boot probe is still in flight (claudeChecked false)', () => {
    healthValue = health({ claudeChecked: false, claudeInstalled: false });
    render(<AgentSetupBanner />);
    expect(screen.queryByText(/Claude Code isn’t installed/i)).toBeNull();
  });

  it('is hidden once claude is installed AND signed in', () => {
    healthValue = health({ claudeChecked: true, claudeInstalled: true, claudeAuthed: true });
    render(<AgentSetupBanner />);
    expect(screen.queryByText(/Claude Code isn’t installed/i)).toBeNull();
    expect(screen.queryByText(/isn’t signed in/i)).toBeNull();
  });

  it('shows the sign-in nudge when installed but NOT signed in', () => {
    healthValue = health({ backendID: 'win', claudeChecked: true, claudeInstalled: true, claudeAuthed: false });
    render(<AgentSetupBanner />);
    expect(screen.getByText(/Claude Code isn’t signed in/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Log in$/i })).toBeInTheDocument();
    // Not the install state.
    expect(screen.queryByText(/isn’t installed/i)).toBeNull();
  });

  it('names the active backend in dual mode', () => {
    window.history.replaceState(null, '', '/?solu_router=1&b=win');
    storeMock.setActiveBackend('win');
    healthValue = health({ backendID: 'win', claudeChecked: true, claudeInstalled: true, claudeAuthed: false });

    render(<AgentSetupBanner />);

    expect(screen.getByText(/Claude Code isn’t signed in yet in Windows PowerShell\./i)).toBeInTheDocument();
    expect(screen.getByText(/Linux \(WSL\) and Windows PowerShell each need their own sign-in/i)).toBeInTheDocument();
  });

  it('does not show stale WSL health while PowerShell is active', () => {
    window.history.replaceState(null, '', '/?solu_router=1&b=win');
    storeMock.setActiveBackend('win');
    healthValue = health({
      backendID: 'wsl',
      claudeChecked: true,
      claudeInstalled: false,
      claudeAuthed: false,
    });

    render(<AgentSetupBanner />);

    expect(screen.queryByText(/Claude Code isn’t installed/i)).toBeNull();
    expect(screen.queryByText(/PowerShell/i)).toBeNull();
  });

  it('dismisses only the active backend need for the session', () => {
    window.history.replaceState(null, '', '/?solu_router=1&b=wsl');
    storeMock.setActiveBackend('wsl');
    healthValue = health({
      backendID: 'wsl',
      claudeChecked: true,
      claudeInstalled: false,
      claudeAuthed: false,
    });
    const { rerender } = render(<AgentSetupBanner />);
    fireEvent.click(screen.getByRole('button', { name: /Dismiss/i }));
    expect(screen.queryByText(/Claude Code isn’t installed/i)).toBeNull();

    window.history.replaceState(null, '', '/?solu_router=1&b=win');
    storeMock.setActiveBackend('win');
    healthValue = health({
      backendID: 'win',
      claudeChecked: true,
      claudeInstalled: false,
      claudeAuthed: false,
    });
    rerender(<AgentSetupBanner />);

    expect(screen.getByText(/Claude Code isn’t installed yet in Windows PowerShell/i)).toBeInTheDocument();
  });

  it('Log in spawns a sign-in pane and dismisses', async () => {
    healthValue = health({ claudeChecked: true, claudeInstalled: true, claudeAuthed: false });
    render(<AgentSetupBanner />);
    fireEvent.click(screen.getByRole('button', { name: /^Log in$/i }));
    await waitFor(() =>
      expect(storeMock.createPane).toHaveBeenCalledWith(
        expect.objectContaining({ title: 'Sign in to Claude', cmd: ['claude'] }),
      ),
    );
  });

  it('is hidden outside the desktop shell (phone/relay web)', () => {
    desktop = false;
    healthValue = health({ claudeChecked: true, claudeInstalled: false });
    render(<AgentSetupBanner />);
    expect(screen.queryByText(/Claude Code isn’t installed/i)).toBeNull();
  });

  it('is hidden on an older daemon that predates claude detection (fields absent)', () => {
    healthValue = health({}); // claudeChecked === undefined
    render(<AgentSetupBanner />);
    expect(screen.queryByText(/Claude Code isn’t installed/i)).toBeNull();
  });

  it('Install discloses the exact command in a confirm card before running it', async () => {
    healthValue = health({ claudeChecked: true, claudeInstalled: false });
    render(<AgentSetupBanner />);

    fireEvent.click(screen.getByRole('button', { name: /^Install$/i }));

    // No pane spawned yet — the confirm card appears first with the command.
    await waitFor(() => expect(screen.getByText(/Run Claude Code installer\?/i)).toBeInTheDocument());
    expect(screen.getByText(/curl -fsSL https:\/\/claude\.ai\/install\.sh \| bash/)).toBeInTheDocument();
    expect(storeMock.createPane).not.toHaveBeenCalled();

    // Confirming spawns the installer pane.
    fireEvent.click(screen.getByRole('button', { name: /Run installer in new pane/i }));
    await waitFor(() =>
      expect(storeMock.createPane).toHaveBeenCalledWith(
        expect.objectContaining({ title: 'Install Claude Code', cmd: expect.arrayContaining(['bash']) }),
      ),
    );
  });

  it('only points team installs to Credentials when hiding for good', () => {
    healthValue = health({ claudeChecked: true, claudeInstalled: false });
    useEntitlements.setState({ loaded: true, team: false, operator: false });
    const { unmount } = render(<AgentSetupBanner />);
    const hide = screen.getByRole('button', { name: 'Don’t show again' });
    expect(hide.getAttribute('title')).not.toMatch(/Credentials/);
    unmount();
    useEntitlements.setState({ loaded: true, team: true, operator: false });
    // A build without the Credentials section has nowhere to point.
    setExtensionsForTests([]);
    const second = render(<AgentSetupBanner />);
    expect(screen.getByRole('button', { name: 'Don’t show again' }).getAttribute('title')).not.toMatch(/Credentials/);
    second.unmount();
    setExtensionsForTests([{ sidebarSections: { credentials: () => null } }]);
    render(<AgentSetupBanner />);
    expect(screen.getByRole('button', { name: 'Don’t show again' }).getAttribute('title')).toMatch(/Credentials/);
    setExtensionsForTests(null);
    useEntitlements.setState({ loaded: false, team: false, operator: false });
  });

  it('× dismisses the banner for the session', () => {
    healthValue = health({ claudeChecked: true, claudeInstalled: false });
    render(<AgentSetupBanner />);
    expect(screen.getByText(/Claude Code isn’t installed/i)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /Dismiss/i }));
    expect(screen.queryByText(/Claude Code isn’t installed/i)).toBeNull();
  });

  it('forces a fresh probe + health refresh on mount (desktop)', async () => {
    healthValue = health({ claudeChecked: true, claudeInstalled: false });
    render(<AgentSetupBanner />);
    await waitFor(() => expect(refreshDaemonHealth).toHaveBeenCalled());
    expect(claudeRecheck).toHaveBeenCalled();
  });

  describe('start card after an agent install', () => {
    const START = /Next, start your first agent/;
    const bothReady = () => health({
      claudeChecked: true, claudeInstalled: true, claudeAuthed: true,
      codexChecked: true, codexInstalled: true, codexAuthed: true,
    });

    it('stays away until an install has finished', () => {
      healthValue = bothReady();
      render(<AgentSetupBanner />);
      expect(screen.queryByText(START)).toBeNull();
    });

    it('shows once an install finishes, with a Start button per installed agent', () => {
      healthValue = bothReady();
      render(<AgentSetupBanner />);
      act(() => setAgentNextStep('local', true));
      expect(screen.getByText(START)).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Start Claude Code' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Start Codex' })).toBeInTheDocument();
    });

    it('offers only Codex, ahead of the Claude install nudge, when Claude is missing', () => {
      healthValue = health({ claudeChecked: true, claudeInstalled: false, codexChecked: true, codexInstalled: true });
      setAgentNextStep('local', true);
      render(<AgentSetupBanner />);
      expect(screen.getByRole('button', { name: 'Start Codex' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Start Claude Code' })).toBeNull();
      expect(screen.queryByText(/Claude Code isn’t installed/i)).toBeNull();
    });

    it('shows nothing to start when the agent went away again', () => {
      healthValue = health({ claudeChecked: true, claudeInstalled: false, codexChecked: true, codexInstalled: false });
      setAgentNextStep('local', true);
      render(<AgentSetupBanner />);
      expect(screen.queryByText(START)).toBeNull();
      // The Claude install nudge is back in its place.
      expect(screen.getByText(/Claude Code isn’t installed yet\./i)).toBeInTheDocument();
    });

    it('honours a Codex step armed by an older build', () => {
      healthValue = bothReady();
      window.localStorage.setItem('solu.codexNextStep.local', '1');
      render(<AgentSetupBanner />);
      expect(screen.getByText(START)).toBeInTheDocument();
    });

    it('Start Claude Code opens a Claude pane when signed in', async () => {
      healthValue = bothReady();
      setAgentNextStep('local', true);
      render(<AgentSetupBanner />);
      fireEvent.click(screen.getByRole('button', { name: 'Start Claude Code' }));
      await waitFor(() =>
        expect(storeMock.createPane).toHaveBeenCalledWith(expect.objectContaining({ cmd: ['claude'] })),
      );
      expect(storeMock.createPane).toHaveBeenCalledTimes(1);
    });

    it('a signed-out agent starts in its sign-in pane', async () => {
      healthValue = health({ claudeChecked: true, claudeInstalled: true, claudeAuthed: true, codexChecked: true, codexInstalled: true, codexAuthed: false });
      setAgentNextStep('local', true);
      render(<AgentSetupBanner />);
      fireEvent.click(screen.getByRole('button', { name: 'Start Codex' }));
      await waitFor(() =>
        expect(storeMock.createPane).toHaveBeenCalledWith(
          expect.objectContaining({ cmd: ['codex'], title: 'Sign in to Codex' }),
        ),
      );
    });

    it('is gone for good once an agent pane has run', () => {
      healthValue = bothReady();
      setAgentNextStep('local', true);
      storeMock.setPanes({ p1: { status: 'running', cmd: ['codex'] } });
      const { unmount } = render(<AgentSetupBanner />);
      expect(screen.queryByText(START)).toBeNull();
      expect(firstAgentStarted()).toBe(true);
      unmount();
      // The pane closed, and another install finished later: still no card.
      storeMock.setPanes({});
      setAgentNextStep('local', true);
      render(<AgentSetupBanner />);
      expect(screen.queryByText(START)).toBeNull();
      expect(agentNextStepPending('local')).toBe(false);
    });

    it('is not retired by the installer pane or an exited agent pane', () => {
      healthValue = bothReady();
      setAgentNextStep('local', true);
      storeMock.setPanes({
        a: { status: 'running', cmd: ['bash', '-lc', 'curl install'], title: 'Install Codex' },
        b: { status: 'exited', cmd: ['claude'] },
      });
      render(<AgentSetupBanner />);
      expect(screen.getByText(START)).toBeInTheDocument();
      expect(firstAgentStarted()).toBe(false);
    });

    it('× hides it for the session only', () => {
      healthValue = bothReady();
      setAgentNextStep('local', true);
      const { unmount } = render(<AgentSetupBanner />);
      fireEvent.click(screen.getByRole('button', { name: /Dismiss/i }));
      expect(screen.queryByText(START)).toBeNull();
      unmount();
      render(<AgentSetupBanner />);
      expect(screen.getByText(START)).toBeInTheDocument();
    });

    it('is not silenced by the Claude banner’s Don’t show again', () => {
      healthValue = bothReady();
      hideSetupBanner('claude');
      setAgentNextStep('local', true);
      render(<AgentSetupBanner />);
      expect(screen.getByText(START)).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Don’t show again' })).toBeNull();
    });

    it('belongs to the installed side in Dual mode', () => {
      window.history.replaceState(null, '', '/?solu_router=1&b=win');
      storeMock.setActiveBackend('win');
      healthValue = health({ ...bothReady(), backendID: 'win' });
      setAgentNextStep('wsl', true);
      const { unmount } = render(<AgentSetupBanner />);
      expect(screen.queryByText(START)).toBeNull();
      unmount();
      setAgentNextStep('win', true);
      render(<AgentSetupBanner />);
      expect(screen.getByText(/Next, start your first agent in Windows PowerShell\./)).toBeInTheDocument();
    });
  });
});
