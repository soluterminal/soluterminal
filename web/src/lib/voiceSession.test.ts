import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { CaptureSegment, ContinuousCaptureOptions } from 'soluvoice';
import { VoiceSessionController, type StartSession } from './voiceSession';
import { useStore } from '../store';
import { api } from '../api';
import { VoiceRequestError } from './voiceRequest';

let capture: ContinuousCaptureOptions;
let ready: () => void;
const stopCapture = vi.fn(async () => {});
const cancelCapture = vi.fn();
let controller: VoiceSessionController;
let options: StartSession;
const settle = async () => {
  for (let n = 0; n < 15; n++) await Promise.resolve();
};
const segment = (index: number): CaptureSegment => ({
  index,
  blob: new Blob([new Int16Array(16000)]),
  frames: 16000,
  durationMs: 1000,
  endFrame: (index + 1) * 16000,
});
beforeEach(() => {
  vi.useFakeTimers();
  stopCapture.mockReset().mockResolvedValue(undefined);
  cancelCapture.mockClear();
  useStore.setState({ voiceSession: null, voiceHistory: [], voiceAutoSend: true });
  controller = new VoiceSessionController((opts) => {
    capture = opts;
    return {
      start: () =>
        new Promise<void>((r) => {
          ready = r;
        }),
      stop: stopCapture,
      cancel: cancelCapture,
      dispose: cancelCapture,
      subscribe: () => () => {},
    };
  });
  options = {
    paneId: 'p1',
    title: 'test',
    language: 'en',
    context: 'initial',
    autoSend: true,
    deferCleanup: false,
    keepWarmMs: 0,
    valid: () => true,
    deliver: vi.fn(),
    request: vi.fn(async () => ({ text: 'hello', rawText: 'um hello', cleanup: 'ok' })),
  };
});
afterEach(() => {
  controller.cancel();
  vi.useRealTimers();
  vi.restoreAllMocks();
});
const start = async () => {
  expect(controller.start(options)).toBe(true);
  ready();
  await settle();
};
it('captures beyond 120 seconds with ordered delayed work and no internal delivery', async () => {
  let finish!: (r: { text: string }) => void;
  const request = vi
    .fn()
    .mockImplementationOnce(
      () =>
        new Promise((r) => {
          finish = r;
        }),
    )
    .mockResolvedValueOnce({ text: 'second' })
    .mockResolvedValueOnce({ text: 'third' });
  options.request = request;
  await start();
  capture.onSegment(segment(0));
  capture.onSegment(segment(1));
  await vi.advanceTimersByTimeAsync(125000);
  capture.onSegment(segment(2));
  expect(controller.getSnapshot()?.phase).toBe('recording');
  expect(request).toHaveBeenCalledTimes(1);
  expect(options.deliver).not.toHaveBeenCalled();
  finish({ text: 'first' });
  await settle();
  expect(request).toHaveBeenCalledTimes(3);
  expect(options.deliver).not.toHaveBeenCalled();
  controller.stop();
  controller.stop();
  await settle();
  expect(options.deliver).toHaveBeenCalledTimes(1);
  expect(options.deliver).toHaveBeenCalledWith(
    'first second third',
    expect.objectContaining({ autoSend: true }),
  );
  expect(useStore.getState().voiceHistory).toHaveLength(1);
});
it.each(['ok', 'disabled', 'skipped', 'failed', undefined] as const)('does not repeat cleanup when recognition reports %s', async (cleanup) => {
  options.request = undefined; options.deferCleanup = true;
  vi.spyOn(api, 'transcribe').mockResolvedValue({ text: 'English words', cleanup, quota: null });
  const tidy = vi.spyOn(api, 'cleanupVoice');
  await start(); capture.onSegment(segment(0)); await settle();
  expect(tidy).not.toHaveBeenCalled();
  expect(controller.getSnapshot()?.phase).toBe('recording');
  controller.stop(); await settle();
  expect(options.deliver).toHaveBeenCalledWith('English words', expect.anything());
});
it('keeps raw recognition and recording active when optional deferred cleanup fails', async () => {
  options.request = undefined; options.deferCleanup = true;
  const recognize = vi.spyOn(api, 'transcribe').mockResolvedValue({ text: 'English ending', cleanup: 'deferred', quota: null });
  const tidy = vi.spyOn(api, 'cleanupVoice').mockRejectedValue(new VoiceRequestError('Cleanup unavailable', 'other'));
  await start(); capture.onSegment(segment(0)); await settle();
  expect(controller.getSnapshot()).toMatchObject({ phase: 'recording', completedSegments: 1, retainedDurationMs: 0 });
  expect(stopCapture).not.toHaveBeenCalled();
  expect(tidy.mock.calls[0][1]?.language).toBe('en');
  controller.stop(); await settle();
  expect(recognize).toHaveBeenCalledTimes(1);
  expect(options.deliver).toHaveBeenCalledWith('English ending', expect.objectContaining({ rawText: 'English ending', cleanup: 'failed' }));
});
it('checks the context policy once, as recording starts, and uses that answer for every upload', async () => {
  options.request = undefined; options.deferCleanup = true;
  const check = { allowed: true };
  const startCheck = vi.spyOn(api, 'startVoiceContextCheck').mockReturnValue(check);
  const recognize = vi.spyOn(api, 'transcribe').mockResolvedValue({ text: 'some words', cleanup: 'deferred', quota: null });
  const tidy = vi.spyOn(api, 'cleanupVoice').mockResolvedValue({ text: 'Some words.', cleanup: 'ok' });
  expect(controller.start(options)).toBe(true);
  expect(startCheck).toHaveBeenCalledTimes(1);
  ready(); await settle();
  capture.onSegment(segment(0)); await settle();
  capture.onSegment(segment(1)); await settle();
  expect(startCheck).toHaveBeenCalledTimes(1);
  expect(recognize.mock.calls.map((c) => c[1]?.contextCheck)).toEqual([check, check]);
  expect(tidy.mock.calls.map((c) => c[1]?.contextCheck)).toEqual([check, check]);
});
it('keeps capturing after a transient request failure until the bounded buffer requires review', async () => {
  options.request = vi.fn().mockRejectedValue(new Error('Temporary network failure'));
  await start(); capture.onSegment(segment(0)); await settle();
  expect(controller.getSnapshot()?.phase).toBe('recording');
  expect(stopCapture).not.toHaveBeenCalled();
  capture.onSegment(segment(1));
  capture.onProgress?.({ capturedMs: 125000, activeBytes: 0, segments: 2 });
  expect(controller.getSnapshot()?.phase).toBe('recording');
  expect(options.request).toHaveBeenCalledTimes(1);
  capture.onProgress?.({ capturedMs: 185000, activeBytes: 4 * 1024 * 1024, segments: 2 });
  await settle();
  expect(controller.getSnapshot()).toMatchObject({ phase: 'recovery', stopReason: 'backlog' });
  expect(controller.getSnapshot()!.retainedBytes).toBeGreaterThan(0);
  expect(options.deliver).not.toHaveBeenCalled();
});
it('retains failed bytes, retries only the unresolved part with pinned language', async () => {
  options.request = undefined;
  const request = vi
    .spyOn(api, 'transcribe')
    .mockResolvedValueOnce({ text: 'one', quota: null })
    .mockRejectedValueOnce(new Error('offline'))
    .mockResolvedValue({ text: 'two', quota: null });
  await start();
  capture.onSegment(segment(0));
  await settle();
  const second = segment(1);
  capture.onSegment(second);
  await settle();
  expect(controller.getSnapshot()?.phase).toBe('recording');
  controller.stop();
  await settle();
  expect(controller.getSnapshot()?.phase).toBe('recovery');
  useStore.setState({ voiceLanguage: 'ms' });
  controller.retry();
  await settle();
  expect(request.mock.calls.map((c) => c[1]?.language)).toEqual(['en', 'en', 'en']);
  expect(request.mock.calls[2][0]).toBe(second.blob);
  expect(request.mock.calls[0][0]).not.toBe(second.blob);
  controller.useAvailable();
  await settle();
  expect(options.deliver).toHaveBeenCalledWith(
    'one two',
    expect.objectContaining({ autoSend: false }),
  );
});
it('preserves explicit Auto even when preferences change, and cancels pending initialization', async () => {
  options.language = '';
  options.request = undefined;
  const request = vi.spyOn(api, 'transcribe').mockResolvedValue({ text: 'auto', quota: null });
  await start();
  useStore.setState({ voiceLanguage: 'en' });
  capture.onSegment(segment(0));
  await settle();
  expect(request.mock.calls[0][1]?.language).toBe('');
  controller.cancel();
  expect(controller.start(options)).toBe(true);
  controller.stop();
  ready();
  await settle();
  expect(controller.getSnapshot()).toBeNull();
  expect(options.deliver).not.toHaveBeenCalled();
});
it.each(['blur', 'duration', 'backlog'] as const)('requires review after %s', async (reason) => {
  await start();
  capture.onSegment(segment(0));
  await settle();
  controller.stop(false, reason);
  await settle();
  expect(options.deliver).not.toHaveBeenCalled();
  expect(controller.getSnapshot()?.phase).toBe('recovery');
  controller.useAvailable();
  await settle();
  expect(options.deliver).toHaveBeenCalledWith(
    'hello',
    expect.objectContaining({ autoSend: false }),
  );
});
it('quota stops capture and preserves ordered partial text without automatic retry', async () => {
  options.request = vi
    .fn()
    .mockResolvedValueOnce({ text: 'one' })
    .mockRejectedValueOnce(new VoiceRequestError('daily limit', 'quota'));
  await start();
  capture.onSegment(segment(0));
  await settle();
  capture.onSegment(segment(1));
  capture.onSegment(segment(2));
  await settle();
  expect(options.request).toHaveBeenCalledTimes(2);
  expect(stopCapture).toHaveBeenCalled();
  controller.useAvailable();
  await settle();
  expect(options.deliver).toHaveBeenCalledWith(
    'one [Recording part omitted]',
    expect.objectContaining({ partial: true, autoSend: false }),
  );
});
it('reserves the last reported allowance request for the capture tail', async () => {
  options.request = vi.fn()
    .mockResolvedValueOnce({ text: 'one', quota: { remaining: 1, limit: 20 } })
    .mockResolvedValueOnce({ text: 'ending', quota: { remaining: 0, limit: 20 } });
  stopCapture.mockImplementationOnce(async () => { capture.onSegment(segment(1)); });
  await start();
  capture.onSegment(segment(0));
  await settle();
  expect(stopCapture).toHaveBeenCalled();
  expect(options.request).toHaveBeenCalledTimes(2);
  expect(controller.getSnapshot()).toMatchObject({ phase: 'recovery', errorCode: 'quota', quotaRemaining: 0, retainedBytes: 0 });
  expect(options.deliver).not.toHaveBeenCalled();
  controller.useAvailable();
  await settle();
  expect(options.deliver).toHaveBeenCalledWith('one ending', expect.objectContaining({ autoSend: false }));
});
it('retains queued audio without another upload after allowance exhaustion, and permits an explicit later retry', async () => {
  options.request = vi.fn()
    .mockResolvedValueOnce({ text: 'one', quota: { remaining: 0, limit: 20 } })
    .mockResolvedValueOnce({ text: 'ending', quota: { remaining: 19, limit: 20 } });
  stopCapture.mockImplementationOnce(async () => { capture.onSegment(segment(1)); });
  await start();
  capture.onSegment(segment(0));
  await settle();
  expect(options.request).toHaveBeenCalledTimes(1);
  expect(controller.getSnapshot()).toMatchObject({ phase: 'recovery', errorCode: 'quota', quotaRemaining: 0 });
  expect(controller.getSnapshot()!.retainedBytes).toBeGreaterThan(0);
  expect(useStore.getState().voiceHistory[0].text).toContain('one');
  controller.retry();
  await settle();
  expect(options.request).toHaveBeenCalledTimes(2);
  controller.useAvailable();
  await settle();
  expect(options.deliver).toHaveBeenCalledWith('one ending', expect.objectContaining({ autoSend: false }));
});
it('continues capture while the reported allowance covers queued work and the final part', async () => {
  options.request = vi.fn().mockResolvedValue({ text: 'one', quota: { remaining: 19, limit: 20 } });
  await start();
  capture.onSegment(segment(0));
  await settle();
  expect(controller.getSnapshot()).toMatchObject({ phase: 'recording', quotaRemaining: 19 });
});
it('cancel while draining invalidates late callbacks and removes unfinished history', async () => {
  let finish!: (r: { text: string }) => void;
  options.request = () =>
    new Promise((r) => {
      finish = r;
    });
  await start();
  capture.onSegment(segment(0));
  controller.stop();
  controller.cancel();
  finish({ text: 'late' });
  await settle();
  expect(options.deliver).not.toHaveBeenCalled();
  expect(useStore.getState().voiceHistory).toHaveLength(0);
});
it('never reacquires a changed or disposed destination', async () => {
  await start();
  capture.onSegment(segment(0));
  await settle();
  controller.detach(controller.getSnapshot()!.sessionId);
  await settle();
  controller.useAvailable();
  await settle();
  expect(options.deliver).not.toHaveBeenCalled();
  expect(useStore.getState().voiceHistory[0].delivered).toBe(false);
});
it.each(['changed', 'detached'])('keeps history after discarding %s destination recovery and starting again', async (destination) => {
  let valid = true;
  options.valid = () => valid;
  await start();
  capture.onSegment(segment(0));
  await settle();
  if (destination === 'detached') controller.detach(controller.getSnapshot()!.sessionId);
  else {
    valid = false;
    controller.stop();
  }
  await settle();
  expect(controller.getSnapshot()?.phase).toBe('recovery');
  const sessionId = controller.getSnapshot()!.sessionId;
  controller.discardAudio();
  expect(controller.getSnapshot()).toBeNull();
  expect(useStore.getState().voiceHistory).toEqual([expect.objectContaining({
    sessionId, text: 'hello', raw: 'um hello', delivered: false,
  })]);
  expect(options.deliver).not.toHaveBeenCalled();
  expect(controller.start(options)).toBe(true);
  expect(useStore.getState().voiceHistory[0].sessionId).toBe(sessionId);
});
it('does not create an empty history entry for unprocessed audio', async () => {
  options.request = vi.fn().mockRejectedValue(new Error('offline'));
  await start();
  capture.onSegment(segment(0));
  await settle();
  expect(controller.getSnapshot()?.retainedBytes).toBeGreaterThan(0);
  controller.stop();
  await settle();
  controller.discardAudio();
  expect(controller.getSnapshot()).toBeNull();
  expect(useStore.getState().voiceHistory).toHaveLength(0);
});
it('guards actual blob bytes before HTTP and retains audio on text limits', async () => {
  await start();
  capture.onSegment({ ...segment(0), blob: new Blob([new Uint8Array(3 * 1024 * 1024 + 1)]) });
  await settle();
  expect(options.request).not.toHaveBeenCalled();
  expect(controller.getSnapshot()?.phase).toBe('recovery');
});

