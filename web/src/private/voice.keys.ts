// The dictation shortcuts — Alt+S hold-to-talk / tap-to-toggle, Esc to stop,
// Alt+C to cancel — moved out of App's global hotkeys (see voice.register.tsx).
// App calls keydown first for every capture-phase keydown, so these keep
// their priority over the app's own shortcuts.
import type { GlobalKeys } from '../extensions';
import { accountFeaturesAllowed } from '../entitlements';
import { voiceSession } from '../lib/voiceSession';
import { useStore } from '../store';
import type { VoiceControl } from './voice.slice';

// Browser Alt/Option combos can change KeyboardEvent.key on some
// platforms/layouts (macOS Option+S often reports a non-"s" character).
// KeyboardEvent.code keeps the physical key stable.
function isVoiceSKey(e: KeyboardEvent): boolean {
  return e.key.toLowerCase() === 's' || e.code === 'KeyS';
}

function isVoiceShortcutDown(e: KeyboardEvent): boolean {
  if (e.metaKey || !isVoiceSKey(e)) return false;
  if (e.altKey && !e.ctrlKey && !e.shiftKey) return true;
  return false;
}

function isVoiceShortcutRelease(e: KeyboardEvent): boolean {
  return isVoiceSKey(e) || e.key === 'Alt';
}

function isVoiceCancelShortcut(e: KeyboardEvent): boolean {
  return e.altKey &&
    !e.ctrlKey &&
    !e.metaKey &&
    !e.shiftKey &&
    (e.key.toLowerCase() === 'c' || e.code === 'KeyC');
}

function isVoiceCancelKey(e: KeyboardEvent): boolean {
  return e.key.toLowerCase() === 'c' || e.code === 'KeyC';
}

// Voice is deliberately allowed from the pane-owned text capture surface:
// xterm's hidden textarea. Other app inputs (rename, dialogs, pickers) keep
// Alt+S available for typing instead of starting the mic. Kept module-level so
// the exception cannot silently drift without a regression test when the pane
// renderer changes its DOM.
export function blocksVoiceShortcut(el: HTMLElement | null): boolean {
  if (!el) return false;
  if (el.classList.contains('xterm-helper-textarea')) return false;
  const tag = el.tagName.toLowerCase();
  return tag === 'input' || tag === 'textarea' || el.isContentEditable === true;
}

