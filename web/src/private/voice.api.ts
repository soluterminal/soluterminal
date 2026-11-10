// Voice dictation: transcription, cleanup, prewarm and the dictation settings
// (see voice.register.tsx). Merged into `api` by api.ts.
import type { VoiceDiagnostics } from 'soluvoice';
import { apiFetch, currentApiPrefix, currentPrefix, j, rootFetch } from '../api';
import { readVoiceJSON, validateVoiceUpload, voiceErrorFromResponse, voiceQuotaFromHeaders } from '../lib/voiceRequest';

// "Clean up dictation with my Claude plan" (daemon/voice_local_api.go). reason is
// why it isn't available right now: 'off' | 'not_installed' |
// 'not_signed_in' | 'paused' | 'unavailable'.
export interface VoiceLocalSettings {
  enabled: boolean;
  available: boolean;
  reason?: string;
  model: string;
}

// "Words to recognise" (daemon/voice_vocabulary.go): the user's own names,
// sent with the audio so Whisper spells them right. builtIn is the fixed list.
export interface VoiceWordsSettings {
  words: string[];
  builtIn: string[];
  maxWords: number;
}

/** Who cleaned a dictation: the user's own Claude login, or our cloud. */
export type VoiceCleanedBy = 'local' | 'cloud';

// Whether a dictation may send the terminal snapshot (`context`). In cloud
// mode the browser posts straight to the cloud, so the computer's "Only
// devices I approve" policy (daemon/trusted_devices.go) has to be honoured
// here: the policy is read when recording starts, and only an "any" answer
// that arrived by upload time lets context go. No answer yet, an older daemon,
// a refusal or an error all mean no context. The daemon's own proxy drops it
// the same way (daemon/voice.go cloudCleanupBody), which covers local mode.
export interface VoiceContextCheck {
  readonly allowed: boolean;
}

function startVoiceContextCheck(): VoiceContextCheck {
  if (currentPrefix() === '') return { allowed: false };
  // Each backend keeps its own policy; the answer only counts for this one.
  const backend = currentApiPrefix();
  let policy: unknown;
  void apiFetch('/api/remote-devices', undefined, { signRead: true })
    .then(r => (r.ok ? r.json() : null))
    .then((body: { policy?: unknown } | null) => { policy = body?.policy; })
    .catch(() => {});
  return { get allowed() { return policy === 'any' && currentApiPrefix() === backend; } };
}

function mayShareContext(check?: VoiceContextCheck): boolean {
  return currentPrefix() === '' || check?.allowed === true;
}

