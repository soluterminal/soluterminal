import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render, screen, fireEvent, cleanup, act } from '@testing-library/react';

let healthValue: Record<string, unknown> | null = null;
vi.mock('../lib/useDaemonHealth', () => ({
  useDaemonHealth: () => ({ health: healthValue, lastOkMs: 0, polling: false }),
  refreshDaemonHealth: vi.fn(),
}));
vi.mock('../cloudPair', () => ({ isDesktopShell: () => true }));
let routerMode = false;
vi.mock('../dualBackend', () => ({ isRouterMode: () => routerMode, activeBackendFromUrl: () => 'wsl' }));
vi.mock('../api', () => ({ api: { codexRecheckOn: vi.fn() } }));

const storeMock = vi.hoisted(() => {
  const toast = vi.fn();
  const createPane = vi.fn().mockResolvedValue(undefined);
  const state: { activeBackend: 'wsl' | 'win' | null; panes: Record<string, unknown>; toast: unknown; createPane: unknown } = { activeBackend: null, panes: {}, toast, createPane };
  const useStore = vi.fn((sel: (s: typeof state) => unknown) => sel(state)) as ReturnType<typeof vi.fn> & { getState: () => typeof state };
  useStore.getState = () => state;
  return { toast, createPane, state, useStore };
});
vi.mock('../store', () => ({ useStore: storeMock.useStore }));

import { CodexNextStepBanner } from './CodexNextStepBanner';
import { setCodexNextStep, codexNextStepPending } from '../codexBootstrap';

describe('CodexNextStepBanner', () => {
  beforeEach(() => {
    window.localStorage.clear();
    healthValue = { codexChecked: true, codexInstalled: true, codexAuthed: false };
    storeMock.state.panes = {};
    routerMode = false;
    vi.clearAllMocks();
  });
  afterEach(cleanup);

  it('stays away until an install has finished', () => {
    render(<CodexNextStepBanner />);
    expect(screen.queryByText(/Codex is installed/i)).toBeNull();
  });

  it('shows after the install finishes, with a Log in button', () => {
    render(<CodexNextStepBanner />);
    act(() => setCodexNextStep('local', true));
    expect(screen.getByText(/Codex is installed/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Log in$/i })).toBeInTheDocument();
  });

  it('offers Start Codex when already signed in', () => {
    healthValue = { codexChecked: true, codexInstalled: true, codexAuthed: true };
    setCodexNextStep('local', true);
    render(<CodexNextStepBanner />);
    expect(screen.getByRole('button', { name: /^Start Codex$/i })).toBeInTheDocument();
  });

  it('Start Codex opens a plain Codex pane: normal title, no sign-in toast', () => {
    healthValue = { codexChecked: true, codexInstalled: true, codexAuthed: true };
    setCodexNextStep('local', true);
    render(<CodexNextStepBanner />);
    fireEvent.click(screen.getByRole('button', { name: /^Start Codex$/i }));
    expect(storeMock.createPane).toHaveBeenCalledTimes(1);
    const opts = storeMock.createPane.mock.calls[0][0];
    expect(opts.cmd).toEqual(['codex']);
    expect(opts.title).toBe('Codex');
    expect(storeMock.toast).not.toHaveBeenCalled();
  });

  it('opens a Codex pane when the button is pressed', () => {
    setCodexNextStep('local', true);
    render(<CodexNextStepBanner />);
    fireEvent.click(screen.getByRole('button', { name: /^Log in$/i }));
    expect(storeMock.createPane).toHaveBeenCalledWith(expect.objectContaining({ cmd: ['codex'], title: 'Sign in to Codex' }));
  });

  it('hides, and stays hidden, once a Codex pane is running', () => {
    setCodexNextStep('local', true);
    storeMock.state.panes = { p1: { status: 'running', cmd: ['codex'] } };
    render(<CodexNextStepBanner />);
    expect(screen.queryByText(/Codex is installed/i)).toBeNull();
    expect(codexNextStepPending('local')).toBe(false);
  });

  it('is not hidden by the installer pane or an exited Codex pane', () => {
    setCodexNextStep('local', true);
    storeMock.state.panes = {
      a: { status: 'running', cmd: ['bash', '-lc', 'curl install'], title: 'Install Codex' },
      b: { status: 'exited', cmd: ['codex'] },
    };
    render(<CodexNextStepBanner />);
    expect(screen.getByText(/Codex is installed/i)).toBeInTheDocument();
  });

  it('dismiss sticks across a remount', () => {
    setCodexNextStep('local', true);
    const { unmount } = render(<CodexNextStepBanner />);
    fireEvent.click(screen.getByRole('button', { name: /^Dismiss$/i }));
    expect(screen.queryByText(/Codex is installed/i)).toBeNull();
    unmount();
    render(<CodexNextStepBanner />);
    expect(screen.queryByText(/Codex is installed/i)).toBeNull();
  });

  it('belongs to the installed side in Dual mode', () => {
    routerMode = true;
    setCodexNextStep('win', true);
    storeMock.state.panes = { p1: { status: 'running', cmd: ['codex'] } };
    render(<CodexNextStepBanner />);
    // Active side is WSL: nothing shown, and WSL's running Codex pane must not
    // clear the PowerShell side's step.
    expect(screen.queryByText(/Codex is installed/i)).toBeNull();
    expect(codexNextStepPending('win')).toBe(true);
    // Both sides pending at once: one does not overwrite the other.
    setCodexNextStep('wsl', true);
    expect(codexNextStepPending('win')).toBe(true);
    expect(codexNextStepPending('wsl')).toBe(true);
  });
});
