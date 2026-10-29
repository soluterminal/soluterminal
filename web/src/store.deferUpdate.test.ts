import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError } from './api';
import { cancelDeferredUpdateRetry, nativePaneBusy, resetUpdateRecoveryForTests, useStore } from './store';
import type { PaneInfo, UpdateHold, UpdateStatus } from './types';
import { reportUnsentInput, resetUnsentInputForTests } from './lib/unsentInput';
import { setDraft } from './native/useNativePane';

const pane = (p: Partial<PaneInfo> & { id: string }): PaneInfo => ({ ...p }) as PaneInfo;
const status = (over: Partial<UpdateStatus>): UpdateStatus =>
  ({ current: '1.0.0', available: false, ...over }) as UpdateStatus;
const hold = (over: Partial<UpdateHold>): UpdateHold => ({
  cause: 'unknown',
  since: Date.now(),
  auto: false,
  ...over,
});

describe('nativePaneBusy', () => {
  it('blocks only a genuine active native turn', () => {
    expect(nativePaneBusy({ a: pane({ id: 'a', kind: 'native', agentMidTurn: true }) })).toBe(true);
    expect(nativePaneBusy({ a: pane({ id: 'a', kind: 'native', agentMidTurn: false }) })).toBe(false);
    expect(nativePaneBusy({ a: pane({ id: 'a', kind: 'native', status: 'exited', agentMidTurn: false }) })).toBe(false);
    expect(nativePaneBusy({ a: pane({ id: 'a', kind: 'native', status: 'exited' }) })).toBe(false);
    expect(nativePaneBusy({ a: pane({ id: 'a', kind: '', agentMidTurn: true }) })).toBe(false);
    expect(nativePaneBusy({})).toBe(false);
  });
});

