import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

// --- Mocks ---------------------------------------------------------------
const refreshDaemonHealth = vi.fn();
vi.mock('./lib/useDaemonHealth', () => ({
  refreshDaemonHealth: () => refreshDaemonHealth(),
}));

const claudeRecheck = vi.fn();
vi.mock('./api', () => ({
  api: {
    claudeRecheck: () => claudeRecheck(),
    claudeRecheckOn: (backend: 'wsl' | 'win' | null) => claudeRecheck(backend),
  },
}));

// Router-mode toggle per test (single-backend by default).
let routerMode = false;
vi.mock('./dualBackend', () => ({
  isRouterMode: () => routerMode,
  activeBackendFromUrl: () => 'wsl' as const,
}));

// Keep the real (heavy) store out of the module graph; the scheduler only
// reads activeBackend from it (at schedule time, for the router-mode pin).
let storeActiveBackend: 'wsl' | 'win' | null = null;
vi.mock('./store', () => ({
  useStore: Object.assign(vi.fn(), {
    getState: () => ({
      panes: {}, toast: vi.fn(), createPane: vi.fn(), activeBackend: storeActiveBackend,
    }),
  }),
}));

import { scheduleClaudeRecheck, CLAUDE_RECHECK_BACKOFF_MS } from './claudeBootstrap';
import { agentNextStepPending } from './lib/agentNextStep';

function status(over: Partial<{
  claudeChecked: boolean; claudeInstalled: boolean; claudeVersion: string; claudeAuthed: boolean;
}> = {}) {
  return { claudeChecked: true, claudeInstalled: false, claudeVersion: '', claudeAuthed: false, ...over };
}

