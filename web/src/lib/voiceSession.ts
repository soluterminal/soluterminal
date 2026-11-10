import {
  createContinuousCapture,
  type ContinuousCapture,
  type CaptureSegment,
  type CaptureProgress,
  type CaptureStopReason,
  type TranscribeResult,
} from 'soluvoice';
import { useStore } from '../store';
import { api } from '../api';
import type { VoiceContextCheck } from '../private/voice.api';
import { aggregateVoiceSegments, type VoiceSegmentResult } from './voiceHistory';
import { isNoSpeech, trimVoiceContext, utf8Bytes, validateVoiceUpload, VoiceRequestError } from './voiceRequest';
import { withBusyRetry } from './voiceBusyRetry';
import {
  VOICE_MAX_RECORD_MS,
  VOICE_MAX_SEGMENTS,
  VOICE_MAX_AUDIO_BYTES,
  VOICE_AUDIO_RESERVE_BYTES,
  VOICE_SEGMENT_TEXT_BYTES,
  VOICE_SESSION_TEXT_BYTES,
} from './voiceGuards';

export type SessionPhase = 'starting' | 'recording' | 'draining' | 'recovery';
type SessionTranscribeResult = TranscribeResult & {
  quota?: { remaining: number; limit: number | null } | null;
};
export interface SessionProgress {
  sessionId: string;
  phase: SessionPhase;
  paneId: string | null;
  capturedMs: number;
  completedSegments: number;
  totalSegments: number;
  retainedBytes: number;
  retainedDurationMs: number;
  stopReason?: CaptureStopReason;
  error?: string;
  errorCode?: VoiceRequestError['code'];
  failedSegment?: number;
  partial: boolean;
  startDelayMs?: number;
  warm?: boolean;
  requestedLanguage?: string;
  canRecover?: boolean;
  hasText?: boolean;
  quotaRemaining?: number;
}
export interface SessionDelivery {
  sessionId: string;
  autoSend: boolean;
  partial: boolean;
  stopReason: CaptureStopReason;
  rawText: string;
  cleanup: string;
  startDelayMs: number;
  warm: boolean;
}
export interface StartSession {
  paneId: string | null;
  title: string;
  language: string;
  context?: string;
  deferCleanup: boolean;
  autoSend: boolean;
  keepWarmMs: number;
  valid: () => boolean;
  /** Capture may continue while delivery is temporarily unavailable. */
  captureValid?: () => boolean;
  deliver: (text: string, info: SessionDelivery) => boolean | void | Promise<boolean | void>;
  onReady?: () => void;
  onError?: (message: string) => void;
  request?: (blob: Blob, signal: AbortSignal) => Promise<SessionTranscribeResult>;
}
interface Part extends Omit<CaptureSegment, 'blob'> {
  blob?: Blob;
  result?: VoiceSegmentResult;
}
export type CaptureFactory = typeof createContinuousCapture;

const DESTINATION_CHANGED = 'The original destination changed. Nothing was inserted. Your transcript is in Voice history; any unprocessed audio is kept here.';
const ALLOWANCE_STOP = 'Your daily voice allowance limits this recording. Recording stopped for review. Available text and any unprocessed audio are kept here.';

