import { useEffect, useRef, useState } from 'react';
import type { Terminal } from '@xterm/xterm';
import { api, currentRoutingScope, onRoutingScopeChange } from '../api';
import { isRelayedSession, useCloud } from '../cloud';
import { useStore } from '../store';
import { daemonSupports, PANE_RECORDING_TIMELINE } from './daemonFeatures';

// ST-317 phase 1: while anything records, this window sends the daemon a
// "view record" of what it shows (docs/pane-recorder-timeline.md): the
// window size, pane rectangles, focus, maximised pane, scroll, window state,
// theme, display settings, whether the sidebar is collapsed and whether a
// panel is open. Ids, enums and integers only: never a title, a workspace
// name, a path, a URL or anything typed. The daemon accepts it only from the
// person at this computer, only while something records, and writes at most
// 10 a second; this sends only when what it shows changes, at most 4 a second.

const SAMPLE_MS = 250;
/** After a refused or failed send (a 409 before the daemon samples a new
 *  recording, a dropped connection), wait this long before trying again. */
const RETRY_MS = 1000;
/** After the set of recording panes changes, a refused view is tried again
 *  this many times, this far apart, before the window says it is away (the
 *  daemon takes a new recording into its timeline just after it starts). */
const RECHECK_TRIES = 5;
const RECHECK_MS = 300;
/** A health check that failed is asked again after this long. */
const HEALTH_RETRY_MS = 5000;
/** The daemon refuses a record over 4 KB; panes are dropped to fit. */
const MAX_BYTES = 4000;
/** The app's terminal font (Pane.tsx); a custom one would be "other". */
const FONT = 'jetbrains-mono';
/** @xterm/xterm as pinned in package.json; no Unicode addon is loaded (6). */
const XTERM: [number, number, number] = [5, 5, 0];
const UNICODE = 6;

export interface RecordingViewRecord {
  state: 'shown' | 'minimised' | 'tray' | 'reopened';
  away?: true;
  window?: { w: number; h: number };
  theme?: 'dark' | 'light';
  focus?: string;
  panes?: { id: string; x: number; y: number; w: number; h: number; max?: boolean; scroll?: number }[];
  sidebar?: { collapsed: boolean };
  panel?: 'none' | 'open';
  display?: { dpr: number; zoom: number; font: string; size: number; lineHeight: number; letterSpacing: number; xterm: [number, number, number]; unicode: number };
}

const clamp = (v: number, lo: number, hi: number) => Math.max(lo, Math.min(hi, Math.round(v)));

/** Lines scrolled up from the bottom, from the live-terminal registry. */
function paneScroll(paneId: string): number | undefined {
  const terms = (window as unknown as { __soluTerms?: Map<string, Terminal> }).__soluTerms;
  if (!terms) return undefined;
  for (const [key, term] of terms) {
    let id: unknown;
    try { id = (JSON.parse(key) as unknown[])[1]; } catch { continue; }
    if (id !== paneId) continue;
    const b = term.buffer.active;
    return clamp(b.baseY - b.viewportY, 0, 20000);
  }
  return undefined;
}

const TILE_ID = /^[A-Za-z0-9_-]{1,128}$/;

/** Drops panes from the end (keeping the focused one) until the record fits
 *  the daemon's 4 KB. */
export function fitViewRecord(v: RecordingViewRecord): RecordingViewRecord {
  let out = v;
  while (out.panes && out.panes.length > 1 && JSON.stringify(out).length > MAX_BYTES) {
    const panes = [...out.panes];
    const drop = [...panes].reverse().findIndex((p) => p.id !== out.focus);
    panes.splice(panes.length - 1 - (drop < 0 ? 0 : drop), 1);
    out = { ...out, panes };
  }
  return out;
}

/** The record for what this window shows now, or null when no pane is shown.
 *  tray: the desktop app announced it hid the window to the tray. */
export function currentViewRecord(opts: { sidebarCollapsed: boolean; panelOpen: boolean; wasHidden: boolean; tray?: boolean }): RecordingViewRecord | null {
  const hidden = typeof document !== 'undefined' && document.visibilityState === 'hidden';
  const s = useStore.getState();
  const panes: NonNullable<RecordingViewRecord['panes']> = [];
  // Terminal panes, and browser tiles beside them (ids only, never a URL).
  document.querySelectorAll<HTMLElement>('[data-pane-id], [data-browser-id]').forEach((el) => {
    const id = el.dataset.paneId ?? el.dataset.browserId;
    const known = el.dataset.paneId ? !!(id && s.panes[id]) : !!(id && TILE_ID.test(id));
    if (!id || !known || panes.some((p) => p.id === id)) return;
    const r = el.getBoundingClientRect();
    if (r.width <= 0 || r.height <= 0) return;
    const scroll = paneScroll(id);
    panes.push({
      id, x: clamp(r.left, 0, 16384), y: clamp(r.top, 0, 16384), w: clamp(r.width, 0, 16384), h: clamp(r.height, 0, 16384),
      ...(s.zoomedPaneId === id ? { max: true } : {}),
      ...(scroll ? { scroll } : {}),
    });
  });
  if (panes.length === 0) return null;
  const light = document.documentElement.getAttribute('data-theme') === 'light';
  return {
    state: opts.tray ? 'tray' : hidden ? 'minimised' : opts.wasHidden ? 'reopened' : 'shown',
    window: { w: clamp(window.innerWidth, 1, 16384), h: clamp(window.innerHeight, 1, 16384) },
    theme: light ? 'light' : 'dark',
    ...(s.focusedPaneId && panes.some((p) => p.id === s.focusedPaneId) ? { focus: s.focusedPaneId } : {}),
    panes: panes.slice(0, 64),
    sidebar: { collapsed: opts.sidebarCollapsed },
    panel: opts.panelOpen ? 'open' : 'none',
    display: {
      dpr: clamp((window.devicePixelRatio || 1) * 100, 25, 1000),
      zoom: clamp((s.uiScale || 1) * 100, 25, 500),
      font: FONT,
      size: clamp(s.terminalFontSize || 13, 6, 72),
      lineHeight: clamp((s.terminalLineHeight || 1) * 100, 80, 300),
      letterSpacing: 0,
      xterm: XTERM,
      unicode: UNICODE,
    },
  };
}