it('times out permission without opening a late recording session', async () => {
  options.onError = vi.fn();
  controller.start(options);
  await vi.advanceTimersByTimeAsync(30_000);
  expect(controller.getSnapshot()).toBeNull();
  expect(options.onError).toHaveBeenCalledWith(expect.stringContaining('permission timed out'));
  ready();
  await settle();
  expect(controller.getSnapshot()).toBeNull();
  expect(options.deliver).not.toHaveBeenCalled();
});
it('retains recovery when a device cannot finalize its tail', async () => {
  await start();
  capture.onSegment(segment(0));
  await settle();
  stopCapture.mockRejectedValueOnce(new Error('device disconnected'));
  controller.stop();
  await settle();
  expect(controller.getSnapshot()).toMatchObject({
    phase: 'recovery',
    partial: true,
    error: 'device disconnected',
  });
  expect(options.deliver).not.toHaveBeenCalled();
});

it('keeps audio when no actual text is available for a partial delivery', async () => {
  options.request = vi.fn().mockRejectedValue(new Error('offline'));
  await start();
  capture.onSegment(segment(0));
  await settle();
  controller.stop();
  await settle();
  expect(controller.getSnapshot()).toMatchObject({
    phase: 'recovery',
    canRecover: true,
    hasText: false,
  });
  controller.useAvailable();
  await settle();
  expect(options.deliver).not.toHaveBeenCalled();
  expect(controller.getSnapshot()?.retainedDurationMs).toBe(1000);
});