/** One bounded serial pipeline. No settings are read again to build requests. */
export function segmentRequest(
  snapshot: Pick<StartSession, 'language' | 'context' | 'deferCleanup'>,
  contextCheck?: VoiceContextCheck,
) {
  const language = snapshot.language;
  const context = trimVoiceContext(snapshot.context);
  return async (blob: Blob, signal: AbortSignal): Promise<SessionTranscribeResult> => {
    const res =
      await withBusyRetry(
        (s) =>
          api.transcribe(blob, {
            filename: 'audio.wav',
            language,
            context: snapshot.deferCleanup ? undefined : context,
            contextCheck,
            deferCleanup: snapshot.deferCleanup,
            signal: s,
          }),
        signal,
        { bounded: true },
      );
    if (res.quota) useStore.getState().setVoiceQuota(res.quota);
    if (signal.aborted) throw signal.reason;
    const raw = res.rawText ?? res.text;
    if (utf8Bytes(raw) + utf8Bytes(res.text) > VOICE_SEGMENT_TEXT_BYTES)
      throw new Error('Transcript response reached the text limit. Recording is kept.');
    if (!snapshot.deferCleanup || res.cleanup !== 'deferred' || isNoSpeech(res.text))
      return { ...res, text: isNoSpeech(res.text) ? '' : res.text, rawText: raw };
    // The text-only call is in this same pipeline. Failed cleanup keeps raw;
    // it never patches a terminal or re-uploads successful audio.
    try {
      const cleaned = await withBusyRetry(
        (s) => api.cleanupVoice(res.text, { context, contextCheck, language, signal: s }),
        signal,
        { bounded: true },
      );
      return {
        ...res,
        ...cleaned,
        rawText: raw,
        requestedLanguage: res.requestedLanguage ?? (language || 'auto'),
        detectedLanguage: res.detectedLanguage,
        audioDurationMs: res.audioDurationMs,
        transcriptionProvider: res.transcriptionProvider,
      };
    } catch (error) {
      if (signal.aborted) throw error;
      return { ...res, rawText: raw, cleanup: 'failed' };
    }
  };
}

