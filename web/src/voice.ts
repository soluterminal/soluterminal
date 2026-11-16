import { isEmbedded } from './embed';
import { playChime } from './lib/chime';

// Desktop-only voice alerts for high-level events. Gated on Tauri webview
// detection because spoken alerts are meant for a desktop speaker; phones
// already have their own ringtone/notification sound for the same signals.
//
// Bounded vocabulary (4 events) means we ship pre-rendered MP3s rather
// than running live TTS. Every install gets identical premium-quality
// playback, zero runtime cost, zero network dependency.

export type VoiceEvent = 'approval' | 'reply-ready' | 'schedule-soon' | 'schedule-fired' | 'update-available';
// 'chime' = short synthesized tones instead of speech (lib/chime.ts). Unlike
// the spoken clips it also plays in a browser tab.
export type VoiceMode = 'off' | 'sonia' | 'ryan' | 'chime';

// Namespaced key: solu.voice* is already used by the speech-input feature
// (voiceAutoSend, voiceLanguage), so "voiceAlertMode" keeps output-side
// alerts distinct from input-side speech recognition.
const VOICE_KEY = 'solu.voiceAlertMode';

// Only true inside the Tauri webview. A browser tab pointed at the local
// daemon won't speak — matches the "desktop speaker only, not the PWA"
// scope.
//
// Primary signal is `window.__SOLU_TAURI__`, set by an
// `initialization_script` on the WebviewWindowBuilder in lib.rs. We set
// our own flag because Tauri's default globals (`__TAURI__`,
// `__TAURI_INTERNALS__`) aren't reliably present when the frontend is
// served from an external URL (our daemon's HTTP server) rather than
// bundled Tauri assets. The standard globals are still checked as a
// fallback in case config changes in the future.
export function isTauri(): boolean {
  if (typeof window === 'undefined') return false;
  if (isEmbedded()) return false;
  const w = window as unknown as {
    __SOLU_TAURI__?: unknown;
    __TAURI__?: unknown;
    __TAURI_INTERNALS__?: unknown;
    __TAURI_METADATA__?: unknown;
  };
  return !!(
    w.__SOLU_TAURI__ ||
    w.__TAURI__ ||
    w.__TAURI_INTERNALS__ ||
    w.__TAURI_METADATA__
  );
}

export function getVoiceMode(): VoiceMode {
  try {
    const v = localStorage.getItem(VOICE_KEY);
    if (v === 'sonia' || v === 'ryan' || v === 'off' || v === 'chime') return v;
  } catch { /* ignore */ }
  // Default: a short chime. A talking voice is a surprising first
  // impression; Sonia/Ryan stay one click away in the alerts menu, and any
  // saved choice (including 'off') wins above.
  return 'chime';
}

export function setVoiceMode(m: VoiceMode): void {
  try { localStorage.setItem(VOICE_KEY, m); } catch { /* ignore */ }
}

// Per-event cooldown. Prevents a flappy state (e.g. pane oscillating
// between busy and awaiting-input) from becoming a voice-alert barrage.
// The Go-side pusher already debounces push notifications to 1/min/pane;
// this is the browser-side analogue for the voice path.
const lastPlayedAt: Record<VoiceEvent, number> = {
  approval: 0,
  'reply-ready': 0,
  'schedule-soon': 0,
  'schedule-fired': 0,
  'update-available': 0,
};
const COOLDOWN_MS = 10_000;

// Spoken fallback text for each event. Used when the pre-rendered MP3
// is missing (404) or the browser refuses to decode it — rather than
// silently dropping the cue we synthesize it via the browser's built-in
// SpeechSynthesis. Quality is lower than the pre-rendered Sonia/Ryan
// clips (it'll use whatever OS voice happens to be installed), but the
// cue still lands, which matters more than pitch-perfect audio for a
// "heads up" alert. Benjamin can drop a real recording into
// web/public/voice/{sonia,ryan}/<event>.mp3 anytime to override.
const SPOKEN_FALLBACK: Record<VoiceEvent, string> = {
  approval: 'approval needed',
  'reply-ready': 'reply ready',
  'schedule-soon': 'automation runs soon',
  'schedule-fired': 'automation ran',
  'update-available': 'update available',
};

function speakFallback(event: VoiceEvent): void {
  try {
    const w = window as unknown as { speechSynthesis?: SpeechSynthesis };
    if (!w.speechSynthesis) return;
    const text = SPOKEN_FALLBACK[event];
    if (!text) return;
    const utter = new SpeechSynthesisUtterance(text);
    utter.volume = 0.9;
    utter.rate = 1.0;
    w.speechSynthesis.speak(utter);
  } catch {
    /* ignore — speech synthesis is strictly best-effort */
  }
}

export function playVoice(event: VoiceEvent): void {
  const mode = getVoiceMode();
  if (mode === 'off') return;
  if (mode !== 'chime' && !isTauri()) return;

  const now = Date.now();
  if (now - lastPlayedAt[event] < COOLDOWN_MS) return;
  lastPlayedAt[event] = now;

  if (mode === 'chime') {
    playChime(event);
    return;
  }

  try {
    const a = new Audio(`/voice/${mode}/${event}.mp3`);
    a.volume = 0.9;
    // Multi-layer fallback: the `error` event fires when the HTTP fetch
    // fails (missing file, 404); `.play().catch` fires when the browser
    // refuses autoplay. Either way, fall back to SpeechSynthesis so the
    // cue still lands.
    a.addEventListener('error', () => speakFallback(event), { once: true });
    void a.play().catch(() => speakFallback(event));
  } catch {
    /* ignore */
  }
}