it('keeps capturing when focus changes but the original pane still exists', async () => {
  let focused = true;
  options.valid = () => focused;
  options.captureValid = () => true;
  await start();
  capture.onSegment(segment(0));
  await settle();
  focused = false;
  capture.onProgress?.({ capturedMs: 125000, activeBytes: 0, segments: 1 });
  expect(controller.getSnapshot()?.phase).toBe('recording');
  controller.stop();
  await settle();
  expect(controller.getSnapshot()?.phase).toBe('recovery');
  expect(controller.getSnapshot()?.error).toMatch(/destination changed/);
  expect(options.deliver).not.toHaveBeenCalled();
  expect(useStore.getState().voiceHistory[0].text).toBe('hello');
});

it('keeps unresolved audio when available-text delivery has no valid destination', async () => {
  let destinationValid = true;
  options.valid = () => destinationValid;
  options.request = vi.fn().mockResolvedValueOnce({ text: 'preserved words' }).mockRejectedValueOnce(new Error('offline'));
  await start();
  capture.onSegment(segment(0));
  await settle();
  capture.onSegment(segment(1));
  await settle();
  controller.stop();
  await settle();
  const retained = controller.getSnapshot()!.retainedBytes;
  expect(retained).toBeGreaterThan(0);
  destinationValid = false;
  controller.useAvailable();
  await settle();
  expect(controller.getSnapshot()).toMatchObject({ phase: 'recovery', retainedBytes: retained });
  expect(controller.getSnapshot()?.error).toMatch(/Nothing was inserted/);
  expect(options.deliver).not.toHaveBeenCalled();
  expect(useStore.getState().voiceHistory[0].text).toContain('preserved words');
});

it('exposes a connection action reason while keeping the failed recording', async () => {
  options.request = vi.fn().mockRejectedValue(new VoiceRequestError('Connect this computer', 'not_connected'));
  await start();
  capture.onSegment(segment(0));
  await settle();
  expect(controller.getSnapshot()).toMatchObject({ phase: 'recovery', errorCode: 'not_connected' });
  expect(controller.getSnapshot()!.retainedBytes).toBeGreaterThan(0);
});

it('does not turn a user release into a buffer-full stop on a late progress tick', async () => {
  let finish!: () => void;
  stopCapture.mockImplementationOnce(() => new Promise<void>(resolve => { finish = resolve; }));
  await start();
  capture.onSegment(segment(0));
  await settle();
  controller.stop();
  capture.onProgress?.({ capturedMs: 1000, activeBytes: 8 * 1024 * 1024, segments: 1 });
  expect(controller.getSnapshot()?.stopReason).toBe('user');
  expect(controller.getSnapshot()?.partial).toBe(false);
  finish();
  await settle();
  expect(options.deliver).toHaveBeenCalledWith('hello', expect.objectContaining({ autoSend: true, partial: false }));
});