/** Module ownership keeps recovery alive after a destination component unmounts. */
export class VoiceSessionController {
  private state: SessionProgress | null = null;
  private listeners = new Set<() => void>();
  private options: StartSession | null = null;
  private capture: ContinuousCapture | null = null;
  private parts: Part[] = [];
  private abort: AbortController | null = null;
  private activeBytes = 0;
  private busy = false;
  private stopped = false;
  private stopping = false;
  private delivered = false;
  private detached = false;
  private epoch = 0;
  private progressAt = 0;
  private quotaRemaining: number | null = null;
  private processingPaused = false;
  private timer?: ReturnType<typeof setTimeout>;
  constructor(private factory: CaptureFactory = createContinuousCapture) {}
  subscribe = (listener: () => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };
  getSnapshot = () => this.state;
  private publish(patch: Partial<SessionProgress> = {}) {
    if (!this.state) return;
    const retainedBytes =
      this.parts.reduce((n, p) => n + (p.blob?.size ?? 0), 0) + this.activeBytes;
    this.state = {
      ...this.state,
      ...patch,
      canRecover: this.stopped && !this.busy,
      hasText: this.parts.some(
        (part) => !!part.result?.text.trim(),
      ),
      retainedBytes,
      retainedDurationMs: this.parts.reduce((n, p) => n + (p.blob ? p.durationMs : 0), 0),
      completedSegments: this.parts.filter((p) => p.result).length,
      totalSegments: this.parts.length,
    };
    useStore.getState().setVoiceSession(this.state);
    this.listeners.forEach((fn) => fn());
  }
  private checkpoint(delivered = false, partial = this.state?.partial ?? false) {
    if (!this.state || !this.options) return;
    const aggregate = aggregateVoiceSegments(this.parts.map((p) => p.result));
    if (!this.parts.some((part) => {
      const result = part.result;
      return !!(result?.text.trim() || result?.rawText?.trim());
    })) return;
    useStore.getState().addVoiceHistoryEntry({
      sessionId: this.state.sessionId,
      text: aggregate.text,
      raw: aggregate.raw,
      paneTitle: this.options.title,
      delivered,
      partial: partial || aggregate.partial,
      startDelayMs: this.state.startDelayMs,
      warmMic: this.state.warm,
      durationMs: this.state.capturedMs,
      totalSegments: this.parts.length,
      completedSegments: this.parts.filter((p) => p.result).length,
      stopReason: this.state.stopReason,
      requestedLanguage: this.options.language || 'auto',
      detectedLanguage: aggregate.detectedLanguage,
      segments: this.parts
        .flatMap((p) => (p.result ? [p.result] : []))
        .slice(0, VOICE_MAX_SEGMENTS),
    });
  }
  start(options: StartSession): boolean {
    if (this.state) return false;
    const epoch = ++this.epoch;
    // The policy check runs alongside mic start-up (see startVoiceContextCheck).
    this.options = { ...options, request: options.request ?? segmentRequest(options, api.startVoiceContextCheck()) };
    this.progressAt = 0;
    this.parts = [];
    this.busy = false;
    this.stopped = false;
    this.stopping = false;
    this.delivered = false;
    this.detached = false;
    this.activeBytes = 0;
    this.quotaRemaining = null;
    this.processingPaused = false;
    this.state = {
      sessionId: `voice-${Date.now()}-${epoch}`,
      phase: 'starting',
      paneId: options.paneId,
      capturedMs: 0,
      completedSegments: 0,
      totalSegments: 0,
      retainedBytes: 0,
      retainedDurationMs: 0,
      partial: false,
      requestedLanguage: options.language,
    };
    this.publish(); // synchronous registration precedes getUserMedia
    this.capture = this.factory({
      leadInMs: 300,
      keepWarmMs: options.keepWarmMs,
      onSegment: (part) => {
        if (epoch !== this.epoch) return;
        this.activeBytes = 65536;
        this.parts.push({ ...part });
        this.publish();
        this.checkpoint();
        this.guardAllowance();
        if (this.parts.length >= VOICE_MAX_SEGMENTS - 1 && !this.stopping)
          this.stop(false, 'segments');
        void this.pump(epoch);
      },
      onProgress: (progress: CaptureProgress) => {
        if (epoch !== this.epoch) return;
        // Reserve transient PCM-to-container copies as well as both worklet buffers.
        this.activeBytes = progress.activeBytes * 2;
        const bytes = this.parts.reduce((n, p) => n + (p.blob?.size ?? 0), 0) + this.activeBytes;
        if (Date.now() - this.progressAt >= 250 || this.stopping) {
          this.progressAt = Date.now();
          this.publish({
            capturedMs: progress.capturedMs,
            startDelayMs: progress.startDelayMs,
            warm: progress.warm,
          });
        }
        if (!(this.options?.captureValid?.() ?? this.options?.valid()) && !this.stopping) this.stop(false, 'disposed');
        if (!this.stopping && !this.stopped && bytes >= VOICE_MAX_AUDIO_BYTES - VOICE_AUDIO_RESERVE_BYTES) this.stop(false, 'backlog');
      },
      onInterrupted: (message) => {
        if (epoch !== this.epoch) return;
        this.publish({ error: message });
        this.stop(false, 'interrupted');
      },
    });
    this.timer = setTimeout(() => {
      if (epoch !== this.epoch) return;
      options.onError?.('Microphone permission timed out. Allow access and try again.');
      this.cancel();
    }, 30_000);
    void this.capture
      .start()
      .then(() => {
        if (epoch !== this.epoch || this.state?.phase !== 'starting') return;
        clearTimeout(this.timer);
        this.publish({ phase: 'recording' });
        options.onReady?.();
        this.timer = setTimeout(() => this.stop(false, 'duration'), VOICE_MAX_RECORD_MS);
      })
      .catch((error) => {
        if (epoch !== this.epoch) return;
        options.onError?.(error instanceof Error ? error.message : 'Microphone unavailable');
        this.cancel();
      });
    return true;
  }
  stop(cancel = false, reason: CaptureStopReason = 'user') {
    if (cancel) {
      this.cancel();
      return;
    }
    if (!this.state) return;
    if (this.state.phase === 'starting') {
      this.cancel();
      return;
    }
    if (reason !== 'user') this.publish({ stopReason: reason, partial: true });
    if (this.stopping || this.stopped) return;
    this.stopping = true;
    clearTimeout(this.timer);
    this.publish({
      phase: this.state.phase === 'recovery' ? 'recovery' : 'draining',
      stopReason: this.state.stopReason ?? reason,
    });
    const epoch = this.epoch;
    void this.capture
      ?.stop()
      .then(() => {
        if (epoch !== this.epoch) return;
        this.stopped = true;
        this.activeBytes = 0;
        this.publish();
        this.checkpoint();
        void this.pump(epoch);
      })
      .catch((error) => {
        if (epoch !== this.epoch) return;
        this.stopped = true;
        this.activeBytes = 0;
        this.publish({
          phase: 'recovery',
          partial: true,
          stopReason: 'interrupted',
          error: error instanceof Error ? error.message : 'Recording stopped unexpectedly.',
        });
        this.checkpoint();
      });
  }
  private async pump(epoch: number) {
    if (epoch !== this.epoch || this.busy || this.state?.phase === 'recovery') return;
    if (this.processingPaused) {
      if (this.stopped) this.publish({ phase: 'recovery' });
      return;
    }
    const part = this.parts.find((p) => !p.result);
    if (!part) {
      if (this.stopped) {
        const destinationChanged = this.detached || !this.options?.valid();
        if (this.state?.partial || destinationChanged) {
          this.publish({ phase: 'recovery', ...(destinationChanged ? { error: DESTINATION_CHANGED } : {}) });
          this.checkpoint();
        } else await this.deliver(false);
      }
      return;
    }
    if (this.quotaRemaining === 0) {
      this.publish({ phase: 'recovery', partial: true, errorCode: 'quota', error: ALLOWANCE_STOP });
      this.stop(false, 'segments');
      this.checkpoint();
      return;
    }
    this.busy = true;
    const abort = new AbortController();
    this.abort = abort;
    try {
      validateVoiceUpload(
        part.blob!,
        { language: this.options!.language, context: this.options!.context },
        'audio.wav',
      );
      const result = await this.options!.request!(part.blob!, abort.signal);
      if (epoch !== this.epoch || abort.signal.aborted) return;
      const size = utf8Bytes(result.text) + utf8Bytes(result.rawText ?? result.text);
      const used = this.parts.reduce(
        (n, p) =>
          n +
          (p.result ? utf8Bytes(p.result.text) + utf8Bytes(p.result.rawText ?? p.result.text) : 0),
        0,
      );
      if (size > VOICE_SEGMENT_TEXT_BYTES || size + used > VOICE_SESSION_TEXT_BYTES)
        throw new Error(
          'Transcript reached the text limit. Recording parts are kept for recovery.',
        );
      // Allow only bounded non-content metadata from an older or newer server.
      const bounded = (v: string | undefined) => v?.replace(/[^a-zA-Z0-9_./ -]/g, '').slice(0, 64);
      part.result = {
        index: part.index,
        text: result.text,
        rawText: result.rawText,
        requestedLanguage: this.options!.language || 'auto',
        detectedLanguage: bounded(result.detectedLanguage),
        audioDurationMs:
          result.audioDurationMs && Number.isFinite(result.audioDurationMs)
            ? result.audioDurationMs
            : part.durationMs,
        transcriptionProvider: bounded(result.transcriptionProvider),
        cleanupProvider: bounded(result.cleanupProvider),
        cleanup: bounded(result.cleanup),
      };
      part.blob = undefined; // success never retransmits audio
      if (result.quota && Number.isFinite(result.quota.remaining))
        this.quotaRemaining = Math.max(0, Math.floor(result.quota.remaining));
      else if (this.quotaRemaining !== null)
        this.quotaRemaining = Math.max(0, this.quotaRemaining - 1);
      this.publish({ quotaRemaining: this.quotaRemaining ?? undefined });
      this.checkpoint();
      this.guardAllowance();
    } catch (error) {
      if (epoch !== this.epoch || abort.signal.aborted) return;
      this.processingPaused = true;
      const terminal = error instanceof VoiceRequestError && ['quota', 'not_connected', 'too_long'].includes(error.code);
      this.publish({
        phase: this.stopping || this.stopped || terminal ? 'recovery' : 'recording',
        partial: true,
        stopReason: 'error',
        failedSegment: part.index + 1,
        errorCode: error instanceof VoiceRequestError ? error.code : undefined,
        error:
          error instanceof Error ? error.message : 'This recording part could not be processed.',
      });
      if (terminal) this.stop(false, 'error');
      this.checkpoint();
    } finally {
      if (epoch === this.epoch) {
        this.busy = false;
        this.abort = null;
        this.publish();
        void this.pump(epoch);
      }
    }
  }
  retry = () => {
    if (!this.state || this.state.phase !== 'recovery' || !this.stopped || this.busy) return;
    // An explicit retry may be on a new allowance day; the service remains
    // authoritative. Never automatically probe again after exhaustion.
    this.quotaRemaining = null;
    this.processingPaused = false;
    this.publish({ phase: 'draining', partial: false, error: undefined, errorCode: undefined, failedSegment: undefined });
    void this.pump(this.epoch);
  };
  private guardAllowance() {
    if (this.stopping || this.stopped || this.quotaRemaining === null) return;
    // Reserve a request for capture.stop()'s tail, as well as queued parts.
    const pending = this.parts.filter((part) => !part.result).length;
    if (pending + 1 < this.quotaRemaining) return;
    this.publish({ errorCode: 'quota', error: ALLOWANCE_STOP });
    this.stop(false, 'segments');
  }
  useAvailable = () => {
    if (this.stopped && !this.busy) void this.deliver(true);
  };
  private async deliver(partial: boolean) {
    if (!this.state || !this.options || this.delivered) return;
    const epoch = this.epoch;
    const aggregate = aggregateVoiceSegments(this.parts.map((p) => p.result));
    if (
      !this.parts.some((part) => !!part.result?.text.trim())
    ) {
      if (!partial) this.cancel();
      return;
    }
    if (this.detached || !this.options.valid()) {
      this.publish({ phase: 'recovery', error: DESTINATION_CHANGED });
      this.checkpoint(false, partial || aggregate.partial);
      return;
    }
    this.delivered = true;
    const autoSend =
      !partial &&
      !aggregate.partial &&
      this.state.stopReason === 'user' &&
      this.options.autoSend &&
      useStore.getState().voiceAutoSend;
    let delivered = false;
    this.checkpoint(false, partial || aggregate.partial);
    try {
      if (!this.detached && this.options.valid() && aggregate.text.trim()) {
        const result = await this.options.deliver(aggregate.text, {
          sessionId: this.state.sessionId,
          autoSend,
          partial: partial || aggregate.partial,
          stopReason: this.state.stopReason ?? 'user',
          startDelayMs: this.state.startDelayMs ?? 0,
          warm: this.state.warm ?? false,
          rawText: aggregate.raw,
          cleanup: this.parts.some((p) => p.result?.cleanup === 'failed') ? 'failed' : 'ok',
        });
        delivered = result !== false;
      }
    } catch (error) {
      this.options?.onError?.(
        error instanceof Error
          ? error.message
          : 'Transcript delivery failed. Your words are saved in voice history.',
      );
    } finally {
      if (epoch === this.epoch) {
        this.checkpoint(delivered, partial || aggregate.partial);
        this.release();
      }
    }
  }
  download = () => {
    for (const part of this.parts) {
      if (!part.blob) continue;
      const url = URL.createObjectURL(part.blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `voice-part-${part.index + 1}.wav`;
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    }
  };
  detach = (sessionId: string) => {
    if (this.state?.sessionId !== sessionId) return;
    this.detached = true;
    if (this.state.phase === 'starting') {
      this.cancel();
      return;
    }
    this.abort?.abort();
    // Abort is ambiguous; no automatic retransmission. Retain unresolved bytes.
    this.publish({ phase: 'recovery', partial: true });
    this.stop(false, 'disposed');
    this.checkpoint();
  };
  cancel = () => {
    if (this.state) useStore.getState().removeVoiceCheckpoint(this.state.sessionId);
    this.release();
  };
  discardAudio = () => {
    if (this.state?.phase !== 'recovery') return;
    // Recovery can outlive its destination. Clearing its audio must not
    // delete the transcript the review panel promised to keep in history.
    this.checkpoint();
    this.release();
  };
  private release() {
    ++this.epoch;
    clearTimeout(this.timer);
    this.abort?.abort();
    this.abort = null;
    this.capture?.dispose();
    this.capture = null;
    this.parts = [];
    this.options = null;
    this.state = null;
    this.activeBytes = 0;
    useStore.getState().setVoiceSession(null);
    this.listeners.forEach((fn) => fn());
  }
}
export const voiceSession = new VoiceSessionController();
