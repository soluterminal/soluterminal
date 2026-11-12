import { useSyncExternalStore } from 'react';
import { CONNECTED_FOR_VOICE, connectThisComputerAfterWarning } from '../accountConnect';
import { voiceSession, type SessionProgress } from '../lib/voiceSession';
import { VOICE_WARN_AUDIO_BYTES } from '../lib/voiceGuards';
import './VoiceSessionStatus.css';

export function voiceStopLabel(reason: SessionProgress['stopReason']): string {
  switch (reason) {
    case 'duration':
      return 'The 30-minute maximum was reached.';
    case 'segments':
      return 'The recording-part limit was reached.';
    case 'backlog':
      return 'Processing fell behind and the audio buffer is full.';
    case 'text_limit':
      return 'The transcript size limit was reached.';
    case 'blur':
      return 'Recording stopped when you left the window.';
    case 'disposed':
      return 'The destination view closed.';
    case 'interrupted':
      return 'The microphone was interrupted.';
    case 'error':
      return 'A recording part could not be processed.';
    default:
      return 'Recording stopped.';
  }
}
export function VoiceSessionStatus({
  paneId,
  global = false,
}: {
  paneId?: string | null;
  global?: boolean;
}) {
  const state = useSyncExternalStore(
    voiceSession.subscribe,
    voiceSession.getSnapshot,
    voiceSession.getSnapshot,
  );
  if (!state || (!global && state.paneId !== paneId)) return null;
  return (
    <VoiceSessionStatusView
      state={state}
      stop={() => voiceSession.stop()}
      cancel={state.phase === 'recovery' ? voiceSession.discardAudio : voiceSession.cancel}
      retry={voiceSession.retry}
      useAvailable={voiceSession.useAvailable}
      download={voiceSession.download}
      connect={() => { void connectThisComputerAfterWarning(CONNECTED_FOR_VOICE); }}
    />
  );
}
function languageLabel(state: SessionProgress): string {
  const names: Record<string, string> = {
    en: 'English',
    zh: 'Chinese',
    ms: 'Malay',
    ja: 'Japanese',
    es: 'Spanish',
  };
  return state.requestedLanguage
    ? (names[state.requestedLanguage] ?? state.requestedLanguage)
    : 'Auto-detect';
}
export function VoiceSessionStatusView({
  state,
  stop,
  cancel,
  retry,
  useAvailable,
  download,
  connect,
}: {
  state: SessionProgress;
  stop: () => void;
  cancel: () => void;
  retry: () => void;
  useAvailable: () => void;
  download: () => void;
  connect?: () => void;
}) {
  const seconds = Math.floor(state.capturedMs / 1000);
  const time = `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
  const recording = state.phase === 'recording';
  const language = languageLabel(state);
  const message =
    state.phase === 'starting'
      ? 'Preparing microphone…'
      : recording
        ? 'Recording'
        : state.phase === 'recovery'
          ? 'Stopped for review'
          : 'Finishing dictation…';
  return (
    <section className="voice-session-status" aria-label="Voice dictation">
      <div role="status" aria-live="polite" aria-atomic="true">
        {message}
        {state.phase === 'recovery' ? ` · ${voiceStopLabel(state.stopReason)}` : ''}
      </div>
      <div>
        <time aria-label="Recorded duration" aria-live="off">
          {time}
        </time>{' '}
        · {state.completedSegments} of {state.totalSegments} parts processed
        {recording && state.completedSegments < state.totalSegments ? ' · recording continues' : ''}
      </div>
      {(recording || state.phase === 'starting') && (
        <small>{language} · Up to 30 minutes; daily allowance applies · Alt+C to cancel</small>
      )}
      <small>Each audio part (about one minute) uses one cloud transcription.
        {state.quotaRemaining !== undefined ? ` ${state.quotaRemaining} left today.` : ''}
      </small>
      {state.retainedBytes >= VOICE_WARN_AUDIO_BYTES && (
        <p role="status">
          Processing is falling behind. Recording will stop soon to preserve your audio.
        </p>
      )}
      {recording && state.error && (
        <p role="status">Processing paused; recording continues. {state.error} Stop to retry or download your audio.</p>
      )}
      {state.phase === 'recovery' && (
        <>
          <p>
            {state.failedSegment ? `Part ${state.failedSegment} needs attention. ` : ''}
            {state.error}
          </p>
          <p>
            {Math.ceil(state.retainedDurationMs / 1000)} seconds of retained audio. Audio stays in
            memory until retry, discard, or page reload. Download it before reloading.
            {' '}Available text stays in Voice history when you discard audio.
          </p>
          {state.partial && (
            <p>Partial result · review required. Nothing will be submitted automatically.</p>
          )}
          {state.hasText && state.retainedDurationMs > 0 && (
            <p>Using available text clears unprocessed audio. Download it first if you need it.</p>
          )}
        </>
      )}
      <div className="voice-session-actions">
        {(recording || state.phase === 'starting') && (
          <button type="button" onClick={stop}>
            Stop
          </button>
        )}
        <button type="button" onClick={cancel}>
          {state.phase === 'recovery' ? 'Discard audio' : 'Cancel'}
        </button>
        {state.phase === 'recovery' && (
          <>
            {state.errorCode === 'not_connected' && connect && (
              <button type="button" onClick={connect} disabled={state.canRecover === false}>
                Connect this computer
              </button>
            )}
            <button
              type="button"
              onClick={retry}
              disabled={state.canRecover === false || !state.retainedDurationMs}
            >
              Retry remaining
            </button>
            <button
              type="button"
              onClick={useAvailable}
              disabled={state.canRecover === false || state.hasText === false}
            >
              Use available text
            </button>
            <button
              type="button"
              onClick={download}
              disabled={!state.retainedDurationMs || state.canRecover === false}
            >
              Download retained audio
            </button>
          </>
        )}
      </div>
    </section>
  );
}