/** The ids of the panes recording now, sorted and joined. */
function recordingPaneSet(): string {
  return Object.values(useStore.getState().panes)
    .filter((p) => p.recording?.on)
    .map((p) => p.id)
    .sort()
    .join(',');
}

/** Sends view records while any pane records and the daemon keeps a timeline. */
export function useRecordingViewRecords(opts: { sidebarCollapsed: boolean; panelOpen: boolean }) {
  const recording = useStore((s) => Object.values(s.panes).some((p) => p.recording?.on));
  const optsRef = useRef(opts);
  optsRef.current = opts;
  // Restart on a machine or backend switch, so records go to the daemon shown.
  const [scopeKey, setScopeKey] = useState(() => currentRoutingScope().key);
  useEffect(() => onRoutingScopeChange(() => setScopeKey(currentRoutingScope().key)), []);
  useEffect(() => {
    // Reached through the cloud the daemon refuses view records (switch-on's
    // gate): send none.
    if (!recording || isRelayedSession(useCloud.getState())) return;
    const scope = currentRoutingScope();
    let live = true;
    let served = false;
    // last is what the daemon has answered for (accepted, or refused with 409
    // and answered by the away record): it is not sent again until it
    // changes. A failed send is retried after a short wait.
    let last = '';
    let sending = false;
    let retryAt = 0;
    let wasHidden = false;
    let tray = false;
    let timer: number | undefined;
    let healthTimer: number | undefined;
    // Sends one record. The daemon answers 409 when none of the shown tiles is
    // in a recording workspace: the window then says it is away.
    const send = (body: RecordingViewRecord, onAccepted?: () => void) => {
      const json = JSON.stringify(body);
      sending = true;
      return api.sendRecordingView(body, scope)
        .then(async (status) => {
          if (status === 409 && !body.away && recheck > 0) {
            // A recording just started: the daemon may not have taken it into
            // its timeline yet. Try the same view again shortly.
            recheck--;
            retryAt = Date.now() + RECHECK_MS;
            return;
          }
          if (status === 409 && !body.away) {
            const away: RecordingViewRecord = { state: 'shown', away: true };
            const awayStatus = await api.sendRecordingView(away, scope);
            if (awayStatus !== 200 && awayStatus !== 409) throw new Error(String(awayStatus));
            // Remember the refused view: re-sent only when what is shown changes.
            last = json;
            return;
          }
          if (status !== 200) throw new Error(String(status));
          last = json;
          // Accepted: a later refusal is a real one, not the daemon catching up.
          recheck = 0;
          onAccepted?.();
        })
        .catch(() => { retryAt = Date.now() + RETRY_MS; })
        .finally(() => { sending = false; });
    };
    const record = () => {
      const hidden = document.visibilityState === 'hidden';
      const view = currentViewRecord({ ...optsRef.current, wasHidden: wasHidden && !hidden && !tray, tray });
      if (hidden) wasHidden = true;
      return view ? fitViewRecord(view) : ({ state: 'shown', away: true } as RecordingViewRecord);
    };
    // The set of recording panes: when it changes (a recording starts or
    // stops anywhere), the current view is sent again even if it looks the
    // same, so a view refused before its workspace recorded is in the
    // timeline from the recording's first second.
    let recordingSet = recordingPaneSet();
    // Retries left for a view refused right after the recording set changed;
    // armed at start too (the first recording has just started).
    let recheck = RECHECK_TRIES;
    const tick = () => {
      if (!live || !served || sending || Date.now() < retryAt) return;
      const now = recordingPaneSet();
      if (now !== recordingSet) {
        recordingSet = now;
        last = '';
        recheck = RECHECK_TRIES;
      }
      const body = record();
      if (JSON.stringify(body) === last) return;
      void send(body, () => { if (body.state === 'reopened') wasHidden = false; });
    };
    // A hide to the tray is recorded at once, so a quick return can't hide it.
    const onBackground = (e: Event) => {
      const reason = (e as CustomEvent<{ reason?: string }>).detail?.reason;
      tray = reason === 'close-to-tray' || reason === 'tray-hide' || reason === 'close';
      wasHidden = true;
      if (tray && live && served) void send(record());
    };
    const onRestored = () => { tray = false; };
    window.addEventListener('solu:desktop-will-background', onBackground);
    window.addEventListener('solu:desktop-restored', onRestored);
    const probe = () => {
      void api.healthScoped(scope)
        .then((h) => {
          served = daemonSupports(h, PANE_RECORDING_TIMELINE);
          if (served && live) {
            tick();
            timer = window.setInterval(tick, SAMPLE_MS);
          }
        })
        .catch(() => { if (live) healthTimer = window.setTimeout(probe, HEALTH_RETRY_MS); });
    };
    probe();
    return () => {
      live = false;
      window.removeEventListener('solu:desktop-will-background', onBackground);
      window.removeEventListener('solu:desktop-restored', onRestored);
      if (timer !== undefined) window.clearInterval(timer);
      if (healthTimer !== undefined) window.clearTimeout(healthTimer);
    };
  }, [recording, scopeKey]);
}