// scheduleClaudeRecheck closes the post-install blind window: without it the
// daemon's cached probe refreshes only on its 5-minute ticker, so a user who
// just installed/signed in saw no UI change (and Claude panes dead-ended)
// until the next tick. These tests pin the backoff loop's contract.
describe('scheduleClaudeRecheck', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    claudeRecheck.mockReset();
    refreshDaemonHealth.mockClear();
    routerMode = false;
    storeActiveBackend = null;
    window.localStorage.clear();
  });
  afterEach(() => {
    // Supersede any loop the test left running, THEN drop fake timers.
    scheduleClaudeRecheck('install');
    vi.clearAllTimers();
    vi.useRealTimers();
  });

  it('polls on the backoff and stops once the install lands (authed too)', async () => {
    claudeRecheck
      .mockResolvedValueOnce(status()) // still not installed
      .mockResolvedValueOnce(status({ claudeInstalled: true, claudeAuthed: true }));
    scheduleClaudeRecheck('install');

    expect(claudeRecheck).not.toHaveBeenCalled(); // nothing before the first delay
    await vi.advanceTimersByTimeAsync(CLAUDE_RECHECK_BACKOFF_MS[0]);
    expect(claudeRecheck).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(CLAUDE_RECHECK_BACKOFF_MS[1]);
    expect(claudeRecheck).toHaveBeenCalledTimes(2);
    // Fresh probe pushed into the shared health poll on every tick.
    expect(refreshDaemonHealth).toHaveBeenCalledTimes(2);

    // Done → no further polling no matter how long we wait.
    await vi.advanceTimersByTimeAsync(20 * 60_000);
    expect(claudeRecheck).toHaveBeenCalledTimes(2);
    // The lasting start card is armed for this side.
    expect(agentNextStepPending('local')).toBe(true);
  });

  it('arms no start card from a sign-in watch or an unfinished install', async () => {
    claudeRecheck.mockResolvedValue(status({ claudeInstalled: true, claudeAuthed: true }));
    scheduleClaudeRecheck('login');
    await vi.advanceTimersByTimeAsync(CLAUDE_RECHECK_BACKOFF_MS[0]);
    expect(agentNextStepPending('local')).toBe(false);
    claudeRecheck.mockResolvedValue(status());
    scheduleClaudeRecheck('install');
    await vi.advanceTimersByTimeAsync(CLAUDE_RECHECK_BACKOFF_MS[0]);
    expect(agentNextStepPending('local')).toBe(false);
  });

  it('chains install → login watch when the install lands unauthenticated', async () => {
    claudeRecheck
      .mockResolvedValueOnce(status({ claudeInstalled: true })) // installed, not signed in
      .mockResolvedValueOnce(status({ claudeInstalled: true, claudeAuthed: true }));
    scheduleClaudeRecheck('install');

    await vi.advanceTimersByTimeAsync(CLAUDE_RECHECK_BACKOFF_MS[0]);
    expect(claudeRecheck).toHaveBeenCalledTimes(1);
    // The chained login loop restarts the backoff from the top.
    await vi.advanceTimersByTimeAsync(CLAUDE_RECHECK_BACKOFF_MS[0]);
    expect(claudeRecheck).toHaveBeenCalledTimes(2);
    // Authed now → chain is done.
    await vi.advanceTimersByTimeAsync(20 * 60_000);
    expect(claudeRecheck).toHaveBeenCalledTimes(2);
  });

  it('a newer schedule supersedes the older loop', async () => {
    claudeRecheck.mockResolvedValue(status());
    scheduleClaudeRecheck('install');
    scheduleClaudeRecheck('login'); // takes over before any timer fires

    await vi.advanceTimersByTimeAsync(CLAUDE_RECHECK_BACKOFF_MS[0]);
    // The superseded install loop's timer fired but did nothing; only the
    // login loop actually probed.
    expect(claudeRecheck).toHaveBeenCalledTimes(1);
  });

  it('keeps polling through a failed recheck (transient daemon hiccup)', async () => {
    claudeRecheck
      .mockRejectedValueOnce(new Error('daemon restarting'))
      .mockResolvedValueOnce(status({ claudeInstalled: true, claudeAuthed: true }));
    scheduleClaudeRecheck('install');

    await vi.advanceTimersByTimeAsync(CLAUDE_RECHECK_BACKOFF_MS[0]);
    expect(claudeRecheck).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(CLAUDE_RECHECK_BACKOFF_MS[1]);
    expect(claudeRecheck).toHaveBeenCalledTimes(2);
  });

  it('gives up after the backoff table is exhausted (abandoned install)', async () => {
    claudeRecheck.mockResolvedValue(status()); // never completes
    scheduleClaudeRecheck('install');

    const total = CLAUDE_RECHECK_BACKOFF_MS.reduce((a, b) => a + b, 0);
    await vi.advanceTimersByTimeAsync(total + 60_000);
    expect(claudeRecheck).toHaveBeenCalledTimes(CLAUDE_RECHECK_BACKOFF_MS.length);
  });

  it('stops immediately on 403/404 (relay surface / older daemon)', async () => {
    const err = Object.assign(new Error('forbidden'), { status: 403 });
    claudeRecheck.mockRejectedValue(err);
    scheduleClaudeRecheck('login');

    await vi.advanceTimersByTimeAsync(20 * 60_000);
    expect(claudeRecheck).toHaveBeenCalledTimes(1); // no doomed retries
  });

  it('pins the backend it was armed for across a mid-loop switch (dual mode)', async () => {
    routerMode = true;
    storeActiveBackend = 'wsl';
    claudeRecheck
      .mockResolvedValueOnce(status({ claudeInstalled: true })) // install done → chains login
      .mockResolvedValueOnce(status({ claudeInstalled: true, claudeAuthed: true }));
    scheduleClaudeRecheck('install');

    // The user flips to the Windows side while the loop is waiting — the loop
    // must keep probing the WSL daemon it was armed for.
    storeActiveBackend = 'win';
    await vi.advanceTimersByTimeAsync(CLAUDE_RECHECK_BACKOFF_MS[0]);
    expect(claudeRecheck).toHaveBeenLastCalledWith('wsl');
    // The chained login watch inherits the same pin.
    await vi.advanceTimersByTimeAsync(CLAUDE_RECHECK_BACKOFF_MS[0]);
    expect(claudeRecheck).toHaveBeenLastCalledWith('wsl');
    expect(claudeRecheck).toHaveBeenCalledTimes(2);
    // The start card belongs to the side that was installed.
    expect(agentNextStepPending('wsl')).toBe(true);
    expect(agentNextStepPending('win')).toBe(false);
  });
});
