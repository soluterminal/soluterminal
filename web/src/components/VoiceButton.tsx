import { beginTerminalInteraction } from '../lib/terminalInteractionState';
import { currentRoutingScope, type RoutingScope } from '../api';
import type { VoiceCleanedBy } from '../private/voice.api';
import { effectiveTerminalTarget, terminalSessionKey } from '../store';
import { terminalSession, captureSessionGesture } from '../lib/terminalViewSessions';
import { useEffect, useRef } from 'react';
import { useContinuousVoice } from '../lib/useContinuousVoice';
import type { CleanupStatus } from 'soluvoice';
import { api } from '../api';
import { isTypingElsewhere } from '../lib/focusGuard';
import { debugLog } from '../debug';
import { useStore } from '../store';
import { enterGapMs } from '../ptySubmit';

import { IconMic, IconBolt, IconPencil } from './ui/icons';
import { effectivePaneAgent } from '../lib/agentKind';
import { paneTypedSince } from '../lib/voiceGuards';
import { trimVoiceContext } from '../lib/voiceRequest';

import { voiceErrorForHost } from '../lib/voiceMicMessage';

import { VOICE_LEAD_IN_MS, type VoicePolish } from '../private/voice.slice';

import { oneShotPolish, VOICE_POLISH_MIN_CHARS } from '../lib/voiceHistory';

/** How the recording started, carried into the voice history entry. */
export type VoiceCaptureMeta = { startDelayMs?: number; warmMic?: boolean };

// Recorder lifecycle (MediaRecorder, slide-to-cancel, push-to-talk
// races, stall watchdog) lives in the soluvoice package — extracted
// 2026-06-12. This file is the soluterminal-specific glue: where a
// transcript may land (pane PTY vs ARIA chat), how it's delivered
// (bracketed paste, split-Enter timing), and the surrounding UX
// (voiceSession chips, history, toasts).

// Tooltip shown when hovering the cleanup status dot. Kept separate
// from the mic's main title so it doesn't clobber the hold-to-talk hint.
function cleanupTooltip(s: CleanupStatus): string {
  switch (s) {
    case 'ok': return 'Last dictation was cleaned up (filler words removed, punctuation fixed).';
    case 'disabled': return 'Cleanup is off on this path. The raw transcript was inserted.';
    case 'skipped': return 'The cleanup result failed a safety check. The raw transcript was inserted.';
    case 'failed': return 'Cleanup was unavailable. The raw transcript was inserted.';
    default: return '';
  }
}

// Where a recording session delivers its transcript. Pinned by
// acquireTarget at record-start — transcription ALWAYS lands here even
// if the user navigates elsewhere while we wait on the transcription
// server. Without this pin the delivery races focus: user dictates
// into pane A, clicks into pane B to read something, transcription
// finishes 4s later, text lands in B.
type VoiceTarget = { kind: 'pane'; paneId: string };

// Raw-then-patch (solu.voiceDeferCleanup) on the TERMINAL path. In review mode
// we ask the server for raw Whisper text, inject it the instant it lands (words
// on screen ~2s sooner than waiting on Haiku), then polish in place: fetch the
// cleanup and, if the input line is still the untouched raw, erase it and
// re-inject the cleaned text. Unlike the composer (which rewrites a controlled
// textarea), here we're editing a live PTY line — so the erase is DEL-per-rune
// and everything is best-effort, falling back to "keep the raw" whenever we
// can't confirm it's safe.
// Abort bound on the cleanup fetch. Haiku cleanup measured ~2s p50 and ~5.4s
// p90 in production, so 2.5s dropped many polishes. 5s is safe now because the
// patch is abandoned if the user typed or pressed Enter since the raw landed
// (voiceGuards), so a slow polish can't clobber their input.
const VOICE_POLISH_CAP_MS = 5000;
// Merge soft-wrapped rows + collapse whitespace so a wrapped input line still
// matches the raw we injected. Used only for the "did the user touch the line?"
// check — never for anything destructive on its own.
function normForGuard(s: string): string {
  // Whitespace removed entirely: a narrow pane can wrap the raw mid-word,
  // which a space-collapsing compare would miss.
  return s.replace(/\s+/g, '');
}

