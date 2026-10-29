import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError } from './api';
import { useCloud } from './cloud';
import {
  noteUserActivity,
  SHELL_AUTO_APP_NOT_FOUND_PER_DAY,
  SHELL_AUTO_APP_NOT_FOUND_RETRY_MS,
  SHELL_AUTO_COUNTDOWN_MS,
  SHELL_AUTO_HOLD_MS,
  SHELL_AUTO_QUIET_MS,
  shellAutoApplyNeedsManual,
  shellAutoApplyWaitReason,
  shellAutoWaitLine,
  useStore,
} from './store';
import type { PaneInfo, UpdateStatus } from './types';
import { reportUnsentInput, resetUnsentInputForTests } from './lib/unsentInput';

const native = vi.hoisted(() => ({
  desktop: true,
  installed: '1.0.0' as string | null,
  outcome: { ok: true } as { ok: boolean; staged?: string; error?: string },
  apply: vi.fn(),
  exit: vi.fn(),
  appPid: null as number | null,
}));
const drafts = vi.hoisted(() => ({ present: false, paneId: undefined as string | undefined }));

vi.mock('./tauri-browser', () => ({
  isDesktopApp: () => native.desktop,
  getShellVersion: () => Promise.resolve(native.installed),
  getAppProcessId: () => Promise.resolve(native.appPid),
  applyShellUpdateSilentNative: () => {
    native.apply();
    return Promise.resolve(native.outcome);
  },
  exitForUpdateNative: () => {
    native.exit();
    return Promise.resolve(true);
  },
}));

vi.mock('./native/useNativePane', () => ({
  hasAnyUnsentDraft: () => drafts.present,
  unsentDraftPaneId: () => (drafts.present ? drafts.paneId ?? 'draft-pane' : undefined),
}));

const pane = (over: Partial<PaneInfo> & { id: string }): PaneInfo => over as PaneInfo;
let versionSerial = 0;
let version = '';

function update(over: Partial<UpdateStatus> = {}): UpdateStatus {
  return {
    current: '1.0.0',
    available: false,
    shellLatest: version,
    shellCarriesLatestDaemon: true,
    shellAutoApplyPaused: false,
    ...over,
  };
}

async function becomeQuiet(): Promise<void> {
  await vi.advanceTimersByTimeAsync(SHELL_AUTO_QUIET_MS + 5);
}

async function arm(): Promise<void> {
  useStore.getState().maybeAutoApplyShellUpdate();
  await vi.advanceTimersByTimeAsync(0);
  expect(useStore.getState().shellAutoRestart?.version).toBe(version);
}