describe('typed deferred updates', () => {
  const realApplyUpdate = useStore.getState().applyUpdate;
  const realRefreshPanes = useStore.getState().refreshPanes;
  const realRefreshUpdateStatus = useStore.getState().refreshUpdateStatus;

  beforeEach(() => {
    vi.useFakeTimers();
    cancelDeferredUpdateRetry();
    resetUpdateRecoveryForTests();
    useStore.setState({
      panes: {},
      deferredUpdate: null,
      updateApplyRequestInFlight: false,
      updateSettings: { autoApply: 'off' },
      updateStatus: status({ latest: '2.0.0', available: true, applying: false }),
      toasts: [],
      applyUpdate: realApplyUpdate,
      refreshPanes: realRefreshPanes,
      refreshUpdateStatus: realRefreshUpdateStatus,
    });
  });

  afterEach(() => {
    cancelDeferredUpdateRetry();
    resetUpdateRecoveryForTests();
    useStore.setState({
      panes: {},
      deferredUpdate: null,
      updateApplyRequestInFlight: false,
      updateStatus: null,
      applyUpdate: realApplyUpdate,
      refreshPanes: realRefreshPanes,
      refreshUpdateStatus: realRefreshUpdateStatus,
    });
    vi.restoreAllMocks();
    vi.useRealTimers();
    localStorage.removeItem('solu.update.autoApplied');
  });

  it('keeps a workflow 409 truthful and never forces, even with zero native panes for 20 minutes', async () => {
    const deadline = Date.now() + 10 * 60_000;
    const applySpy = vi.spyOn(api, 'applyUpdate').mockRejectedValue(
      new ApiError(409, 'workflow drain', {
        busy: true,
        cause: 'workflow',
        reason: 'waiting for 2 in-flight workflow invocations',
        workflowInflight: 2,
        workflowDeferDeadlineMs: deadline,
      }),
    );
    vi.spyOn(api, 'getUpdateStatus').mockResolvedValue(status({
      latest: '2.0.0',
      available: true,
      workflowDeferred: true,
      workflowInflight: 2,
      workflowDeferDeadlineMs: deadline,
    }));

    await useStore.getState().applyUpdate();
    expect(useStore.getState().deferredUpdate?.cause).toBe('workflow');
    expect(useStore.getState().toasts.at(-1)?.message).not.toContain('Focus');

    await vi.advanceTimersByTimeAsync(20 * 60_000);
    expect(applySpy).not.toHaveBeenCalledWith(true);
    expect(applySpy.mock.calls.every(([force]) => force === false)).toBe(true);
    expect(useStore.getState().deferredUpdate?.cause).toBe('workflow');
  });

  it('retries a settled Focus hold without force and transitions in place to workflow', async () => {
    const deadline = Date.now() + 10 * 60_000;
    const applySpy = vi.spyOn(api, 'applyUpdate').mockRejectedValue(
      new ApiError(409, 'workflow drain', {
        busy: true,
        cause: 'workflow',
        reason: 'waiting for 1 in-flight workflow invocation',
        workflowInflight: 1,
        workflowDeferDeadlineMs: deadline,
      }),
    );
    useStore.setState({
      panes: { focus1: pane({ id: 'focus1', kind: 'native', agentMidTurn: true }) },
    });
    const observed: Array<UpdateHold | null> = [];
    const unsubscribe = useStore.subscribe((s) => observed.push(s.deferredUpdate));

    await useStore.getState().applyUpdate();
    const since = useStore.getState().deferredUpdate?.since;
    expect(useStore.getState().deferredUpdate).toMatchObject({ cause: 'focus', paneId: 'focus1' });
    expect(applySpy).not.toHaveBeenCalled();

    useStore.setState({
      panes: { focus1: pane({ id: 'focus1', kind: 'native', agentMidTurn: false }) },
    });
    await vi.advanceTimersByTimeAsync(4000);
    unsubscribe();

    expect(applySpy).toHaveBeenCalledWith(false);
    expect(useStore.getState().deferredUpdate).toMatchObject({ cause: 'workflow', since });
    const firstHeld = observed.findIndex(Boolean);
    expect(observed.slice(firstHeld).every((value) => value !== null)).toBe(true);
  });

  it('bounds repeated 409/status events to one retry timer and never flashes applying', async () => {
    const deadline = Date.now() + 10 * 60_000;
    vi.spyOn(api, 'applyUpdate').mockRejectedValue(
      new ApiError(409, 'workflow drain', {
        busy: true,
        cause: 'workflow',
        workflowInflight: 3,
        workflowDeferDeadlineMs: deadline,
      }),
    );
    vi.spyOn(api, 'getUpdateStatus').mockResolvedValue(status({
      latest: '2.0.0', available: true, workflowDeferred: true,
      workflowInflight: 3, workflowDeferDeadlineMs: deadline,
    }));
    const applying: boolean[] = [];
    const unsubscribe = useStore.subscribe((s) => applying.push(s.updateStatus?.applying === true));

    await useStore.getState().applyUpdate();
    expect(vi.getTimerCount()).toBe(1);
    for (let i = 0; i < 8; i += 1) {
      await vi.advanceTimersByTimeAsync(4000);
      expect(vi.getTimerCount()).toBe(1);
    }
    unsubscribe();
    expect(applying).not.toContain(true);
  });

  it('never exposes applying during the reported transient-status then typed-409 race', async () => {
    const deadline = Date.now() + 6 * 60_000 + 56_000;
    let rejectApply!: (error: Error) => void;
    const pending = new Promise<{ applying: boolean }>((_resolve, reject) => { rejectApply = reject; });
    const applySpy = vi.spyOn(api, 'applyUpdate').mockReturnValue(pending);
    vi.spyOn(api, 'getUpdateStatus').mockResolvedValue(status({
      latest: '2.0.0', available: true, applying: true,
      workflowDeferred: true, workflowDeferState: 'draining',
      workflowInflight: 6, workflowDeferDeadlineMs: deadline,
    }));
    useStore.setState({ updateSettings: { autoApply: 'when-idle' } });
    const observations: boolean[] = [];
    const unsubscribe = useStore.subscribe((state) => {
      observations.push(state.updateStatus?.applying === true);
    });

    const request = useStore.getState().applyUpdate({ auto: true });
    await Promise.resolve();
    await useStore.getState().refreshUpdateStatus();
    expect(useStore.getState().updateStatus?.applying).toBe(false);

    rejectApply(new ApiError(409, 'workflow drain', {
      busy: true, cause: 'workflow',
      workflowInflight: 6, workflowDeferDeadlineMs: deadline,
    }));
    await request;
    unsubscribe();

    expect(observations).not.toContain(true);
    expect(useStore.getState().deferredUpdate).toMatchObject({
      cause: 'workflow', workflowInflight: 6, workflowDeferDeadlineMs: deadline,
    });
    expect(vi.getTimerCount()).toBe(1);
    useStore.getState().maybeAutoApplyUpdate();
    expect(applySpy).toHaveBeenCalledTimes(1);
    expect(applySpy.mock.calls.some(([force]) => force === true)).toBe(false);
  });

  it('an unattended swap held for a mid-turn pane also waits out text typed meanwhile', async () => {
    const applySpy = vi.spyOn(api, 'applyUpdate').mockResolvedValue({ applying: false });
    vi.spyOn(api, 'getUpdateStatus').mockResolvedValue(status({ latest: '2.0.0', available: true }));
    useStore.setState({ panes: { p: pane({ id: 'p', kind: 'native', agentMidTurn: true }) } });
    await useStore.getState().applyUpdate({ auto: true });
    expect(useStore.getState().deferredUpdate?.cause).toBe('focus');

    useStore.setState({ panes: { p: pane({ id: 'p', kind: 'native', agentMidTurn: false }) } });
    reportUnsentInput('reply', 'one more thing');
    try {
      await vi.advanceTimersByTimeAsync(4000 * 3);
      expect(applySpy).not.toHaveBeenCalled();
      expect(useStore.getState().deferredUpdate?.cause).toBe('focus');

      reportUnsentInput('reply', '');
      await vi.advanceTimersByTimeAsync(4000);
      expect(applySpy).toHaveBeenCalledTimes(1);
    } finally {
      resetUnsentInputForTests();
    }
  });

  it('treats a legacy cause-less 409 as unknown with generic non-forced copy', async () => {
    const applySpy = vi.spyOn(api, 'applyUpdate').mockRejectedValue(
      new ApiError(409, 'old daemon busy', { busy: true, reason: 'active work' }),
    );
    vi.spyOn(api, 'getUpdateStatus').mockResolvedValue(status({ latest: '2.0.0', available: true }));

    await useStore.getState().applyUpdate();
    expect(useStore.getState().deferredUpdate?.cause).toBe('unknown');
    const copy = useStore.getState().toasts.at(-1)?.message ?? '';
    expect(copy).toContain('active work');
    expect(copy).not.toContain('Focus');
    await vi.advanceTimersByTimeAsync(4000);
    expect(applySpy).not.toHaveBeenCalledWith(true);
  });

	it('preserves installing copy for a forward update generic hold', async () => {
		vi.spyOn(api, 'applyUpdate').mockRejectedValue(
			new ApiError(409, 'old daemon busy', { busy: true }),
		);
		await useStore.getState().applyUpdate();
		expect(useStore.getState().toasts.at(-1)?.message).toBe(
			'Update queued — installing when active work finishes',
		);
	});

  it('preserves a legacy plain-text reason and pane id without claiming a Focus cause', async () => {
    const paneId = 'pane_0123456789abcdef';
    vi.spyOn(api, 'applyUpdate').mockRejectedValue(
      new ApiError(409, `a Focus pane is mid-turn (pane ${paneId})`),
    );

    await useStore.getState().applyUpdate();

    expect(useStore.getState().deferredUpdate).toMatchObject({
      cause: 'unknown',
      reason: `a Focus pane is mid-turn (pane ${paneId})`,
      paneId,
    });
    const copy = useStore.getState().toasts.at(-1)?.message ?? '';
    expect(copy).toContain('active work');
    expect(copy).not.toContain('Focus');
  });

  it('ignores transient applying polls during a hold and clears expired workflow detail', async () => {
    const deadline = Date.now() + 10 * 60_000;
    vi.spyOn(api, 'applyUpdate').mockRejectedValue(
      new ApiError(409, 'workflow drain', {
        busy: true,
        cause: 'workflow',
        workflowInflight: 2,
        workflowDeferDeadlineMs: deadline,
      }),
    );
    const statusSpy = vi.spyOn(api, 'getUpdateStatus');

    await useStore.getState().applyUpdate();
    statusSpy.mockResolvedValueOnce(status({
      latest: '2.0.0',
      available: true,
      applying: true,
      workflowDeferred: true,
      workflowInflight: 2,
      workflowDeferDeadlineMs: deadline,
    }));
    await useStore.getState().refreshUpdateStatus();
    expect(useStore.getState().updateStatus?.applying).toBe(false);

    statusSpy.mockResolvedValueOnce(status({
      latest: '2.0.0',
      available: true,
      applying: false,
    }));
    await useStore.getState().refreshUpdateStatus();
    expect(useStore.getState().deferredUpdate).toMatchObject({ cause: 'workflow' });
    expect(useStore.getState().deferredUpdate?.workflowInflight).toBeUndefined();
    expect(useStore.getState().deferredUpdate?.workflowDeferDeadlineMs).toBeUndefined();
    expect(vi.getTimerCount()).toBe(1);
  });

  it('normalizes the legacy applying conflict on a manual check response', async () => {
    vi.spyOn(api, 'checkForUpdate').mockResolvedValue(status({
      latest: '2.0.0', available: true, applying: true,
      workflowDeferred: true, workflowDeferState: 'draining',
      workflowInflight: 2,
    }));

    await useStore.getState().checkForUpdate();

    expect(useStore.getState().updateStatus).toMatchObject({
      applying: false,
      workflowDeferred: true,
      workflowInflight: 2,
    });
    expect(vi.getTimerCount()).toBe(0);
  });

  it('deduplicates concurrent apply requests and keeps one retry controller', async () => {
    let rejectRequest!: (error: Error) => void;
    const request = new Promise<{ applying: boolean }>((_resolve, reject) => {
      rejectRequest = reject;
    });
    const applySpy = vi.spyOn(api, 'applyUpdate').mockReturnValue(request);
    useStore.setState({ deferredUpdate: hold({ cause: 'workflow', workflowInflight: 1 }) });

    const first = useStore.getState().applyUpdate();
    const second = useStore.getState().applyUpdate();
    await second;
    await Promise.resolve();

    expect(applySpy).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(1);
    expect(useStore.getState().updateStatus?.applying).toBe(false);

    rejectRequest(new ApiError(500, 'stop'));
    await expect(first).rejects.toThrow('stop');
    expect(vi.getTimerCount()).toBe(1);
    expect(useStore.getState().updateStatus?.applying).toBe(false);
  });

  it('does not re-announce cause flips within one queued update', async () => {
    const deadline = Date.now() + 10 * 60_000;
    vi.spyOn(api, 'applyUpdate').mockRejectedValue(
      new ApiError(409, 'workflow drain', {
        busy: true, cause: 'workflow', workflowInflight: 2,
        workflowDeferDeadlineMs: deadline,
      }),
    );
    vi.spyOn(api, 'getUpdateStatus').mockResolvedValue(status({
      latest: '2.0.0', available: true, workflowDeferred: true,
      workflowInflight: 2, workflowDeferDeadlineMs: deadline,
    }));

    await useStore.getState().applyUpdate();
    const toastCount = useStore.getState().toasts.length;
    useStore.setState({
      panes: {
        focus: pane({
          id: 'pane_0123456789abcdef', kind: 'native', agentMidTurn: true,
        }),
      },
    });
    await vi.advanceTimersByTimeAsync(4000);
    expect(useStore.getState().deferredUpdate?.cause).toBe('focus');
    expect(useStore.getState().toasts).toHaveLength(toastCount);

    useStore.setState({ panes: {} });
    await vi.advanceTimersByTimeAsync(4000);
    expect(useStore.getState().deferredUpdate?.cause).toBe('workflow');
    expect(useStore.getState().toasts).toHaveLength(toastCount);
  });

  it('keeps unattended workflow retries non-forced over an extended hold', async () => {
    const deadline = Date.now() + 10 * 60_000;
    const applySpy = vi.spyOn(api, 'applyUpdate').mockRejectedValue(
      new ApiError(409, 'workflow drain', {
        busy: true, cause: 'workflow', workflowInflight: 2,
        workflowDeferDeadlineMs: deadline,
      }),
    );
    vi.spyOn(api, 'getUpdateStatus').mockResolvedValue(status({
      latest: '2.0.0', available: true, workflowDeferred: true,
      workflowInflight: 2, workflowDeferDeadlineMs: deadline,
    }));
    useStore.setState({ updateSettings: { autoApply: 'when-idle' } });

    useStore.getState().maybeAutoApplyUpdate();
    await vi.waitFor(() => expect(useStore.getState().deferredUpdate?.cause).toBe('workflow'));
    await vi.advanceTimersByTimeAsync(20 * 60_000);

    expect(applySpy).toHaveBeenCalled();
    expect(applySpy.mock.calls.every(([force]) => force === false)).toBe(true);
    expect(useStore.getState().deferredUpdate).toMatchObject({ cause: 'workflow', auto: true });
    expect(useStore.getState().toasts.at(-1)?.message).not.toContain('Focus');
  });

  it('sends force only for an explicit override and clears the hold first', async () => {
    useStore.setState({ deferredUpdate: hold({ cause: 'workflow' }) });
    const applySpy = vi.spyOn(api, 'applyUpdate').mockRejectedValue(new ApiError(500, 'stop'));
    await expect(useStore.getState().applyUpdate({ force: true })).rejects.toThrow('stop');
    expect(applySpy).toHaveBeenCalledTimes(1);
    expect(applySpy).toHaveBeenCalledWith(true);
    expect(useStore.getState().deferredUpdate).toBeNull();
  });

  it('cleans up on latest-version change and on an accepted apply', async () => {
    const applySpy = vi.spyOn(api, 'applyUpdate');
    applySpy.mockRejectedValueOnce(
      new ApiError(409, 'workflow drain', { busy: true, cause: 'workflow' }),
    );
    const statusSpy = vi.spyOn(api, 'getUpdateStatus');
    await useStore.getState().applyUpdate();
    expect(vi.getTimerCount()).toBe(1);

    statusSpy.mockResolvedValueOnce(status({ latest: '3.0.0', available: true }));
    await useStore.getState().refreshUpdateStatus();
    expect(useStore.getState().deferredUpdate).toBeNull();
    expect(vi.getTimerCount()).toBe(0);

    useStore.setState({ updateStatus: status({ latest: '3.0.0', available: true }) });
    applySpy.mockResolvedValueOnce({ applying: true });
    statusSpy.mockResolvedValueOnce(status({ current: '3.0.0', latest: '3.0.0', available: false }));
    const applyingPromise = useStore.getState().applyUpdate({ skipReload: true });
    await vi.advanceTimersByTimeAsync(500);
    await applyingPromise;
    expect(useStore.getState().deferredUpdate).toBeNull();
    expect(useStore.getState().updateStatus?.applying).not.toBe(true);
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe('maybeAutoApplyUpdate', () => {
  const realApplyUpdate = useStore.getState().applyUpdate;
  afterEach(() => {
    useStore.setState({ deferredUpdate: null, panes: {}, applyUpdate: realApplyUpdate });
    localStorage.removeItem('solu.update.autoApplied');
  });

  it.each([
    ['optional off', { autoApply: 'off' as const }, false, false],
    ['optional when-idle', { autoApply: 'when-idle' as const }, false, true],
    ['required off', { autoApply: 'off' as const }, true, true],
  ])('%s', (name, updateSettings, required, shouldApply) => {
    const applySpy = vi.fn().mockResolvedValue(undefined);
    useStore.setState({
      deferredUpdate: null,
      updateSettings,
      updateStatus: status({ latest: `4.0.${name.length}`, available: true, required }),
      applyUpdate: applySpy as never,
    });
    useStore.getState().maybeAutoApplyUpdate();
    expect(applySpy).toHaveBeenCalledTimes(shouldApply ? 1 : 0);
    if (shouldApply) expect(applySpy).toHaveBeenCalledWith({ auto: true });
  });

  // The swap reloads the page. Unsent text in any compose box holds it until
  // the text is sent or cleared, and the hold uses up no attempt.
  it.each([
    ['a native pane draft', '4.1.1', (text: string) => setDraft('p-draft', text)],
    ['another compose box', '4.1.2', (text: string) => reportUnsentInput('feedback', text)],
  ])('waits while %s holds unsent text', (_name, latest, type) => {
    const applySpy = vi.fn().mockResolvedValue(undefined);
    useStore.setState({
      deferredUpdate: null,
      updateSettings: { autoApply: 'when-idle' },
      updateStatus: status({ latest, available: true }),
      applyUpdate: applySpy as never,
    });
    try {
      type('half a sentence');
      useStore.getState().maybeAutoApplyUpdate();
      useStore.getState().maybeAutoApplyUpdate();
      expect(applySpy).not.toHaveBeenCalled();

      type('');
      useStore.getState().maybeAutoApplyUpdate();
      expect(applySpy).toHaveBeenCalledTimes(1);
      expect(applySpy).toHaveBeenCalledWith({ auto: true });
    } finally {
      type('');
      resetUnsentInputForTests();
    }
  });
});