// The input REGION as the guard sees it: the last few non-empty visible rows.
// We check the raw against this — not the exact tail of the whole buffer —
// because a TUI (Claude Code, a REPL) renders chrome BELOW its input (a border,
// a "? for shortcuts" hint), so the raw is never the literal last thing on
// screen. Looking at the last handful of rows lets a bordered input still count
// as "the raw is still here, untouched".
function inputRegionForGuard(snap: string): string {
  const rows = snap.replace(/\r/g, '').split('\n').filter((l) => l.trim() !== '');
  return normForGuard(rows.slice(-6).join(''));
}

type InjectPaneLike = { title?: string; cmd?: string[] } | undefined;

// Kept exported here: tests and older imports reach it through this module.
export { oneShotPolish };

// The terminal raw-then-patch cycle. Module-level (reads fresh store state) so
// the hook's once-captured onText closure can't go stale on it. Exported for
// unit tests — the full VoiceButton needs MediaRecorder, which jsdom lacks.
export async function terminalRawThenPatch(
  rawText: string,
  lockedPaneId: string,
  lockedPane: InjectPaneLike,
  action?: {scope:RoutingScope;valid:()=>boolean},
  meta: VoiceCaptureMeta = {},
): Promise<void> {
  if(action && !action.valid()) return;
  const paneTitle = lockedPane?.title || lockedPane?.cmd?.[0] || '';
  const toast = useStore.getState().toast;
  // Every history entry says what happened to the cleanup pass, so a bad
  // transcript can be traced to the mic start, Whisper, or the cleanup.
  const record = (entry: { text: string; delivered: boolean; polish: VoicePolish; cleanedBy?: VoiceCleanedBy }) => {
    useStore.getState().addVoiceHistoryEntry({
      ...entry,
      paneTitle,
      ...(entry.text !== rawText ? { raw: rawText } : {}),
      ...meta,
    });
  };

  // 1) Show the raw immediately.
  let ok = false;
  const injectedAt = Date.now();
  try {
    if(action && !action.valid()) return;
    await (action ? api.injectPane(lockedPaneId,rawText,action.scope) : api.injectPane(lockedPaneId,rawText));
    ok = true;
  } catch {
    ok = false;
  }
  if (!ok) {
    // Pane died mid-transcription — same clipboard recovery as the normal path.
    let copied = false;
    try {
      await navigator.clipboard?.writeText?.(rawText);
      copied = true;
    } catch { /* clipboard unavailable — surface inline */ }
    record({ text: rawText, delivered: false, polish: 'off' });
    toast(
      copied
        ? 'Target terminal closed before transcription finished — text copied to clipboard, paste it anywhere.'
        : 'Target terminal closed before transcription finished — text: ' + rawText,
      { kind: 'warn', ttlMs: 12000 },
    );
    return;
  }

  // 2) Decide whether to polish at all. Too short → server wouldn't clean it.
  //    Newline in the raw → erasing/​re-injecting a multi-line line in a shell
  //    is unsafe (a stray \n submits), so we leave the raw untouched.
  const patchable = rawText.trim().length >= VOICE_POLISH_MIN_CHARS && !/[\r\n]/.test(rawText);
  if (!patchable) {
    record({ text: rawText, delivered: true, polish: 'short' });
    return;
  }

  // 3) Fetch the cleanup, capped so a slow Haiku (p90 ~5.4s) can't hang the
  //    patch. On abort / failure / unpaired-503 we simply keep the raw.
  const snapshotKey = action ? terminalSessionKey(action.scope, lockedPaneId) : lockedPaneId;
  const context = trimVoiceContext(useStore.getState().paneSnapshots[snapshotKey]?.());
  const controller = new AbortController();
  const capTimer = window.setTimeout(() => controller.abort(), VOICE_POLISH_CAP_MS);
  let cleaned: string | null = null;
  let cleanedBy: VoiceCleanedBy | undefined;
  let polish: VoicePolish = 'failed';
  try {
    const res = await api.cleanupVoice(rawText, { context, signal: controller.signal });
    if (res.cleanup === 'ok' && res.text && res.text.trim() !== '') {
      cleanedBy = res.by;
      if (res.text !== rawText) {
        cleaned = res.text;
        polish = 'cleaned';
      } else {
        polish = 'unchanged';
      }
    } else if (res.cleanup === 'disabled') {
      polish = 'off';
    }
  } catch { /* keep raw */ }
  window.clearTimeout(capTimer);

  // 4) Patch in place ONLY if the raw is still sitting in the input region (the
  //    user hasn't cleared it or scrolled it away by submitting). Erase +
  //    re-inject in ONE write so there's no flash of an empty line. If we can't
  //    read the buffer, patch anyway (best-effort; the cap bounds the window).
  //    Note: this is deliberately lenient about trailing TUI chrome, so if the
  //    user typed a little MORE right after dictating, the erase can catch it —
  //    an accepted best-effort edge, kept rare by the short cap.
  if (action && !action.valid()) {
    record({ text: cleaned ?? rawText, delivered: false, polish, cleanedBy });
    return;
  }
  // Anything the user typed since the raw landed (more words, Enter) means
  // the line is theirs now: patching would erase their input or type the
  // cleaned text into the next prompt.
  if (cleaned !== null && paneTypedSince(lockedPaneId, injectedAt)) {
    record({ text: rawText, delivered: true, polish: 'kept-raw', cleanedBy });
    return;
  }
  if (cleaned !== null) {
    const snap = useStore.getState().paneSnapshots[snapshotKey]?.() ?? '';
    const untouched = snap === '' || inputRegionForGuard(snap).includes(normForGuard(rawText));
    if (!untouched) {
      record({ text: rawText, delivered: true, polish: 'kept-raw', cleanedBy });
      return;
    }
    const erase = '\x7f'.repeat([...rawText].length); // DEL per rune, from end of line
    try {
      if(action && !action.valid()) return;
      await (action ? api.injectPane(lockedPaneId,erase+cleaned,action.scope) : api.injectPane(lockedPaneId,erase+cleaned));
    } catch { /* pane gone mid-patch — raw already delivered */ }
  }
  record({ text: cleaned ?? rawText, delivered: true, polish, cleanedBy });
}

