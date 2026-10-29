// Shared voice-first chat composer — the input row used by soluterminal's
// mobile quick-reply and native-pane surfaces. Adapted from the ARIA comms
// composer (the reference design we agreed to reuse) and wired to soluterminal's
// existing voice + image backend.
//
// VOICE-FIRST layout: there is no mic button. When the box is EMPTY the textarea
// itself is the recorder (press & hold → record, release → transcribe into the
// draft; slide up → lock hands-free; slide left → cancel). The moment there's
// text it's a plain auto-grow textarea and a ➤ send button appears. The gesture
// engine and the recording overlay come from the composerVoice slot
// (extensions.ts); without it this is a plain text composer with an
// always-visible Send button. This component stays presentational — the
// surface owns the draft, the send path, the attach handler, and the optional
// in-flight Stop.
//
// Layout (left→right): <auto-grow textarea / recorder> [📎], then a far-right ➤
// send (or ⏹ stop when a turn is in flight) that appears only when relevant.

import { useEffect, useRef } from 'react';
import { IconPaperclip, IconSend, IconStop } from './icons';
import { composerVoiceAvailable, slotValue, useComposerVoice } from '../../extensions';
import { useAccountFeatures } from '../../entitlements';
import './composer.css';
import { useUnsentInput } from '../../lib/unsentInput';

export interface ComposerVoiceProps {
  /** Reject delayed dictation after its surface changes scope or target. */
  isTargetCurrent?: (() => boolean) | undefined;
  /** Capture terminal placement at recording start, after async view attach. */
  captureTargetGuard?: (() => () => boolean) | undefined;
  /** Submit the draft after dictation when auto-send (🚀) is on. */
  onSubmitText?: (() => void) | undefined;
  /** Disambiguation context for the cleanup LLM (e.g. the pane's terminal tail). */
  getContext?: (() => string | undefined) | undefined;
  /** Error sink (the surface shows it as a toast). */
  onError?: ((message: string, kind: 'error' | 'warn') => void) | undefined;
  /** Let this composer own App's global Alt+S shortcut while its pane is focused. */
  shortcutActive?: boolean | undefined;
  /** Pane pinned into the global voice session while shortcut recording runs. */
  shortcutPaneId?: string | undefined;
}

export interface ComposerProps {
  // Textarea (the surface owns the ref + value so it can focus / insert / read).
  inputRef: React.RefObject<HTMLTextAreaElement>;
  draft: string;
  placeholder: string;
  onDraftChange: (value: string) => void;
  onKeyDown?: (e: React.KeyboardEvent<HTMLTextAreaElement>) => void;
  onPaste?: (e: React.ClipboardEvent<HTMLTextAreaElement>) => void;
  /** Optional prompt-suggestion shown as dim ghost text over an empty box
   *  (Tab-to-accept is owned by the surface's onKeyDown, not here). */
  suggestion?: string;
  /** IME composition hooks forwarded to the textarea so the surface can gate
   *  Tab-accept (and suppress the ghost) during composition. */
  onCompositionStart?: (e: React.CompositionEvent<HTMLTextAreaElement>) => void;
  onCompositionEnd?: (e: React.CompositionEvent<HTMLTextAreaElement>) => void;

  /** Surface-owned banners (e.g. staged-attachment chips) rendered above the
   *  pill, in the surface's own DOM order. */
  banners?: React.ReactNode;

  // Attach 📎 — the surface's per-pane staging logic runs in onPickFile.
  onPickFile?: ((file: File) => void) | undefined;
  /** `accept` for the hidden file input (default images only — Claude reads
   *  pasted images; arbitrary-file upload needs a daemon endpoint). */
  attachAccept?: string;
  attachDisabled?: boolean;

  // Send button (appears only when there's content; hidden while recording).
  sendDisabled?: boolean;
  /** a11y title for the send button. */
  sendLabel?: string;
  /** Spinner in the send button while a blocking send is in flight. */
  busy?: boolean;
  onSend: () => void;

  // In-flight (native panes): show a Stop/interrupt button instead of Send.
  inFlight?: boolean;
  onStop?: (() => void) | undefined;
  stopping?: boolean;

  /** Voice wiring. Omit for a plain text composer (then a send button always
   *  shows and the textarea is never a recorder). */
  voice?: ComposerVoiceProps | undefined;

  /** Empty-state placeholder shown when the box is a recorder. */
  voiceHint?: string;
}

