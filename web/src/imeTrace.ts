// In-app IME event trace ring buffer. Captures the real composition /
// beforeinput / onData sequence from a MOBILE keyboard so the Android
// predictive-rewrite gibberish (see feedback_mobile_ime_composition) can be
// diagnosed from a physical device WITHOUT DevTools. Mirror of scrollDiag.ts.
//
// Enable console-free with ?imetrace=1 (sticky — persisted to localStorage so
// the installed PWA picks it up after you set it once in mobile Chrome). The
// overlay's × button disables it again. See FOLLOWUPS-mobile-2026-06-19.md.

export interface ImeTraceEvent {
  t: number; // performance.now() at log time
  kind: string; // 'c-start' | 'c-update' | 'c-end' | 'beforeinput' | 'onData'
  data?: string | null; // CompositionEvent/InputEvent .data
  inputType?: string; // beforeinput only
  isComposing?: boolean; // beforeinput only
  taValue?: string; // helper-textarea .value (compositionend)
  sent?: string; // the bytes xterm.onData actually emitted to the PTY
  anchor?: string; // helper-textarea rect at compositionstart ("x,y wxh", desktop)
}

const MAX_EVENTS = 500;
const events: ImeTraceEvent[] = [];
const listeners = new Set<() => void>();
let enabled = false;

function readEnabled(): boolean {
  try {
    if (typeof window !== 'undefined') {
      const q = new URLSearchParams(window.location.search).get('imetrace');
      if (q === '1') localStorage.setItem('solu.imetrace', '1');
      else if (q === '0') localStorage.removeItem('solu.imetrace');
    }
    return localStorage.getItem('solu.imetrace') === '1';
  } catch {
    return false;
  }
}
enabled = readEnabled();

export function isImeTraceEnabled(): boolean {
  return enabled;
}

export function setImeTraceEnabled(on: boolean): void {
  enabled = on;
  try {
    if (on) localStorage.setItem('solu.imetrace', '1');
    else localStorage.removeItem('solu.imetrace');
  } catch {
    /* ignore */
  }
  for (const l of listeners) l();
}

// Cheap no-op when disabled — safe to call on every keystroke.
export function recordIme(ev: ImeTraceEvent): void {
  if (!enabled) return;
  events.push(ev);
  if (events.length > MAX_EVENTS) events.splice(0, events.length - MAX_EVENTS);
  for (const l of listeners) l();
}

export function getImeTraceEvents(): readonly ImeTraceEvent[] {
  return events;
}

export function clearImeTrace(): void {
  events.length = 0;
  for (const l of listeners) l();
}

export function subscribeImeTrace(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

// Console/Playwright access to the ring without the overlay.
if (typeof window !== 'undefined') {
  (window as unknown as { __imeTrace?: () => readonly ImeTraceEvent[] }).__imeTrace = getImeTraceEvents;
}