export function VoiceButton({ headless = false, extraClass }: { headless?: boolean; extraClass?: string } = {}) {
  const focusedPaneId = useStore((s) => effectiveTerminalTarget(s));
  const targetAction = useRef<{scope:RoutingScope;valid:()=>boolean;captureValid:()=>boolean}|null>(null);
  const voiceLease = useRef<(() => void) | null>(null);
  useEffect(() => () => { voiceLease.current?.(); voiceLease.current = null; }, []);
  const voiceLanguage = useStore((s) => s.voiceLanguage);
  const keepWarmMs = useStore((s) => s.voiceKeepWarmMs);
  const restorePane = useRef<string | null>(null);
  const previousPhase = useRef('idle');
  const voice = useContinuousVoice<VoiceTarget>({
    // If the user has pinned a language in settings, send it as a
    // Whisper hint — auto-detect is unreliable on short clips. An
    // empty string means "auto" (no hint sent; the hook drops falsy).
    language: voiceLanguage,

    // Start every clip with a short lead-in so the first word isn't
    // clipped (Whisper turns a cut-off "commit and push" into "hit and
    // push"). The mic stays open between dictations only when the user
    // opted in (Settings › Voice); by default keepWarmMs is 0, so the mic
    // opens on press, closes when the clip ends, and the lead-in is silence.
    keepWarmMs,
    leadInMs: VOICE_LEAD_IN_MS,

    // Voice routing: the active terminal pane (focusedPaneId).
    acquireTarget: () => {
      const state = useStore.getState();
      const targetPaneId = effectiveTerminalTarget(state);
      if (!targetPaneId) {
        useStore.getState().toast('Focus a pane first to dictate', { kind: 'warn' });
        return null;
      }
      const scope = currentRoutingScope(); const generation = state.navigationGeneration;
      const pid = state.panes[targetPaneId]?.pid;
      const session = terminalSession(terminalSessionKey(scope,targetPaneId));
      const release = beginTerminalInteraction(terminalSessionKey(scope, targetPaneId));
      if (!release) { state.toast('Retiring previous owner… Try dictating again shortly.', {kind: 'info'}); return null; }
      voiceLease.current?.(); voiceLease.current = release;
      const gesture = session ? captureSessionGesture(session) : ()=>true;
      targetAction.current = {scope,captureValid:()=>currentRoutingScope().key === scope.key && !!useStore.getState().panes[targetPaneId] && useStore.getState().panes[targetPaneId].pid === pid,valid:()=>gesture() && currentRoutingScope().key === scope.key && useStore.getState().navigationGeneration === generation && effectiveTerminalTarget(useStore.getState()) === targetPaneId && useStore.getState().panes[targetPaneId]?.pid === pid};
      restorePane.current = targetPaneId;
      // Terminal dictation always carries the pane's text as context.
      api.voicePrewarm(true);
      return { kind: 'pane', paneId: targetPaneId };
    },

    // Last ~50 lines of the target pane's visible buffer — passed to
    // the cleanup LLM so "use state" can become useState in a React
    // repo, etc. The hook captures this at record-start so the xterm
    // buffer traversal parallelizes with mic init instead of delaying
    // the upload.
    getContext: (target) => {
      const snapFn = useStore.getState().paneSnapshots[terminalSessionKey(currentRoutingScope(),target.paneId)];
      return snapFn ? trimVoiceContext(snapFn()) : undefined;
    },

    // Blur whatever holds focus (usually xterm's hidden helper
    // textarea) so mobile browsers don't pop the virtual keyboard over
    // the recording UI.
    blurOnStart: () => true,

    valid: () => targetAction.current?.valid() ?? false,
    captureValid: () => targetAction.current?.captureValid() ?? false,
    onError: (message, kind) => {
      useStore.getState().toast(voiceErrorForHost(message.replace(/^Transcription failed: /, '')), { kind, ttlMs: kind === 'error' ? 6000 : undefined });
    },

    onText: async (cleanedText, { cleanup, target, autoSend: sessionAutoSend }) => {
      // Capture auto-send flag at dispatch time so toggling mid-flight
      // doesn't change behavior after the user already held-to-record.
      const autoSendSetting = sessionAutoSend && useStore.getState().voiceAutoSend;

      // Deliver via the daemon HTTP inject endpoint, not the
      // WebSocket sender registry. The sender is registered by a
      // mounted Pane.tsx — if the user switched workspaces during
      // transcription, that Pane unmounted and its sender was
      // unregistered, even though the PTY is still alive on the
      // daemon. HTTP inject talks to the daemon directly, so the
      // pinned-target contract survives workspace switches.
      const action = targetAction.current;
      if (!action?.valid()) {
        useStore.getState().toast('Transcript saved in voice history; the terminal view changed.',{kind:'info'});
        return false;
      }
      const lockedPaneId = target.paneId;
      const lockedPane = useStore.getState().panes[lockedPaneId];
      // Auto-send presses Enter for you, which is only safe in an agent's
      // prompt. In a bare shell the dictation would run as a command, so
      // the text is typed for review instead.
      const agentPane = !!lockedPane && (lockedPane.hasClaude === true || effectivePaneAgent(lockedPane) !== null);
      const autoSend = autoSendSetting && agentPane;
      if (autoSendSetting && !agentPane) {
        useStore.getState().toast('Auto-send only presses Enter in Claude and Codex panes. Review this one and press Enter yourself.', { kind: 'info', ttlMs: 5000 });
      }

      // Per-target adapter. Users commonly launch CLIs from inside
      // a shell (e.g. `bash → codex`, `bash → claude`), so cmd[0]
      // reflects bash, not the foreground TUI. That's why earlier
      // cmd-based detection misrouted panes. Correct signals:
      //
      // - Claude Code: `hasClaude` is daemon-detected from Claude's
      //   TUI output, reliable regardless of launch path.
      // - Everything else (Codex, aider, bare shells, …): default
      //   to bracketed paste. Modern TUIs handle \x1b[200~ …
      //   \x1b[201~ as a clean paste event (Codex verified via
      //   Ctrl+Shift+V submitting cleanly). Modern shells with
      //   readline also accept bracketed paste, so dictating at
      //   a bare bash prompt still works.
      //
      // Claude Code keeps the plain-text path because Bubble Tea's
      // bracketed-paste support is version-dependent and v0.0.135
      // regressed Claude when we forced it there. Plain text +
      // 400ms gap is the v0.0.130 known-good path for Claude.
      const exe = (lockedPane?.cmd[0] || '').split('/').pop()?.toLowerCase() || '';
      const hasClaude = lockedPane?.hasClaude === true
        || exe === 'claude' || exe.endsWith('/claude');
      // Bracketed-paste routes past paste-burst detectors entirely,
      // but only matters during auto-send. Without auto-send we
      // keep plain text: the user will review + press Enter
      // themselves, at which point any paste-burst suppression
      // will have long expired, and plain text avoids showing
      // bracketed-paste markers if the foreground doesn't
      // understand them.
      const useBracketedPaste = !!lockedPane && ((autoSend && !hasClaude) || /[\r\n]/.test(cleanedText));
      const payload = useBracketedPaste
        ? `\x1b[200~${cleanedText}\x1b[201~`
        : cleanedText;
      let ok = false;
      try {
        // The pane-header last-prompt strip needs no client-side
        // mirroring here: the DAEMON records prompts at the PTY write
        // choke point (daemon/prompt_recorder.go), so this inject is
        // captured the same as typed input.
        if(!action.valid())return false;
        await api.injectPane(lockedPaneId, payload,action.scope);
        ok = true;
      } catch {
        ok = false;
      }
      // Record the transcription to local history for recovery. We
      // log BEFORE we care about delivery outcome so a pane-closed
      // scenario still preserves the text. `delivered: ok` tags the
      // entry so the history UI can flag "never-landed" ones.
      const toast = useStore.getState().toast;
      if (!ok) {
        // Target pane was killed while we were transcribing. On
        // mobile "here's your transcript in a toast, manually
        // select + copy it into another pane" is painful — we
        // auto-copy to the clipboard as a recovery path so the
        // user can paste wherever they switch next. Best-effort:
        // clipboard write can reject on browsers that require a
        // user gesture in the same task; falling back to the
        // previous inline-text toast keeps the transcript visible
        // so nothing is ever silently lost.
        let copied = false;
        try {
          await navigator.clipboard?.writeText?.(cleanedText);
          copied = true;
        } catch { /* clipboard unavailable — surface the text inline */ }
        toast(
          copied
            ? 'Target terminal closed before transcription finished — text copied to clipboard, paste it anywhere.'
            : 'Target terminal closed before transcription finished — text: ' + cleanedText,
          { kind: 'warn', ttlMs: 12000 },
        );
      } else {
        if (autoSend) {
          // Enter timing — see ptySubmit.ts (enterGapMs) for the
          // rationale and the timing numbers. Mirrored across web
          // (this file) and daemon (pty_submit.go).
          const enterDelay = enterGapMs(useBracketedPaste);
          debugLog('[voice] auto-send \\r after', enterDelay, 'ms');
          const releaseEnter = beginTerminalInteraction(terminalSessionKey(action.scope, lockedPaneId));
          if (releaseEnter) setTimeout(() => {
            if(!action.valid() || !useStore.getState().voiceAutoSend) { releaseEnter(); return; }
            api.injectPane(lockedPaneId, '\r',action.scope)
              .catch(() => { /* pane gone — body already landed */ }).finally(releaseEnter);
          }, enterDelay);
        }
        if (cleanup === 'failed') {
          // Claude API broke (network, 5xx, timeout). Raw Whisper was
          // returned; detail is in Render logs. Operator-facing signal.
          // paneId → click the toast to jump straight to the pane
          // that received the raw transcript, since the user often
          // wants to review/edit it.
          toast('Voice cleanup unavailable — inserted raw transcript', {
            kind: 'warn', ttlMs: 5000, paneId: lockedPaneId,
          });
        } else if (cleanup === 'skipped') {
          // Claude returned off-rails output (refusal, commentary). Our
          // server caught it and fell back to raw. Still worth surfacing
          // so the operator can see the cleanup path isn't trustworthy
          // for this input.
          toast('Voice cleanup output rejected — inserted raw transcript', {
            kind: 'warn', ttlMs: 5000, paneId: lockedPaneId,
          });
        }
      }
      return ok;
    },
  });

  useEffect(() => {
    if (voice.phase === 'idle') {
      voiceLease.current?.(); voiceLease.current = null;
      if (previousPhase.current === 'draining' && restorePane.current && targetAction.current?.valid()) {
        const textarea = document.querySelector<HTMLElement>(`[data-pane-id="${restorePane.current}"] .xterm-helper-textarea`);
        if (textarea && !isTypingElsewhere(textarea)) textarea.focus({ preventScroll: true });
      }
      restorePane.current = null;
    }
    previousPhase.current = voice.phase;
  }, [voice.phase]);

  // Register an imperative start/stop control so App.tsx's global Alt+S
  // handler can invoke this component. The hook's start/stop are stable
  // closures over refs, so registering the mount-time identities once
  // is safe — no re-register per render.
  useEffect(() => {
    const register = useStore.getState().registerVoiceControl;
    const unregister = useStore.getState().unregisterVoiceControl;
    const ctrl = {
      start: voice.start,
      stop: voice.stop,
      preserve: voice.preserve,
    };
    register(ctrl);
    return () => { unregister(ctrl); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const { phase: state, cleanupStatus } = voice;
  const quota = useStore((s) => s.voiceQuota);
  const hasTarget = !!focusedPaneId;
  const quotaNote = quota && quota.limit
    ? ` · ${quota.remaining} of ${quota.limit} left today`
    : quota ? ` · ${quota.remaining} left today` : '';
  const label =
    state === 'recording' ? 'Recording… release to transcribe'
    : (state === 'draining' || state === 'recovery') ? 'Transcribing…'
    : focusedPaneId ? `Hold to talk, or press Space to start and stop${quotaNote}`
    : 'Focus a terminal first';
  // Keyboard users: Space or Enter on the focused mic starts, then commits.
  const onKeyDown = (e: React.KeyboardEvent<HTMLButtonElement>) => {
    if ((e.key !== ' ' && e.key !== 'Enter') || e.repeat) return;
    e.preventDefault();
    if (state === 'idle') voice.start();
    else if (state === 'recording' || state === 'starting') voice.stop(false);
  };

  if (headless) return null;

  return (
    <button
      type="button"
      className={`voice-btn${extraClass ? ` ${extraClass}` : ''} state-${state}`}
      disabled={(state === 'draining' || state === 'recovery') || !hasTarget}
      title={label}
      aria-label={label}
      aria-pressed={state === 'recording'}
      onKeyDown={onKeyDown}
      {...voice.holdHandlers}
    >
      <IconMic size={16} className="voice-btn-icon" aria-hidden />
      {cleanupStatus !== 'none' && (
        <span
          className={`voice-cleanup-dot cleanup-${cleanupStatus}`}
          title={cleanupTooltip(cleanupStatus)}
          aria-hidden
        />
      )}
    </button>
  );
}

// Common Whisper-supported languages we surface. Anything not in the
// list can still be added by the user editing localStorage directly.
// "" = auto-detect (Whisper guesses). Adding an option here just means
// hinting Whisper — downstream Claude cleanup prompt is language-agnostic.
// `label` is the compact form for the mic menu; `name` is the full one for
// Settings.
export const VOICE_LANGS: Array<{ code: string; label: string; name: string }> = [
  { code: '', label: 'Auto', name: 'Auto-detect' },
  { code: 'en', label: 'EN', name: 'English' },
  { code: 'zh', label: '中', name: 'Chinese (中文)' },
  { code: 'ms', label: 'MS', name: 'Malay (Bahasa Melayu)' },
  { code: 'ja', label: '日', name: 'Japanese (日本語)' },
  { code: 'es', label: 'ES', name: 'Spanish (Español)' },
];

// Compact language selector next to the mic. Persists via store
// (localStorage-backed). Default "Auto" keeps the existing
// auto-detect behaviour; users who dictate in one language and keep
// seeing mis-detections can pin it and forget it.
export function VoiceLanguageSelect() {
  const voiceLanguage = useStore((s) => s.voiceLanguage);
  const setVoiceLanguage = useStore((s) => s.setVoiceLanguage);
  return (
    <select
      className="voice-lang"
      value={voiceLanguage}
      onChange={(e) => setVoiceLanguage(e.target.value)}
      title="Voice language hint for Whisper — pick your usual language to avoid short-clip mis-detections"
      aria-label="Voice language"
    >
      {VOICE_LANGS.map((l) => (
        <option key={l.code || 'auto'} value={l.code}>{l.label}</option>
      ))}
    </select>
  );
}

// Small adjacent toggle: 🚀 = transcribed text auto-sends (adds \r),
// ✎ = insert only, user edits + hits Enter themselves. Kept as a
// separate component so the mic button can be rendered standalone.
export function VoiceAutoSendToggle() {
  const voiceAutoSend = useStore((s) => s.voiceAutoSend);
  const setVoiceAutoSend = useStore((s) => s.setVoiceAutoSend);
  return (
    <button
      type="button"
      className={`voice-autosend ${voiceAutoSend ? 'on' : 'off'}`}
      onClick={() => setVoiceAutoSend(!voiceAutoSend)}
      title={voiceAutoSend
        ? 'Voice auto-send ON — transcribed text is submitted immediately. Click to require manual review.'
        : 'Voice review ON — transcribed text is typed into the prompt for you to review, then hit Enter. Click to auto-send.'}
      aria-label={voiceAutoSend ? 'Voice auto-send on' : 'Voice review mode'}
    >
      {voiceAutoSend ? <IconBolt size={15} /> : <IconPencil size={15} />}
    </button>
  );
}