export function Composer(props: ComposerProps): React.ReactElement {
  const {
    inputRef,
    draft,
    placeholder,
    onDraftChange,
    onKeyDown,
    onPaste,
    banners,
    onPickFile,
    attachAccept = 'image/*',
    attachDisabled,
    sendDisabled,
    sendLabel = 'Send',
    busy,
    onSend,
    inFlight,
    onStop,
    stopping,
    voice,
    voiceHint = 'Hold to talk · or type…',
    suggestion,
    onCompositionStart,
    onCompositionEnd,
  } = props;

  const fileInputRef = useRef<HTMLInputElement>(null);
  // Unsent text holds the unattended updates (lib/unsentInput.ts).
  useUnsentInput(draft);

  // Dictation needs a free account (cloud voice): without one this is a
  // plain text composer with an always-visible Send button.
  const voiceAllowed = useAccountFeatures();
  const voiceEnabled = voice != null && voiceAllowed && composerVoiceAvailable();
  const VoiceRecording = slotValue('composerVoice')?.Recording;
  const cv = useComposerVoice({
    targetRef: inputRef,
    getContext: voice?.getContext,
    isTargetCurrent: voice?.isTargetCurrent,
    captureTargetGuard: voice?.captureTargetGuard,
    onSubmitText: voice?.onSubmitText,
    onError: voice?.onError,
    shortcutActive: voice?.shortcutActive,
    shortcutPaneId: voice?.shortcutPaneId,
    enabled: voiceEnabled,
  });

  // Auto-grow the textarea to fit its content (capped by the CSS max-height).
  useEffect(() => {
    const el = inputRef.current;
    if (el === null) return;
    el.style.height = 'auto';
    el.style.height = `${el.scrollHeight}px`;
  }, [draft, inputRef]);

  const hasContent = draft.trim().length > 0;
  const recording = cv.recording || cv.transcribing;

  // Raw-then-patch send gate (voiceDeferCleanup): while the polish call is in
  // flight AND within its cap, a bare Enter is swallowed and Send is disabled,
  // so raw dictation can't be fired before it's polished. Shift+Enter (newline)
  // and IME-composition Enters pass through untouched. Everything else is
  // byte-identical to the plain composer — cv.submitBlocked is always false
  // unless the experimental flag is on.
  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (cv.submitBlocked && e.key === 'Enter' && !e.shiftKey
      && !(e.nativeEvent as unknown as { isComposing?: boolean }).isComposing) {
      e.preventDefault();
      return;
    }
    onKeyDown?.(e);
  };
  const handleSend = () => { if (!cv.submitBlocked) onSend(); };
  // Ghost-text suggestion shows only over an EMPTY, non-recording box — typing
  // or dictation hides it. pointer-events:none on the overlay keeps the textarea
  // interactive.
  const showGhost = !!suggestion && !hasContent && !recording;
  // Gestures attach whenever the box is empty — INCLUDING while recording. A
  // hold flips the box into "recording" mid-gesture; if we detached the handlers
  // then, the release (touchend / pointerup) would never fire and recording
  // would never stop. The moment there's text the box is a normal textarea.
  const gesturesOn = voiceEnabled && !hasContent;
  // Primary button: Stop while a turn is in flight; otherwise Send when there's
  // content (always for non-voice composers); nothing for the empty recorder or
  // while recording.
  const showStop = inFlight === true;
  const showSend = !showStop && !recording && (voiceEnabled ? hasContent : true);
  const placeholderText = showGhost
    ? ''
    : voiceEnabled && !hasContent && !recording
      ? voiceHint
      : placeholder;

  return (
    <>
      {banners}
      <div className="composer-row">
        <div className="composer-pill">
          <div className={`composer-input-wrap${cv.polishing ? ' is-polishing' : ''}`}>
            {/* Never disabled: a disabled textarea drops focus mid-send, which
                dismisses + re-pops the Android keyboard. Kept mounted while
                recording (hidden) so dictation still inserts at its target. */}
            <textarea
              ref={inputRef}
              className={`composer-input${gesturesOn ? ' is-voice-empty' : ''}`}
              placeholder={placeholderText}
              value={draft}
              onChange={(e) => { cv.onComposerInput(e.target.value); onDraftChange(e.target.value); }}
              onKeyDown={handleKeyDown}
              onPaste={onPaste}
              onCompositionStart={onCompositionStart}
              onCompositionEnd={onCompositionEnd}
              enterKeyHint="send"
              rows={1}
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              autoComplete="off"
              style={recording ? { visibility: 'hidden' } : undefined}
              {...(gesturesOn ? cv.handlers : {})}
            />
            {showGhost && (
              <div className="composer-ghost" aria-hidden="true">
                {suggestion}
              </div>
            )}
            {cv.polishing && !recording && (
              <div className="composer-polishing" role="status" aria-live="polite">
                <span className="composer-polishing-dot" aria-hidden="true" />
                polishing…
              </div>
            )}
            {recording && VoiceRecording && <VoiceRecording voice={cv} />}
          </div>
          {onPickFile && (
            <>
              <input
                ref={fileInputRef}
                type="file"
                accept={attachAccept}
                style={{ display: 'none' }}
                onChange={(e) => {
                  const f = e.target.files?.[0];
                  if (f) onPickFile(f);
                  if (e.target) e.target.value = '';
                }}
              />
              <button
                type="button"
                className="composer-btn"
                onClick={() => fileInputRef.current?.click()}
                disabled={attachDisabled}
                aria-label="Attach image"
                title="Attach an image"
              >
                <IconPaperclip />
              </button>
            </>
          )}
        </div>

        <div className="composer-primary-slot">
          {showStop && (
            <button
              type="button"
              className="composer-primary is-stop"
              onClick={onStop}
              disabled={stopping}
              aria-label="Interrupt the current turn"
              title="Stop"
            >
              <IconStop />
            </button>
          )}
          {showSend && (
            <button
              type="button"
              className="composer-primary is-send"
              onClick={handleSend}
              disabled={sendDisabled || cv.submitBlocked}
              aria-label={cv.submitBlocked ? 'Polishing — send in a moment' : sendLabel}
              title={cv.submitBlocked ? 'Polishing your dictation…' : sendLabel}
            >
              {busy === true || cv.submitBlocked ? <span className="composer-spinner" aria-hidden="true" /> : <IconSend />}
            </button>
          )}
        </div>
      </div>
    </>
  );
}