describe('desktop shell auto-apply', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    version = `2.0.${++versionSerial}`;
    // Module-level activity time intentionally survives store resets. Keep fake
    // clocks monotonic across cases so a prior test cannot appear "in future".
    vi.setSystemTime(1_900_000_000_000 + versionSerial * 24 * 60 * 60 * 1000);
    native.desktop = true;
    native.installed = '1.0.0';
    native.outcome = { ok: true };
    native.apply.mockClear();
    native.exit.mockClear();
    drafts.present = false;
    drafts.paneId = undefined;
    native.appPid = null;
    resetUnsentInputForTests();
    delete (window as unknown as { __SOLU_BACKEND__?: unknown }).__SOLU_BACKEND__;
    useCloud.setState({ mode: 'local', isLocal: false });
    noteUserActivity();
    useStore.setState({
      panes: {},
      deferredUpdate: null,
      updateSettings: { autoApply: 'off', shellAutoApply: 'when-idle' },
      updateStatus: update(),
      shellAutoRestart: null,
      toasts: [],
    });
    vi.spyOn(api, 'getUpdateStatus').mockImplementation(async () => update());
  });

  afterEach(async () => {
    noteUserActivity();
    await vi.advanceTimersByTimeAsync(1000);
    useStore.setState({ panes: {}, updateStatus: null, shellAutoRestart: null });
    localStorage.clear();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('arms when quiet, silently installs at the deadline, and exits for relaunch', async () => {
    await becomeQuiet();
    await arm();
    expect(native.apply).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(SHELL_AUTO_COUNTDOWN_MS);
    expect(native.apply).toHaveBeenCalledTimes(1);
    expect(useStore.getState().updateStatus?.shellApplying).toBe(true);
    await vi.advanceTimersByTimeAsync(1500);
    expect(native.exit).toHaveBeenCalledTimes(1);
  });

  it('keeps manual behavior when the preference is off', async () => {
    useStore.setState({ updateSettings: { autoApply: 'off', shellAutoApply: 'off' } });
    await becomeQuiet();
    useStore.getState().maybeAutoApplyShellUpdate();
    await vi.advanceTimersByTimeAsync(SHELL_AUTO_COUNTDOWN_MS + 1000);
    expect(useStore.getState().shellAutoRestart).toBeNull();
    expect(native.apply).not.toHaveBeenCalled();
  });

  it.each([
    ['native pane mid-turn', () => useStore.setState({ panes: { p: pane({ id: 'p', kind: 'native', agentMidTurn: true }) } })],
    ['pending approval', () => useStore.setState({ panes: { p: pane({ id: 'p', pendingApproval: { requestId: 'r', toolName: 'Bash' } }) } })],
    ['unanswered prompt', () => useStore.setState({ panes: { p: pane({ id: 'p', attention: 'needs-user' }) } })],
    ['workflow invocation', () => useStore.setState({ updateStatus: update({ workflowInflight: 1 }) })],
    ['workflow drain', () => useStore.setState({ updateStatus: update({ workflowDeferred: true }) })],
    ['unsent draft', () => { drafts.present = true; }],
  ])('does not arm with the %s hold', async (_name, hold) => {
    await becomeQuiet();
    hold();
    useStore.getState().maybeAutoApplyShellUpdate();
    await vi.advanceTimersByTimeAsync(0);
    expect(useStore.getState().shellAutoRestart).toBeNull();
  });

  it('does not arm after recent keyboard or pointer activity', async () => {
    await becomeQuiet();
    noteUserActivity();
    useStore.getState().maybeAutoApplyShellUpdate();
    await vi.advanceTimersByTimeAsync(0);
    expect(useStore.getState().shellAutoRestart).toBeNull();
  });

  it('activity during countdown disarms and a later status refresh re-arms after fresh quiet', async () => {
    await becomeQuiet();
    await arm();
    noteUserActivity();
    await vi.advanceTimersByTimeAsync(1000);
    expect(useStore.getState().shellAutoRestart).toBeNull();

    await becomeQuiet();
    await useStore.getState().refreshUpdateStatus();
    await vi.advanceTimersByTimeAsync(0);
    expect(useStore.getState().shellAutoRestart?.version).toBe(version);
  });

  it('Hold 1 hour persists the per-version deferral until it expires', async () => {
    await becomeQuiet();
    await arm();
    useStore.getState().postponeShellAutoRestart();
    expect(useStore.getState().shellAutoRestart).toBeNull();

    await vi.advanceTimersByTimeAsync(SHELL_AUTO_HOLD_MS - 1);
    useStore.getState().maybeAutoApplyShellUpdate();
    await vi.advanceTimersByTimeAsync(0);
    expect(useStore.getState().shellAutoRestart).toBeNull();

    await vi.advanceTimersByTimeAsync(1);
    await arm();
  });

  it('Restart now bypasses the remaining countdown', async () => {
    await becomeQuiet();
    await arm();
    await useStore.getState().restartShellNowForUpdate();
    expect(native.apply).toHaveBeenCalledTimes(1);
    expect(useStore.getState().shellAutoRestart).toBeNull();
  });

  it('old-shell failure never exits and the cooldown plus two-attempt cap prevent a loop', async () => {
    native.outcome = { ok: false, error: 'unknown command' };
    await becomeQuiet();
    await arm();
    await useStore.getState().restartShellNowForUpdate();
    expect(native.apply).toHaveBeenCalledTimes(1);
    expect(native.exit).not.toHaveBeenCalled();

    useStore.getState().maybeAutoApplyShellUpdate();
    await vi.advanceTimersByTimeAsync(0);
    expect(useStore.getState().shellAutoRestart).toBeNull();

    await vi.advanceTimersByTimeAsync(6 * 60 * 60 * 1000);
    await arm();
    await useStore.getState().restartShellNowForUpdate();
    expect(native.apply).toHaveBeenCalledTimes(2);

    await vi.advanceTimersByTimeAsync(6 * 60 * 60 * 1000);
    useStore.getState().maybeAutoApplyShellUpdate();
    await vi.advanceTimersByTimeAsync(0);
    expect(useStore.getState().shellAutoRestart).toBeNull();
    expect(native.apply).toHaveBeenCalledTimes(2);
  });

  // A local daemon with sign-in serves /api/config, so a signed-in official
  // install reports useCloud mode 'cloud' with isLocal. That is this computer's
  // daemon, and the countdown must arm (it never did before).
  it('arms on a signed-in local install', async () => {
    useCloud.setState({ mode: 'cloud', isLocal: true });
    await becomeQuiet();
    await arm();
    await vi.advanceTimersByTimeAsync(SHELL_AUTO_COUNTDOWN_MS);
    expect(native.apply).toHaveBeenCalledTimes(1);
  });

  it('never arms over a relay', async () => {
    useCloud.setState({ mode: 'cloud', isLocal: false });
    await becomeQuiet();
    useStore.getState().maybeAutoApplyShellUpdate();
    await vi.advanceTimersByTimeAsync(0);
    expect(useStore.getState().shellAutoRestart).toBeNull();
  });

  it('never arms in cloud launch mode', async () => {
    (window as unknown as { __SOLU_BACKEND__?: unknown }).__SOLU_BACKEND__ = { launchMode: 'cloud' };
    useCloud.setState({ mode: 'cloud', isLocal: true });
    await becomeQuiet();
    useStore.getState().maybeAutoApplyShellUpdate();
    await vi.advanceTimersByTimeAsync(0);
    expect(useStore.getState().shellAutoRestart).toBeNull();
  });

  it.each([
    ['a native pane draft', (on: boolean) => { drafts.present = on; }],
    ['another compose box', (on: boolean) => reportUnsentInput('broadcast', on ? 'tell every pane' : '')],
  ])('waits while %s holds unsent text and arms once it is sent or cleared', async (_name, setDraft) => {
    setDraft(true);
    await becomeQuiet();
    await useStore.getState().refreshUpdateStatus();
    await vi.advanceTimersByTimeAsync(0);
    expect(useStore.getState().shellAutoRestart).toBeNull();

    setDraft(false);
    await useStore.getState().refreshUpdateStatus();
    await vi.advanceTimersByTimeAsync(0);
    expect(useStore.getState().shellAutoRestart?.version).toBe(version);
  });

  it('text typed during the countdown cancels it before anything installs', async () => {
    await becomeQuiet();
    await arm();
    reportUnsentInput('inbox-reply', 'half a reply');
    await vi.advanceTimersByTimeAsync(SHELL_AUTO_COUNTDOWN_MS + 1000);
    expect(useStore.getState().shellAutoRestart).toBeNull();
    expect(native.apply).not.toHaveBeenCalled();
  });

  it('Restart now with unsent text says why it held instead of vanishing', async () => {
    await becomeQuiet();
    await arm();
    drafts.present = true;
    await useStore.getState().restartShellNowForUpdate();
    expect(native.apply).not.toHaveBeenCalled();
    expect(useStore.getState().toasts.map((t) => t.message).join(' ')).toMatch(/Restart held: a message box has text you have not sent/);
  });

  describe('paused by the signed control file', () => {
    it.each([
      ['public (its default)', undefined],
      ['team', 'team' as const],
    ])('never arms on %s while paused, and arms once it is turned on', async (_name, updateChannel) => {
      const paused = (on: boolean) => update({ updateChannel, shellAvailable: true, shellAutoApplyPaused: on });
      useCloud.setState({ mode: 'cloud', isLocal: true });
      useStore.setState({ updateStatus: paused(true) });
      vi.spyOn(api, 'getUpdateStatus').mockImplementation(async () => paused(true));
      await becomeQuiet();
      await useStore.getState().refreshUpdateStatus();
      await vi.advanceTimersByTimeAsync(0);
      expect(useStore.getState().shellAutoRestart).toBeNull();

      vi.spyOn(api, 'getUpdateStatus').mockImplementation(async () => paused(false));
      await useStore.getState().refreshUpdateStatus();
      await vi.advanceTimersByTimeAsync(0);
      expect(useStore.getState().shellAutoRestart?.version).toBe(version);
    });

    it('a status without the field counts as paused (fails closed)', async () => {
      useStore.setState({ updateStatus: { ...update(), shellAutoApplyPaused: undefined } });
      await becomeQuiet();
      useStore.getState().maybeAutoApplyShellUpdate();
      await vi.advanceTimersByTimeAsync(0);
      expect(useStore.getState().shellAutoRestart).toBeNull();
    });

    it('a pause published during the countdown cancels it', async () => {
      await becomeQuiet();
      await arm();
      useStore.setState({ updateStatus: update({ shellAutoApplyPaused: true }) });
      await vi.advanceTimersByTimeAsync(SHELL_AUTO_COUNTDOWN_MS + 1000);
      expect(useStore.getState().shellAutoRestart).toBeNull();
      expect(native.apply).not.toHaveBeenCalled();
    });

    it('a daemon refusal for a pause is quiet and uses up no attempt', async () => {
      useCloud.setState({ mode: 'cloud', isLocal: true });
      const team = () => update({ updateChannel: 'team', shellAvailable: true });
      useStore.setState({ updateStatus: team() });
      vi.spyOn(api, 'getUpdateStatus').mockImplementation(async () => team());
      vi.spyOn(api, 'applyShellUpdate').mockRejectedValue(
        new ApiError(409, 'paused', { code: 'shell_silent_paused', paused: true, reason: 'automatic desktop restarts paused by operator' }),
      );
      await becomeQuiet();
      await arm();
      await vi.advanceTimersByTimeAsync(SHELL_AUTO_COUNTDOWN_MS + 2000);
      expect(native.exit).not.toHaveBeenCalled();
      expect(useStore.getState().toasts).toHaveLength(0);
      expect(shellAutoApplyNeedsManual(version)).toBe(false);
    });

    it('a pause keeps the count of earlier failed attempts', async () => {
      useCloud.setState({ mode: 'cloud', isLocal: true });
      const team = () => update({ updateChannel: 'team', shellAvailable: true });
      useStore.setState({ updateStatus: team() });
      vi.spyOn(api, 'getUpdateStatus').mockImplementation(async () => team());
      const viaDaemon = vi.spyOn(api, 'applyShellUpdate').mockRejectedValueOnce(new Error('download installer: HTTP 503'));
      await becomeQuiet();
      await arm();
      await useStore.getState().restartShellNowForUpdate();
      expect(shellAutoApplyNeedsManual(version)).toBe(true);

      viaDaemon.mockRejectedValueOnce(
        new ApiError(409, 'paused', { code: 'shell_silent_paused', paused: true, reason: 'paused by operator' }),
      );
      await useStore.getState().restartShellNowForUpdate();
      expect(shellAutoApplyNeedsManual(version)).toBe(true);
      // A click that met a pause says so.
      expect(useStore.getState().toasts.map((t) => t.message).join(' ')).toMatch(/Automatic desktop restarts are paused/);
    });
  });

  describe('team channel', () => {
    const team = () => update({ updateChannel: 'team', shellAvailable: true });

    beforeEach(() => {
      useCloud.setState({ mode: 'cloud', isLocal: true });
      useStore.setState({ updateStatus: team() });
      vi.spyOn(api, 'getUpdateStatus').mockImplementation(async () => team());
    });

    it('installs through the local daemon, never the app’s own updater', async () => {
      const viaDaemon = vi.spyOn(api, 'applyShellUpdate').mockResolvedValue({ applying: true, silent: true });
      await becomeQuiet();
      await arm();
      await vi.advanceTimersByTimeAsync(SHELL_AUTO_COUNTDOWN_MS);
      expect(viaDaemon).toHaveBeenCalledTimes(1);
      expect(viaDaemon).toHaveBeenCalledWith(false, { silent: true });
      expect(native.apply).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1500);
      expect(native.exit).toHaveBeenCalledTimes(1);
    });

    it('does not arm when the daemon has no installer for this computer', async () => {
      useStore.setState({ updateStatus: update({ updateChannel: 'team', shellAvailable: false }) });
      await becomeQuiet();
      useStore.getState().maybeAutoApplyShellUpdate();
      await vi.advanceTimersByTimeAsync(0);
      expect(useStore.getState().shellAutoRestart).toBeNull();
    });

    it('keeps the app open when the daemon opened the installer window instead', async () => {
      vi.spyOn(api, 'applyShellUpdate').mockResolvedValue({ applying: true });
      await becomeQuiet();
      await arm();
      await vi.advanceTimersByTimeAsync(SHELL_AUTO_COUNTDOWN_MS + 2000);
      expect(native.exit).not.toHaveBeenCalled();
      expect(useStore.getState().updateStatus?.shellApplying).toBe(false);
      expect(useStore.getState().toasts.map((t) => t.message).join(' ')).toMatch(/installer opened/);
    });

    it('keeps the app open when the daemon refuses', async () => {
      vi.spyOn(api, 'applyShellUpdate').mockRejectedValue(new Error('download installer: HTTP 503'));
      await becomeQuiet();
      await arm();
      await vi.advanceTimersByTimeAsync(SHELL_AUTO_COUNTDOWN_MS + 2000);
      expect(native.apply).not.toHaveBeenCalled();
      expect(native.exit).not.toHaveBeenCalled();
      expect(useStore.getState().toasts.map((t) => t.message).join(' ')).toMatch(/could not start/);
    });

    it('passes the app’s own pid so the courier waits on exactly this process', async () => {
      native.appPid = 4242;
      const viaDaemon = vi.spyOn(api, 'applyShellUpdate').mockResolvedValue({ applying: true, silent: true });
      await becomeQuiet();
      await arm();
      await vi.advanceTimersByTimeAsync(SHELL_AUTO_COUNTDOWN_MS);
      expect(viaDaemon).toHaveBeenCalledWith(false, { silent: true, appPid: 4242 });
    });

    // #739: the courier started but found no app. Nothing was installed, so
    // it is retried in 15 minutes and is not one of the two attempts, but only
    // a few times a day: then it counts, and the two-attempt cap ends it.
    it('retries "app not found" after 15 minutes, a bounded number of times a day', async () => {
      const notFound = () => new ApiError(409, 'not found', { code: 'shell_silent_app_not_found', message: 'the desktop app is not running in this session' });
      const viaDaemon = vi.spyOn(api, 'applyShellUpdate').mockImplementation(async () => { throw notFound(); });
      const tryOnce = async () => {
        await becomeQuiet();
        await arm();
        await vi.advanceTimersByTimeAsync(SHELL_AUTO_COUNTDOWN_MS + 2000);
      };

      await tryOnce();
      expect(viaDaemon).toHaveBeenCalledTimes(1);
      expect(native.exit).not.toHaveBeenCalled();
      expect(shellAutoApplyNeedsManual(version)).toBe(false);
      expect(useStore.getState().toasts.map((t) => t.message).join(' ')).toMatch(/tries again in 15 minutes/);
      expect(shellAutoApplyWaitReason()).toMatchObject({ kind: 'retry', lastError: 'the last try did not find the app' });

      // Not before the 15 minutes are up.
      useStore.getState().maybeAutoApplyShellUpdate();
      await vi.advanceTimersByTimeAsync(0);
      expect(useStore.getState().shellAutoRestart).toBeNull();

      for (let i = 2; i <= SHELL_AUTO_APP_NOT_FOUND_PER_DAY; i++) {
        await vi.advanceTimersByTimeAsync(SHELL_AUTO_APP_NOT_FOUND_RETRY_MS);
        await tryOnce();
        expect(viaDaemon).toHaveBeenCalledTimes(i);
        expect(shellAutoApplyNeedsManual(version)).toBe(false);
      }
      // One more the same day counts as a real failed attempt (6 h cooldown).
      await vi.advanceTimersByTimeAsync(SHELL_AUTO_APP_NOT_FOUND_RETRY_MS);
      await tryOnce();
      expect(viaDaemon).toHaveBeenCalledTimes(SHELL_AUTO_APP_NOT_FOUND_PER_DAY + 1);
      expect(shellAutoApplyNeedsManual(version)).toBe(true);
      await vi.advanceTimersByTimeAsync(SHELL_AUTO_APP_NOT_FOUND_RETRY_MS);
      useStore.getState().maybeAutoApplyShellUpdate();
      await vi.advanceTimersByTimeAsync(0);
      expect(useStore.getState().shellAutoRestart).toBeNull();

      // However long it runs, the total stays bounded: the second counted
      // failure ends it for this version.
      for (let day = 0; day < 4; day++) {
        await vi.advanceTimersByTimeAsync(6 * 60 * 60 * 1000);
        useStore.getState().maybeAutoApplyShellUpdate();
        await vi.advanceTimersByTimeAsync(0);
        if (useStore.getState().shellAutoRestart) {
          await vi.advanceTimersByTimeAsync(SHELL_AUTO_COUNTDOWN_MS + 2000);
        } else {
          await tryOnce().catch(() => {});
        }
      }
      expect(viaDaemon.mock.calls.length).toBeLessThanOrEqual(SHELL_AUTO_APP_NOT_FOUND_PER_DAY + 2);
      expect(shellAutoApplyWaitReason()).toMatchObject({ kind: 'gave-up', reason: 'gave up after 2 tries' });
    });
  });

  it('waits for a daemon-first update when the installer does not carry it', async () => {
    useStore.setState({
      updateStatus: update({
        current: '0.0.932', latest: '0.0.932', available: false,
        shellLatest: '0.0.916', shellAvailable: true, shellCarriesLatestDaemon: false,
      }),
    });
    await becomeQuiet();
    useStore.getState().maybeAutoApplyShellUpdate();
    await vi.advanceTimersByTimeAsync(0);
    expect(useStore.getState().shellAutoRestart).toBeNull();
    expect(native.apply).not.toHaveBeenCalled();
  });

  it('arms a monotonic shell-first update even when a daemon-only publish is newer', async () => {
    native.installed = '0.0.900';
    useStore.setState({
      updateStatus: update({
        current: '0.0.900', latest: '0.0.932', available: true,
        shellLatest: '0.0.916', shellAvailable: true, shellCarriesLatestDaemon: false,
      }),
    });
    await becomeQuiet();
    useStore.getState().maybeAutoApplyShellUpdate();
    await vi.advanceTimersByTimeAsync(0);
    expect(useStore.getState().shellAutoRestart?.version).toBe('0.0.916');
  });

  describe('why it is still waiting (version menu)', () => {
    const at = (ms: number) => new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

    it('names the quiet time, then whatever holds it, with the pane', async () => {
      expect(shellAutoApplyWaitReason()).toEqual({ kind: 'waiting', reason: 'quiet time left' });
      useStore.setState({ panes: { p: pane({ id: 'p', title: 'Claude 2', pendingApproval: { requestId: 'r', toolName: 'Bash' } }) } });
      expect(shellAutoApplyWaitReason()?.reason).toBe('approval or question waiting in Claude 2');
      useStore.setState({ panes: { q: pane({ id: 'q', title: 'shell', titleOverride: 'Reviewer', attention: 'needs-user' }) } });
      expect(shellAutoApplyWaitReason()?.reason).toBe('approval or question waiting in Reviewer');
      useStore.setState({ panes: { n: pane({ id: 'n', kind: 'native', title: 'Codex', agentMidTurn: true }) } });
      expect(shellAutoApplyWaitReason()?.reason).toBe('agent working in Codex');
      useStore.setState({ panes: { d: pane({ id: 'd', title: 'Notes' }) } });
      drafts.present = true;
      drafts.paneId = 'd';
      expect(shellAutoApplyWaitReason()?.reason).toBe('unsent text in Notes');
      drafts.present = false;
      reportUnsentInput('broadcast', 'tell every pane');
      expect(shellAutoApplyWaitReason()?.reason).toBe('unsent text in a message box');
      reportUnsentInput('broadcast', '');
      useStore.setState({ panes: {}, updateStatus: update({ workflowInflight: 1 }) });
      expect(shellAutoApplyWaitReason()?.reason).toBe('workflow running');
      useStore.setState({ updateStatus: update() });
      await becomeQuiet();
      expect(shellAutoApplyWaitReason()).toBeNull();
      expect(shellAutoWaitLine(shellAutoApplyWaitReason(), true)).toBe('↻ Restarts automatically when idle');
    });

    it('says paused, postponed, cooldown and gave up, and stops promising a restart once a try failed', async () => {
      // The operator paused it: shell_auto_apply_paused, or the global pause.
      for (const reason of ['automatic desktop restarts paused by operator', 'updates paused by operator']) {
        useStore.setState({ updateStatus: update({ shellAutoApplyPaused: true, shellAutoApplyPausedReason: reason }) });
        expect(shellAutoApplyWaitReason()).toEqual({ kind: 'paused', reason: 'paused' });
        expect(shellAutoWaitLine(shellAutoApplyWaitReason(), false)).toBe('Automatic restart is paused — use Update now');
      }

      useStore.setState({ updateStatus: update() });
      await becomeQuiet();
      await arm();
      useStore.getState().postponeShellAutoRestart();
      expect(shellAutoApplyWaitReason()?.reason).toBe(`postponed until ${at(Date.now() + SHELL_AUTO_HOLD_MS)}`);
      await vi.advanceTimersByTimeAsync(SHELL_AUTO_HOLD_MS + 1000);

      native.outcome = { ok: false, error: 'unknown command' };
      await becomeQuiet();
      await arm();
      const failedAt = Date.now();
      await useStore.getState().restartShellNowForUpdate();
      useStore.setState({
        updateStatus: update({ shellLaunch: { state: 'launch-failed', error: 'installer did not start' } }),
      });
      const retry = shellAutoApplyWaitReason();
      expect(retry).toEqual({ kind: 'retry', reason: `cooldown until ${at(failedAt + 6 * 60 * 60 * 1000)}`, lastError: 'installer did not start' });
      const line = shellAutoWaitLine(retry, true);
      expect(line).toBe(`Automatic restart did not work — waiting: cooldown until ${at(failedAt + 6 * 60 * 60 * 1000)} · installer did not start`);
      expect(line).not.toMatch(/Restarts automatically when idle/);

      await vi.advanceTimersByTimeAsync(6 * 60 * 60 * 1000);
      await arm();
      await useStore.getState().restartShellNowForUpdate();
      const gaveUp = shellAutoApplyWaitReason();
      expect(gaveUp).toMatchObject({ kind: 'gave-up', reason: 'gave up after 2 tries' });
      expect(shellAutoWaitLine(gaveUp, true)).toBe('Automatic restart gave up after 2 tries — use Update now · installer did not start');
    });

    // Public's default pause is not news: no automatic-restart line at all,
    // as the setting itself is hidden there (#749). Nor for an older daemon
    // that gives no reason.
    it.each([
      ['the channel default (public)', 'automatic desktop restarts are not enabled for this channel yet'],
      ['no reason (older daemon)', undefined],
    ])('shows no automatic-restart line for a pause that is %s', (_name, reason) => {
      useStore.setState({ updateStatus: update({ shellAutoApplyPaused: true, shellAutoApplyPausedReason: reason }) });
      expect(shellAutoApplyWaitReason()).toBeNull();
      expect(shellAutoWaitLine(shellAutoApplyWaitReason(), false)).toBe('');
    });

    it('waits with the quiet reason in the version line', () => {
      expect(shellAutoWaitLine(shellAutoApplyWaitReason(), true)).toBe('↻ Restarts automatically when idle — waiting: quiet time left');
    });
  });
});

