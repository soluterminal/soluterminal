import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

const refreshDaemonHealth = vi.fn();
vi.mock('./lib/useDaemonHealth', () => ({ refreshDaemonHealth: () => refreshDaemonHealth() }));

const codexRecheck = vi.fn();
vi.mock('./api', () => ({
  api: { codexRecheckOn: (backend: 'wsl' | 'win' | null) => codexRecheck(backend) },
}));

let routerMode = false;
vi.mock('./dualBackend', () => ({ isRouterMode: () => routerMode, activeBackendFromUrl: () => 'wsl' as const }));

const toast = vi.fn();
const createPane = vi.fn();
let panes: Record<string, { title: string }> = {};
vi.mock('./store', () => ({
  useStore: Object.assign(vi.fn(), {
    getState: () => ({ panes, toast, createPane, activeBackend: null }),
  }),
}));

import { scheduleCodexRecheck, openCodexSignIn, CODEX_RECHECK_BACKOFF_MS, CODEX_LOGIN_PANE_TITLE, codexNextStepPending } from './codexBootstrap';

function status(over: Partial<{ codexInstalled: boolean; codexAuthed: boolean }> = {}) {
  return { codexChecked: true, codexInstalled: false, codexVersion: '', codexAuthed: false, ...over };
}

describe('scheduleCodexRecheck', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    codexRecheck.mockReset();
    refreshDaemonHealth.mockClear();
    toast.mockReset();
    createPane.mockReset();
    panes = {};
    routerMode = false;
    window.localStorage.clear();
  });
  afterEach(() => {
    scheduleCodexRecheck('install');
    vi.clearAllTimers();
    vi.useRealTimers();
  });

  it('checks within seconds, then offers sign-in once installed and watches for it', async () => {
    codexRecheck
      .mockResolvedValueOnce(status())
      .mockResolvedValueOnce(status({ codexInstalled: true }))
      .mockResolvedValueOnce(status({ codexInstalled: true, codexAuthed: true }));
    scheduleCodexRecheck('install');
    expect(CODEX_RECHECK_BACKOFF_MS[0]).toBeLessThanOrEqual(5_000);
    await vi.advanceTimersByTimeAsync(CODEX_RECHECK_BACKOFF_MS[0]);
    await vi.advanceTimersByTimeAsync(CODEX_RECHECK_BACKOFF_MS[1]);
    expect(codexRecheck).toHaveBeenCalledTimes(2);
    expect(refreshDaemonHealth).toHaveBeenCalledTimes(2);

    expect(toast).toHaveBeenCalledOnce();
    const [message, opts] = toast.mock.calls[0];
    expect(message).toContain('Codex is installed');
    expect(message).toContain('new pane');
    expect(opts.action.label).toBe('Log in');
    // The lasting banner is armed too.
    expect(codexNextStepPending('local')).toBe(true);

    // Now in the login loop.
    await vi.advanceTimersByTimeAsync(CODEX_RECHECK_BACKOFF_MS[0]);
    expect(codexRecheck).toHaveBeenCalledTimes(3);
    await vi.advanceTimersByTimeAsync(20 * 60_000);
    expect(codexRecheck).toHaveBeenCalledTimes(3);
  });

  it('stops on 403/404 (no recheck on this surface)', async () => {
    codexRecheck.mockRejectedValue(Object.assign(new Error('forbidden'), { status: 403 }));
    scheduleCodexRecheck('install');
    await vi.advanceTimersByTimeAsync(20 * 60_000);
    expect(codexRecheck).toHaveBeenCalledTimes(1);
  });

  it('pins the backend in router mode', async () => {
    routerMode = true;
    codexRecheck.mockResolvedValue(status({ codexInstalled: true, codexAuthed: true }));
    scheduleCodexRecheck('login');
    await vi.advanceTimersByTimeAsync(CODEX_RECHECK_BACKOFF_MS[0]);
    expect(codexRecheck).toHaveBeenCalledWith('wsl');
  });
});

describe('openCodexSignIn', () => {
  beforeEach(() => {
    toast.mockReset();
    createPane.mockReset();
    panes = {};
  });

  it('opens a pane running codex with a plain browser hint', async () => {
    createPane.mockResolvedValue({ id: 'p1' });
    const onSpawn = vi.fn();
    openCodexSignIn(onSpawn);
    await vi.waitFor(() => expect(onSpawn).toHaveBeenCalledOnce());
    expect(createPane).toHaveBeenCalledWith({ cwd: '', cmd: ['codex'], title: CODEX_LOGIN_PANE_TITLE });
    expect(toast.mock.calls[0][0]).toContain('browser window');
    scheduleCodexRecheck('install'); // supersede the login loop it armed
  });

  it('does not open a second sign-in pane', () => {
    panes = { a: { title: CODEX_LOGIN_PANE_TITLE } };
    openCodexSignIn();
    expect(createPane).not.toHaveBeenCalled();
    expect(toast.mock.calls[0][0]).toContain('already open');
  });
});