// Voice-chord state lives in this closure, created once per App rather than
// inside App's hotkeys effect, so it survives that effect's re-mount. The
// effect depends on focusedPaneId; if focus changes between the voice-shortcut
// keydown and keyup (ARIA spawns/opens a pane mid-dictation, the user clicks
// another pane), the effect tears down and re-runs. When this state was
// declared with `let` inside the effect, that reset vc.keydownAt to null, so
// the keyup saw "no press in progress" and never stopped the MediaRecorder —
// a stuck mic.
export function createVoiceKeys(): GlobalKeys {
  // Voice-to-text keybind state:
  //   - vc.keydownAt: ms when the current voice shortcut press started; null
  //     between presses. Used in keyup to distinguish TAP (<300ms) from
  //     HOLD (≥300ms).
  //   - vc.armedByKey: true if the CURRENT recording session was
  //     started by keyboard (vs. the mouse button). Makes sure a voice
  //     shortcut can only stop what it started — we don't want a stray tap to
  //     kill a mouse-held recording.
  const HOLD_THRESHOLD_MS = 300;
  const ALT_ONLY_FALLBACK_DELAY_MS = 160;
  // The bare-Alt fallback (for browsers that swallow Alt+S) is off: a plain
  // Alt tap opened a hot mic that kept recording. Alt+S and the mic button
  // remain.
  const altOnlyVoiceFallbackEnabled = false;
  const vc = {
    keydownAt: null as number | null,
    armedByKey: false,
    altTimer: null as number | null,
    altStarted: false,
    // Pin the recorder selected at keydown. A Focus change unregisters that
    // composer's shortcut controller and exposes the terminal fallback, but
    // keyup/cancel must still stop the recorder that actually started.
    control: null as VoiceControl | null,
  };

  const clearAltVoiceFallbackTimer = () => {
    if (vc.altTimer !== null) {
      window.clearTimeout(vc.altTimer);
      vc.altTimer = null;
    }
  };

  const stopAltVoiceFallbackIfStarted = () => {
    clearAltVoiceFallbackTimer();
    if (!vc.altStarted) return;
    const ctrl = vc.control ?? useStore.getState().voiceControl;
    if (ctrl && ['starting', 'recording'].includes(useStore.getState().voiceSession?.phase ?? '')) {
      ctrl.stop(true);
    }
    vc.altStarted = false;
    vc.armedByKey = false;
    vc.keydownAt = null;
    vc.control = null;
  };

  const cancelVoiceShortcut = (): boolean => {
    const phase = useStore.getState().voiceSession?.phase ?? null;
    // Both keydown and keyup reach this helper. Once capture ends,
    // Alt+C belongs to the terminal and must preserve retained audio.
    if (phase !== null && phase !== 'starting' && phase !== 'recording') return false;
    const hadPendingStart = vc.altTimer !== null || vc.keydownAt !== null || vc.armedByKey;
    clearAltVoiceFallbackTimer();
    const ctrl = vc.control ?? useStore.getState().voiceControl;
    if (!ctrl || (!hadPendingStart && phase === null)) return false;
    ctrl.stop(true);
    vc.altStarted = false;
    vc.armedByKey = false;
    vc.keydownAt = null;
    vc.control = null;
    return true;
  };

  const startVoiceShortcut = (startedAt: number, fallbackLabel?: string): boolean => {
    const ctrl = useStore.getState().voiceControl;
    if (!ctrl) {
      useStore.getState().toast('Voice shortcut seen, but recorder is not mounted', {
        kind: 'warn',
        ttlMs: 4000,
      });
      return false;
    }
    // Only `recording` phase can be stopped by the shortcut. `transcribing`
    // is post-mic-release (server is thinking) — let that complete.
    // `null` means idle, ready to start.
    const phase = useStore.getState().voiceSession?.phase ?? null;
    if ((phase === 'recording' || phase === 'starting') && vc.armedByKey && vc.keydownAt === null) {
      // TAP-to-stop: a prior short tap left us recording; this tap
      // commits and transcribes.
      (vc.control ?? ctrl).stop(false);
      vc.armedByKey = false;
      vc.altStarted = false;
      vc.control = null;
      return true;
    }
    if (phase === null) {
      vc.keydownAt = startedAt;
      vc.armedByKey = true;
      vc.control = ctrl;
      if (fallbackLabel) {
        useStore.getState().toast(`Voice shortcut: ${fallbackLabel}`, {
          kind: 'info',
          ttlMs: 1500,
        });
      }
      ctrl.start();
      return true;
    }
    // Recording but started via mouse, OR transcribing. Either way,
    // the voice shortcut shouldn't hijack — mouse-started recordings end by
    // releasing the button, and transcription should run to completion.
    return true;
  };

  return {
    keydown: (e) => {
      const state = useStore.getState();
      const target = e.target as HTMLElement | null;
      // Dictation needs a free account (no recorder is mounted without one).
      const voiceBlocked = !accountFeaturesAllowed() || blocksVoiceShortcut(target);

      const voiceCancelShortcut = isVoiceCancelShortcut(e);
      const keyboardVoicePending = vc.altTimer !== null ||
        vc.keydownAt !== null ||
        vc.armedByKey ||
        vc.altStarted;
      const voiceCancelEvent = voiceCancelShortcut || (keyboardVoicePending && isVoiceCancelKey(e));
      if (e.key !== 'Alt' && !voiceCancelEvent) {
        if (vc.altTimer !== null) clearAltVoiceFallbackTimer();
        else if (vc.altStarted) stopAltVoiceFallbackIfStarted();
      }

      // Esc while recording → stop and transcribe, keeping the words (as
      // Shinoque does); Alt+C is the discard key. Checked BEFORE dialog-close
      // etc. so Esc reaches the recording before anything else reacts. Only
      // fires when voice is actively capturing (not while transcribing — by
      // then the mic is off); Esc propagates normally to HotkeyHelp /
      // CommandPalette otherwise.
      if (e.key === 'Escape' && ['starting', 'recording'].includes(useStore.getState().voiceSession?.phase ?? '')) {
        const ctrl = vc.control ?? useStore.getState().voiceControl;
        if (ctrl) {
          e.preventDefault();
          e.stopImmediatePropagation();
          ctrl.stop(false);
          vc.armedByKey = false;
          vc.keydownAt = null;
          vc.altStarted = false;
          vc.control = null;
          return true;
        }
      }

      // Alt+C — cancel voice recording without reaching for Escape.
      // Benjamin dictates with the left hand on Alt+S; Escape on the
      // top-left corner is an awkward reach with the same hand while
      // Alt+S is still held. Alt+C sits naturally under the middle or
      // index finger of the same hand. Runs on keydown (not keyup) so
      // it fires mid-chord, and only engages during the 'recording'
      // phase — by 'transcribing' the audio is already uploading to
      // Groq and cancelling the UI wouldn't stop that.
      if (
        voiceCancelEvent &&
        (['starting', 'recording'].includes(state.voiceSession?.phase ?? '') ||
          (state.voiceSession === null && keyboardVoicePending))
      ) {
        if (cancelVoiceShortcut()) {
          e.preventDefault();
          // stopImmediatePropagation keeps xterm from interpreting
          // Alt+C as a meta-escape (ESC+c) into the underlying shell.
          e.stopImmediatePropagation();
          return true;
        }
      }

      // Some cloud browsers/OS layers reserve Alt+S before the page ever
      // sees the "S" keydown. When plain Alt arrives and no second key
      // follows quickly, treat it as the swallowed Alt+S voice gesture.
      // Real Alt shortcuts cancel this timer as soon as their keydown
      // arrives, so Alt+H/J/K/L, Alt+Shift+↑/↓, etc. keep working.
      if (
        altOnlyVoiceFallbackEnabled &&
        e.key === 'Alt' &&
        !e.ctrlKey &&
        !e.metaKey &&
        !e.shiftKey &&
        !voiceBlocked
      ) {
        if (!e.repeat && vc.altTimer === null && vc.keydownAt === null) {
          const startedAt = Date.now();
          e.preventDefault();
          e.stopImmediatePropagation();
          vc.altTimer = window.setTimeout(() => {
            vc.altTimer = null;
            if (!document.hasFocus() || document.visibilityState === 'hidden') return;
            if (startVoiceShortcut(startedAt, 'Alt fallback')) {
              vc.altStarted = ['starting', 'recording'].includes(useStore.getState().voiceSession?.phase ?? '')
                && vc.armedByKey
                && vc.keydownAt !== null;
            }
          }, ALT_ONLY_FALLBACK_DELAY_MS);
        }
        return true;
      }

      // Alt+S — hold-to-talk + tap-to-toggle voice recording. Fires the
      // same MediaRecorder path as the mic button. Skipped inside our own
      // dialog inputs (rename, pane title) so you can type a literal "s".
      // Intentionally NOT skipped over xterm — dictating while focused
      // in the terminal is the whole point.
      if (
        isVoiceShortcutDown(e) &&
        !voiceBlocked
      ) {
        if (e.repeat) {
          // OS key-repeat — ~20 Hz while held. The first keydown already
          // started recording; suppress the rest so xterm doesn't see
          // them either (ESC+S burst) and we don't re-enter start().
          e.preventDefault();
          e.stopImmediatePropagation();
          return true;
        }
        e.preventDefault();
        e.stopImmediatePropagation();
        startVoiceShortcut(Date.now());
        return true;
      }
      return false;
    },
    // Voice shortcut keyup — decides tap vs hold.
    // Listen for key release from either supported chord; whichever leaves first
    // ends the chord. Guarded by vc.keydownAt so we only react when
    // the press actually started a recording session.
    keyup: (e) => {
      const keyboardVoicePending = vc.altTimer !== null ||
        vc.keydownAt !== null ||
        vc.armedByKey ||
        vc.altStarted;
      if ((isVoiceCancelShortcut(e) || (keyboardVoicePending && isVoiceCancelKey(e))) && cancelVoiceShortcut()) {
        e.preventDefault();
        e.stopImmediatePropagation();
        return;
      }
      if (e.key === 'Alt' && vc.altTimer !== null) {
        clearAltVoiceFallbackTimer();
        return;
      }
      if (vc.keydownAt === null) return;
      if (!isVoiceShortcutRelease(e)) return;
      clearAltVoiceFallbackTimer();
      const elapsed = Date.now() - vc.keydownAt;
      vc.keydownAt = null;
      if (elapsed < HOLD_THRESHOLD_MS) {
        // TAP — leave recording open. Next voice shortcut keydown commits.
        return;
      }
      // HOLD — release semantics: stop + transcribe. Only stop if the
      // session is actually in the recording phase; a transcribing phase
      // means the user already committed via a fast release earlier.
      const ctrl = vc.control ?? useStore.getState().voiceControl;
      if (ctrl && ['starting', 'recording'].includes(useStore.getState().voiceSession?.phase ?? '')) {
        ctrl.stop(false);
      }
      vc.armedByKey = false;
      vc.altStarted = false;
      vc.control = null;
    },
    // If the window loses focus, always clear pending Alt-only voice fallback:
    // Alt+Tab can blur the app before the page ever sees Tab, and a stale
    // fallback timer must not start recording after focus leaves. If already
    // recording, commit rather than discard: switching windows to read
    // something mid-dictation used to throw the recording away silently.
    blur: () => {
      const voicePhase = useStore.getState().voiceSession?.phase;
      const wasCapturing = voicePhase === 'starting' || voicePhase === 'recording';
      if (wasCapturing) {
        const ctrl = vc.control ?? useStore.getState().voiceControl;
        if (ctrl) {
          voiceSession.stop(false, 'blur');
          useStore.getState().toast(voicePhase === 'starting'
            ? 'Dictation did not start because the window lost focus. Start it again when you are ready.'
            : 'You left the window. Dictation stopped for review.', { kind: 'info', ttlMs: 5000 });
        }
      }
      vc.armedByKey = false;
      vc.keydownAt = null;
      vc.altStarted = false;
      vc.control = null;
      clearAltVoiceFallbackTimer();
    },
    detach: clearAltVoiceFallbackTimer,
  };
}