const voiceApi = {
  // Voice → text. Browser records via MediaRecorder; in cloud mode
  // the cloud serves this directly using the operator's GROQ_API_KEY
  // (rate-limited per Supabase user); in local mode it falls through
  // to the local daemon's own /api/transcribe (uses ~/.solu/env's
  // key). Either way we hit the path WITHOUT the machine prefix —
  // see rootFetch. Don't set content-type manually — fetch fills in
  // the multipart boundary when given a FormData body.
  //
  // `cleanup` tells the UI which path produced `text`:
  //   ok       — Whisper + the cleanup model polished (best quality)
  //   disabled — no cleanup on this path (raw Whisper)
  //   failed   — the cleanup call errored (raw Whisper; check server logs)
  //   skipped  — cleanup returned off-rails output (raw Whisper; check logs)
  // Absent field = older server build or local daemon; treat as disabled.
  //
  // `context` is the end of the focused pane's buffer (at most 4 KB, the
  // most the cloud uses) — passed to the cleanup LLM so "use state" can
  // become useState in a React repo, etc. Best-effort; empty is fine. It is
  // left out in cloud mode unless `contextCheck` allows it.
  // `quota` is the daily allowance the cloud reported, when it did.
  transcribe: (
    audio: Blob,
    opts: { filename?: string; language?: string; context?: string; contextCheck?: VoiceContextCheck; signal?: AbortSignal; deferCleanup?: boolean } = {},
  ) => {
    const context = mayShareContext(opts.contextCheck) ? opts.context : undefined;
    validateVoiceUpload(audio, { language: opts.language, context, defer_cleanup: opts.deferCleanup ? '1' : undefined }, opts.filename ?? 'audio.webm');
    const fd = new FormData();
    fd.append('audio', audio, opts.filename ?? 'audio.webm');
    if (opts.language) fd.append('language', opts.language);
    if (context) fd.append('context', context);
    // Ask the server to SKIP the cleanup pass and return raw Whisper fast;
    // the client then polishes via cleanupVoice() (raw-then-patch). Rides
    // as a multipart field so the daemon's transparent transcribe proxy
    // forwards it to the cloud unchanged. Server returns cleanup:"deferred".
    if (opts.deferCleanup) fd.append('defer_cleanup', '1');
    return rootFetch('/api/transcribe', {
      method: 'POST',
      body: fd,
      signal: opts.signal,
    }).then(async (r) => {
      // Plain-language errors the mic can act on (connect, daily limit).
      if (!r.ok) throw await voiceErrorFromResponse(r);
      const body = (await readVoiceJSON(r)) as VoiceDiagnostics & { text: string; cleanup?: 'ok' | 'disabled' | 'failed' | 'skipped' | 'deferred'; noSpeech?: boolean; by?: VoiceCleanedBy };
      return { ...body, quota: voiceQuotaFromHeaders(r.headers) };
    });
  },

  // Second half of raw-then-patch: polish already-transcribed text (the
  // same Haiku cleanup the one-shot transcribe runs, isolated so the client
  // can show raw first). Text-only, so it's cheap; no audio, no quota bump
  // (the dictation was already counted at transcribe time). Falls back to
  // the raw text with cleanup!="ok" on any server that lacks the route.
  cleanupVoice: (text: string, opts: { context?: string; contextCheck?: VoiceContextCheck; language?: string; signal?: AbortSignal } = {}) =>
    rootFetch('/api/voice/cleanup', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(mayShareContext(opts.contextCheck)
        ? { text, context: opts.context ?? '', language: opts.language ?? '' }
        : { text, language: opts.language ?? '' }),
      signal: opts.signal,
    }).then(async r => { if (!r.ok) throw await voiceErrorFromResponse(r); return readVoiceJSON<VoiceDiagnostics & { text: string; cleanup?: 'ok' | 'disabled' | 'failed' | 'skipped'; by?: VoiceCleanedBy }>(r); }),

  startVoiceContextCheck,

  // Start a Claude process for the cleanup while the user is still talking
  // (local mode only: on the phone the cloud transcribes and cleans).
  // Fire-and-forget; an older daemon without the route just 404s.
  voicePrewarm: (context: boolean): void => {
    if (currentPrefix() !== '') return;
    void rootFetch('/api/voice/prewarm', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ context }),
    }).catch(() => {});
  },
  // null = older daemon without the endpoint, so the row stays hidden.
  getVoiceLocal: async (): Promise<VoiceLocalSettings | null> => {
    const res = await apiFetch('/api/voice/local');
    if (res.status === 404) return null;
    if (!res.ok) throw new Error(`voice settings: ${res.status}`);
    return res.json();
  },
  // null = older daemon without the endpoint, so the row stays hidden.
  getVoiceWords: async (): Promise<VoiceWordsSettings | null> => {
    const res = await apiFetch('/api/voice/vocabulary');
    if (res.status === 404) return null;
    if (!res.ok) throw new Error(`voice words: ${res.status}`);
    return res.json();
  },
  setVoiceWords: (words: string[]): Promise<VoiceWordsSettings> =>
    apiFetch('/api/voice/vocabulary', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ words }),
    }).then(j<VoiceWordsSettings>),
  setVoiceLocal: (enabled: boolean): Promise<VoiceLocalSettings> =>
    apiFetch('/api/voice/local', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ enabled }),
    }).then(j<VoiceLocalSettings>),
};

type VoiceApi = typeof voiceApi;

declare module '../api' {
  interface FeatureApi extends VoiceApi {}
}

export default voiceApi;
