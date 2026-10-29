// useNativePane.ts — React subscription hook for a kind==='native' pane.
//
// Opens the per-pane SSE (openNativeStream), parses each `data:` frame into a
// NativeEvent, and folds the stream into a render-ready event list. The folded
// list lives in a MODULE-LEVEL per-pane cache so it survives a component
// remount: the warm-pane architecture portals/remounts pane bodies on workspace
// switch / zoom / drag, and we don't want the conversation to blank out and
// re-snapshot every time. The cache is the source of truth; React state is a
// mirror bumped on every change.
//
// Folding rules:
//  - Events are keyed by `seq` (monotonic per-pane). The SSE sends the ring
//    snapshot first (each Seq'd) then live deltas in the same framing; the
//    snapshot tail can overlap the first live deltas, so we DEDUP by seq.
//  - `text_delta` / `thinking_delta` are folded into the growing `text` /
//    `thinking` block sharing the same messageId, so the renderer sees one
//    coalesced block per message instead of hundreds of fragments.
//  - A non-contiguous seq (a gap) triggers a backfill via fetchNativeHistory
//    (since=lastSeq) before the gapped event is folded; if the history came
//    back `truncated` (ring floor passed our cursor) we cannot heal
//    contiguously, so we drop the cache and force a fresh SSE resnapshot.
//  - On es.onopen (every (re)connect, including auto-reconnect after a drop) we
//    resync via fetchNativeHistory because SSE never replays frames fired while
//    we were disconnected (the transition-event-drift pattern).
//
// The hook exposes the structured surface NativePane renders against:
// { events, sessionInit, pendingApproval, connected, send, resolve, interrupt }.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useStore } from '../store';
import { useCloud } from '../cloud';
import { ApiError } from '../api';
import {
  openNativeStream,
  fetchNativeHistory,
  sendNativeInput,
  resolveNativeApproval,
  interruptNative,
  getAllowlist,
  setAllowlist,
} from './nativeApi';
import type {
  NativeEvent,
  NativeInputResponse,
  ResolveApprovalResponse,
  NativeInterruptResponse,
} from './nativeEvents';
import { reportComposerPresence } from './composerPresence';
import { perfTurnSubmit, perfFirstDelta, perfTurnEnd, perfPublish } from './nativePerf';

// ---------------------------------------------------------------------------
// Module-level per-pane cache (survives remount).
// ---------------------------------------------------------------------------

interface PaneCache {
  // Folded, in-order event list (dedup'd + coalesced). This is what the UI
  // renders. Order is seq-ascending as received; deltas mutate the block event
  // they belong to in place rather than appending new entries.
  events: NativeEvent[];
  // Index into `events` of the in-progress text/thinking block per messageId,
  // so a delta can find and grow its block in O(1).
  blockBySeq: Map<number, number>; // messageId-keyed block → events index
  blockByMessageId: Map<string, number>; // messageId → events index
  // Highest LIVE-RING seq folded so far — the resync/backfill + gap-detection
  // cursor. Deliberately ignores transcript-seed events: those carry a high,
  // live-disjoint seq (seedSeqBase ~2^62) and must NOT advance this cursor, or
  // the onopen resync would request `since=2^62` and silently skip the live ring
  // (which uses small monotonic seqs). Only non-FromReplay events bump it.
  lastSeq: number;
  // Seen seqs (dedup the snapshot/live overlap). Includes seed seqs so a
  // re-fetched since=0 history (which re-emits the same deterministic seed seqs)
  // dedups cleanly.
  seen: Set<number>;
  // Whether the on-mount transcript seed (fetchNativeHistory since=0) has been
  // attempted for this pane. Idempotency guard so a warm-pane remount doesn't
  // re-seed an already-populated cache. Set once the seed fetch resolves.
  seeded: boolean;
  // Verbatim `user_text` contents already in the list, used to dedup a
  // user prompt that arrives BOTH as a transcript-seed event (FromReplay) AND
  // as a live `--replay-user-messages` echo (different seq, same text). The
  // daemon dedups seed-vs-ring within a single history response, but the live
  // SSE echo is a separate delivery channel and can still collide; we drop the
  // later same-text user_text here regardless of which one landed first.
  userTexts: Set<string>;
  // The pane's `session_init` event, captured the first time it folds. It is
  // emitted once at the very front of a session and never changes, so caching it
  // here lets the hook expose `sessionInit` without an O(n) forward scan of
  // `events` on every render (which would re-scan on every streaming text_delta).
  // null until a session_init has been folded for this pane.
  sessionInit: NativeEvent | null;
}

const paneCaches = new Map<string, PaneCache>();

// ---------------------------------------------------------------------------
// Optimistic user-message echo (kept ENTIRELY out of the seq cache).
// ---------------------------------------------------------------------------
//
// A pending entry is the user's own message rendered the instant they hit Enter,
// BEFORE its `user_text` replay echo streams back. It lives in this parallel
// per-pane map — NOT in PaneCache.events/seen/lastSeq — so it can never advance
// the backfill cursor, poison gap-detection, or register in c.userTexts (which
// would make foldEvent drop the REAL echo and orphan the optimistic row). When
// the matching user_text folds we drop exactly one entry (content-FIFO by text)
// so the real echo takes its place with no flicker/double.
export interface PendingMsg {
  localMsgId: string;
  text: string;
  failed?: boolean;
}
const pendingByPane = new Map<string, PendingMsg[]>();

// Set by reconcilePending() inside foldEvent when an optimistic row is dropped;
// the hook's publish() reads + clears it to know it must re-mirror `pending`.
// PER-PANE (was a module boolean): foldEvent is a free function and publish() is
// rAF-COALESCED, so a flush scheduled by pane A can run a frame later and would
// otherwise consume a `true` set by pane B's interleaved fold — mirroring the
// WRONG pane's pending list. Keyed by paneId, the flag is read+cleared only for
// the flushing pane.
const pendingReconciledByPane = new Set<string>();

// Always-on vanish instrumentation. The "submitted prompt didn't render" bug is
// rare and resync-timing-dependent, so a repro must SELF-REPORT. These fire only
// on cold paths (a user_text drop, a gap-detect skip, a socket close/rebuild) —
// not the hot streaming path — so they are free to leave on. Greppable by
// "[native-vanish]"; pair with the daemon's "[native-input]"/"[native-turn]" logs.
function vlog(msg: string, fields: Record<string, unknown>): void {
  try {
    // eslint-disable-next-line no-console
    console.debug(`[native-vanish] ${msg}`, fields);
  } catch {
    /* never let logging throw */
  }
}
// Cheap, collision-tolerant text fingerprint for logs (never reversed; it just
// correlates a dropped echo with its pending row without dumping prompt text).
function textHash(t: string): string {
  let h = 0;
  for (let i = 0; i < t.length; i++) h = (h * 31 + t.charCodeAt(i)) | 0;
  return `${(h >>> 0).toString(36)}:${t.length}`;
}

function getPending(paneId: string): PendingMsg[] {
  let p = pendingByPane.get(paneId);
  if (!p) {
    p = [];
    pendingByPane.set(paneId, p);
  }
  return p;
}

// Reconcile the pending list against a freshly-folded user_text: remove exactly
// ONE matching entry. Returns true if one was dropped (caller should bump the
// pending mirror).
//
// Reconcile is CONTENT-FIFO: we drop the first non-failed pending row whose text
// equals the echo's text. There is deliberately NO id-match branch — the daemon
// does NOT stamp localMsgId onto the streamed `user_text` echo (it only returns
// localMsgId in the /native/input POST response, which we never thread back onto
// the wire event). An id-match branch here was dead code: e.localMsgId is always
// undefined on a real echo. Content-FIFO is correct because the optimistic rows
// and their echoes arrive in the SAME order (a single per-pane POST queue), so
// the oldest unmatched same-text pending row is always the one this echo settles
// — even for repeated identical prompts ("yes", "continue"). Failed rows are
// skipped so they survive as the retry anchor until the user retries them.
function reconcilePending(paneId: string, e: NativeEvent): boolean {
  const p = pendingByPane.get(paneId);
  if (!p || !p.length) return false;
  const text = e.text ?? '';
  if (!text) return false;
  const idx = p.findIndex((m) => m.text === text && !m.failed);
  if (idx < 0) return false;
  p.splice(idx, 1);
  return true;
}

// ---------------------------------------------------------------------------
// Per-pane draft persistence (survives warm-pane remounts like the transcript,
// AND page reloads via sessionStorage — see below).
//
// A tiny per-pane subscriber set lets code OUTSIDE the composer (the message-
// action "edit & resend") push text into a mounted composer: setDraft writes the
// module map AND notifies the live NativePane so it mirrors the new value into
// its React state immediately. NativePane subscribes via subscribeDraft while
// mounted; its own keystrokes go straight through setDraftValue (which calls this
// setDraft) so the notify is a cheap same-value no-op in that common path.
//
// sessionStorage write-through: a module map alone dies with the page, and the
// page reloads OUTSIDE the user's control — the "install when idle" /
// required-update swap, the stale-bundle self-heal — so typed-but-unsent
// composer text would silently vanish (the exact work-destruction the update
// gates promise not to do). Per-tab sessionStorage is the right scope (two tabs
// shouldn't share a composer), it hydrates once at module load, and sends
// already clear via setDraft(paneId, '') so sent text can't be re-restored.
// Orphans (pane closed with a draft) die with the tab — bounded, not swept.
// ---------------------------------------------------------------------------
const DRAFT_SS_PREFIX = 'solu.native.draft.';
function loadDraftsFromSession(): Map<string, string> {
  const m = new Map<string, string>();
  try {
    for (let i = 0; i < sessionStorage.length; i++) {
      const k = sessionStorage.key(i);
      if (k && k.startsWith(DRAFT_SS_PREFIX)) {
        const v = sessionStorage.getItem(k);
        if (v) m.set(k.slice(DRAFT_SS_PREFIX.length), v);
      }
    }
  } catch {
    /* sessionStorage unavailable (privacy mode) — drafts last the page only */
  }
  return m;
}
const draftByPane = loadDraftsFromSession();
const draftSubsByPane = new Map<string, Set<(v: string) => void>>();
export function getDraft(paneId: string): string {
  return draftByPane.get(paneId) ?? '';
}
// The first pane among livePanes holding a draft with more than whitespace.
// Only panes that still exist count: a draft restored from sessionStorage for
// a pane that has since closed (or lives on the other backend) is never
// swept, and must not hold the unattended updates for the life of the app.
export function unsentDraftPaneId(livePanes: Record<string, unknown>): string | undefined {
  for (const [paneId, draft] of draftByPane) {
    if (draft.trim().length > 0 && Object.prototype.hasOwnProperty.call(livePanes, paneId)) return paneId;
  }
  return undefined;
}
export function hasAnyUnsentDraft(livePanes: Record<string, unknown>): boolean {
  return unsentDraftPaneId(livePanes) !== undefined;
}
export function setDraft(paneId: string, v: string): void {
  if (v) draftByPane.set(paneId, v);
  else draftByPane.delete(paneId);
  try {
    if (v) sessionStorage.setItem(DRAFT_SS_PREFIX + paneId, v);
    else sessionStorage.removeItem(DRAFT_SS_PREFIX + paneId);
  } catch {
    /* quota/privacy failure — the in-memory map still covers this page */
  }
  reportComposerPresence(paneId, v.length > 0);
  const subs = draftSubsByPane.get(paneId);
  if (subs) for (const fn of subs) fn(v);
}
// Subscribe a mounted composer to external draft pushes (edit & resend). Returns
// an unsubscribe. The callback fires on EVERY setDraft for the pane; the composer
// must guard against echoing its own writes (it already owns the value).
export function subscribeDraft(paneId: string, fn: (v: string) => void): () => void {
  let subs = draftSubsByPane.get(paneId);
  if (!subs) {
    subs = new Set();
    draftSubsByPane.set(paneId, subs);
  }
  subs.add(fn);
  return () => {
    const s = draftSubsByPane.get(paneId);
    if (s) {
      s.delete(fn);
      if (!s.size) draftSubsByPane.delete(paneId);
    }
  };
}

// ---------------------------------------------------------------------------
// Per-pane mid-turn STEER QUEUE — the follow-up messages typed while a turn is
// in flight, FIFO, auto-flushed (one per settle) when the turn ends. Module-
// cached like the draft so a warm-pane remount / reload / pane-swap doesn't
// silently DROP a queued steer (the mounted pane was previously the only holder,
// in component-local state, so any remount lost it). The mounted NativePane is
// the only writer and re-seeds from here on (re)mount — no subscriber set needed.
// ---------------------------------------------------------------------------
const queuedByPane = new Map<string, string[]>();
export function getQueued(paneId: string): string[] {
  return queuedByPane.get(paneId) ?? [];
}
export function setQueued(paneId: string, msgs: string[]): void {
  if (msgs.length) queuedByPane.set(paneId, msgs);
  else queuedByPane.delete(paneId);
}

// ---------------------------------------------------------------------------
// Per-pane client "/clear" floor — the set of event seqs hidden from the
// rendered transcript at the moment the user ran /clear. Purely client-side
// (the daemon session + history are untouched); module-cached so a warm-pane
// remount keeps the cleared view (like the draft) instead of unhiding history.
// New events carry new seqs not in the set and render normally.
// ---------------------------------------------------------------------------
const clearedSeqsByPane = new Map<string, Set<number>>();
export function getClearedSeqs(paneId: string): Set<number> {
  return clearedSeqsByPane.get(paneId) ?? new Set();
}
export function setClearedSeqs(paneId: string, seqs: Set<number>): void {
  if (seqs.size) clearedSeqsByPane.set(paneId, seqs);
  else clearedSeqsByPane.delete(paneId);
}

// ---------------------------------------------------------------------------
// Per-pane "consumed" prompt-suggestion seq — the seq of the suggestion the
// user Tab-accepted (or that a newer turn superseded). Module-cached like
// clearedSeqs so a warm-pane remount within the ~10-20s suggestion window does
// NOT re-show an already-accepted suggestion. derivePromptSuggestion treats any
// suggestion at-or-before this seq as consumed.
// ---------------------------------------------------------------------------
const consumedSuggestionByPane = new Map<string, number>();
export function getConsumedSuggestionSeq(paneId: string): number {
  return consumedSuggestionByPane.get(paneId) ?? 0;
}
export function setConsumedSuggestionSeq(paneId: string, seq: number): void {
  if (seq > 0) consumedSuggestionByPane.set(paneId, seq);
  else consumedSuggestionByPane.delete(paneId);
}

// Whether a pane's most recent clear was a REAL context reset (forwarded /clear)
// vs a view-only hide. Module-cached like clearedSeqs so the cleared-notice copy +
// undo-suppression survive a warm-pane remount — a plain useState would reset to
// false and resurrect the misleading "Show again" (which would re-reveal context
// the agent has actually forgotten).
const realClearByPane = new Map<string, boolean>();
export function getRealClear(paneId: string): boolean {
  return realClearByPane.get(paneId) ?? false;
}
export function persistRealClear(paneId: string, real: boolean): void {
  if (real) realClearByPane.set(paneId, true);
  else realClearByPane.delete(paneId);
}

// ---------------------------------------------------------------------------
// Per-pane tool allowlist ("always allow this tool") — module-cached so it
// survives warm-pane remounts (the body portals/remounts on workspace switch /
// zoom / drag). The daemon is the source of truth; this cache is a mirror seeded
// once per pane via GET /native/allowlist and reconciled with the POST response
// on every add/remove. `loaded` guards the one-time fetch so a remount doesn't
// re-fetch (and doesn't blank the mirror back to []) on every mount.
// ---------------------------------------------------------------------------
interface AllowlistCache {
  tools: string[];
  loaded: boolean;
}
const allowlistByPane = new Map<string, AllowlistCache>();
function getAllowlistCache(paneId: string): AllowlistCache {
  let a = allowlistByPane.get(paneId);
  if (!a) {
    a = { tools: [], loaded: false };
    allowlistByPane.set(paneId, a);
  }
  return a;
}

function newCache(): PaneCache {
  return {
    events: [],
    blockBySeq: new Map(),
    blockByMessageId: new Map(),
    lastSeq: 0,
    seen: new Set(),
    seeded: false,
    userTexts: new Set(),
    sessionInit: null,
  };
}

// ---------------------------------------------------------------------------
// Client-cache compaction high-water mark.
//
// The daemon ring caps at 4000 events; the client cache (events/seen/userTexts)
// otherwise grows unbounded across a very long-lived pane. When `events` exceeds
// COMPACT_HIGH_WATER we drop the OLDEST non-seed (live) events, keeping the seed
// block (FromReplay history) plus the most recent COMPACT_KEEP_RECENT live rows.
// The threshold is deliberately well ABOVE the daemon ring cap (4000) so normal
// operation never trips it; it is a safety valve, not a routine path. See
// compactCache for the resync/seed invariants this preserves.
const COMPACT_HIGH_WATER = 6000;
const COMPACT_KEEP_RECENT = 4000;

// Resync cadence. While the SSE is OPEN we periodically re-pull
// /native/history?since=lastSeq to self-heal a dropped/stalled TAIL (a half-open
// connection or a non-blocking-fanout drop that leaves no later event to trip
// gap-detection and no reconnect to trip onopen). The GET is near-free when caught
// up: NativeSince(lastSeq) returns an empty slice, foldEvent finds nothing, no
// re-render. So we poll FAST while a turn is in flight — a stuck spinner / dropped
// tail then clears in ~RESYNC_ACTIVE_MS instead of ~7s, exactly when the user is
// waiting — and THROTTLE to ~7.5s when idle so an idle pane stays cheap. "Mid-turn"
// is read from the module cache (a block still marked streaming), NOT a prop, so it
// adds no effect dependency that would tear down + rebuild the EventSource.
const RESYNC_ACTIVE_MS = 2500; // tick cadence; also the mid-turn resync cadence
const RESYNC_IDLE_TICKS = 3; // idle = every 3rd tick → ~7.5s

// Seed retry while a Term→Focus switch is respawning the pane. The daemon
// answers /native/history with 409 ("pane is not native") until the new native
// agent lands — worst case ~5.5s (settle 1.5s + 3×700ms spawn retries + 600ms
// health grace). 10 × 700ms comfortably covers it without polling forever.
const SEED_409_RETRY_MS = 700;
const SEED_409_MAX_RETRIES = 10;

// Whether the idle-resync timer should fire a backfill this tick: only while the
// effect is live (not torn down) AND the EventSource is OPEN. A CONNECTING /
// CLOSED stream is handled by onopen/onerror; polling it would be wasteful and
// could race the rebuild. Pure so it's unit-testable without rendering the hook.
export function shouldIdleResync(closed: boolean, readyState: number): boolean {
  return !closed && readyState === WebSocket.OPEN; // WebSocket.OPEN === EventSource.OPEN === 1
}

function getCache(paneId: string): PaneCache {
  let c = paneCaches.get(paneId);
  if (!c) {
    c = newCache();
    paneCaches.set(paneId, c);
  }
  return c;
}

function resetCache(paneId: string): PaneCache {
  // A reset throws away the folded list but PRESERVES the `seeded` flag: the
  // transcript seed is a one-time on-mount backfill, not part of the live ring
  // we're re-snapshotting, so re-seeding after a truncated-history reset would
  // duplicate the prior conversation. (A genuinely new session arrives on a new
  // paneId with a fresh cache, so `seeded` resetting to false there is correct.)
  const prev = paneCaches.get(paneId);
  const c = newCache();
  if (prev) c.seeded = prev.seeded;
  paneCaches.set(paneId, c);
  return c;
}

// ---------------------------------------------------------------------------
// Respawn detection + full cache reset.
//
// A Terminal↔Native mode switch KILLS + RESPAWNS the pane as a brand-new agent
// process for the SAME pane id (daemon/pane_native.go), so the daemon's per-pane
// `nativeSeq` resets to 0 and the fresh ring re-emits seq 1,2,3…. But the CLIENT
// keeps this pane id's module cache across the warm-pane remount (the map + the
// `seeded` flag survive), so foldEvent's `c.seen.has(e.seq)` dedup would treat
// the respawned ring's seq 1,2,3… as "already seen" → never fold them → the
// conversation freezes/blanks after a switch.
//
// The reliable respawn signal is the pane's OS process id (`pane.pid`): a
// kill+respawn always lands a new pid. We track the last-seen pid per pane and,
// on a CHANGE (never the first observation), wipe the cache COMPLETELY — unlike
// resetCache this flips `seeded=false` so the on-mount since=0 transcript seed
// re-runs for the fresh ring, and it drops `seen`/`lastSeq`/`userTexts` so the
// new ring's low seqs fold cleanly instead of being deduped against the dead
// session's seqs. Pending optimistic echoes, drafts, cleared-view floors and
// the allowlist live in SEPARATE module maps and are deliberately untouched.
// ---------------------------------------------------------------------------
const panePids = new Map<string, number>();

// Record `pid` for `paneId` and report whether it represents a RESPAWN — i.e.
// the pid changed from a previously-recorded, valid (>0) value. The first time
// we see a pane (no prior entry) is NOT a respawn (initial mount): we just
// record it. A transition to/from an unknown pid (0/undefined, e.g. a poll that
// hasn't populated it yet) is also NOT treated as a respawn — only a real
// new-process pid swap fires. Always stores the latest valid pid.
export function notePidAndDetectRespawn(paneId: string, pid: number | undefined): boolean {
  const next = typeof pid === 'number' && pid > 0 ? pid : 0;
  const prev = panePids.get(paneId);
  if (next) panePids.set(paneId, next);
  // Respawn only when we had a real prior pid and the new real pid differs.
  return prev !== undefined && prev > 0 && next > 0 && next !== prev;
}

// Per-pane last-seen daemon epoch (PaneInfo.nativeEpoch). A daemon RESTART/SWAP
// rebuilds every native pane's event ring from seq 1 (AttachExisting) but the
// surviving agent keeps its pid — so notePidAndDetectRespawn can't see it, and
// the surviving client cache dedups the re-seq'd 1..N as "already seen" → the
// mid-turn reattach jumbles/freezes. The daemon epoch is the missing signal: it
// changes ONLY on a daemon restart. Mostly the same shape as
// notePidAndDetectRespawn — a change from a prior valid epoch is a reset.
//
// `hadContent` (cache.lastSeq > 0) catches the ONE transitional swap a plain
// defined→defined change can't: an epoch-LESS older daemon (≤v0.0.629) sent no
// epoch, so we never recorded a prior value; when it swaps to an epoch daemon the
// epoch goes undefined→defined, which would otherwise read as a harmless "first
// observation" and skip the reset — leaving the re-seq'd ring colliding with the
// surviving cursor (lost-latest-messages-until-a-term-switch, the rollout symptom).
// Resetting on undefined→defined ONLY when the cache already holds a live session
// is safe: an epoch daemon stamps the epoch on EVERY poll, so a fresh pane records
// it at mount (lastSeq=0) BEFORE any content — only an epoch-less predecessor
// leaves content with no recorded epoch. A full page reload clears lastSeq too, so
// a fresh load never trips it. Distinct map from panePids so a pane that changes
// BOTH (a respawn under a new daemon) still records each independently.
const paneEpochs = new Map<string, number>();
export function noteEpochAndDetectReset(
  paneId: string,
  epoch: number | undefined,
  hadContent = false,
): boolean {
  const next = typeof epoch === 'number' && epoch > 0 ? epoch : 0;
  const prev = paneEpochs.get(paneId);
  if (next) paneEpochs.set(paneId, next);
  if (next <= 0) return false;
  // Known prior epoch → reset on any change (steady-state daemon swap).
  if (prev !== undefined && prev > 0) return next !== prev;
  // First epoch ever seen, but a live session predates it → epoch-less→epoch swap.
  return hadContent;
}

// Fully reset a pane's fold cache for a RESPAWN (new agent process, same pane
// id): throw away the folded list AND re-arm the transcript seed (`seeded`
// stays false on the brand-new cache) so the on-mount since=0 backfill re-runs
// against the fresh ring. Distinct from resetCache, which preserves `seeded`
// for a truncated-history rebuild (same live session, just a rolled ring). The
// optimistic pending list is cleared too: those echoes belonged to the dead
// session and will never arrive on the new ring (orphaning them would leave a
// stuck "sending…" row forever). Drafts / cleared-view / allowlist are left
// intact (separate maps; they're pane-scoped UI state, not session state).
export function resetCacheForRespawn(paneId: string): PaneCache {
  const c = newCache(); // seeded=false, empty events/seen/lastSeq/userTexts
  paneCaches.set(paneId, c);
  pendingByPane.set(paneId, []);
  return c;
}

// Fold one event into the cache. Returns true if the cache changed (i.e. the
// caller should bump React state). Idempotent on already-seen seqs.
function foldEvent(c: PaneCache, e: NativeEvent): boolean {
  if (c.seen.has(e.seq)) return false;
  c.seen.add(e.seq);
  // Stamp a best-effort client arrival time on LIVE events only. The wire carries
  // no per-event timestamp, so turn separators / tool durations derive from when
  // we first saw each event. Replayed (transcript-seed) events have no meaningful
  // "now" — leave clientTs unset so the UI doesn't fabricate a precise time for
  // history (it falls back to a plain "earlier" boundary).
  if (!e.fromReplay && e.clientTs == null) e.clientTs = Date.now();
  // Only LIVE events advance the resync cursor; seed events (FromReplay) carry
  // a high disjoint seq that would poison the since= backfill (see PaneCache).
  if (!e.fromReplay && e.seq > c.lastSeq) c.lastSeq = e.seq;

  // Capture the session_init so the hook can expose it without an O(n) per-render
  // scan of `events`. Two sources fold it: the transcript SEED synthesizes a
  // FromReplay init (model/cwd) so the Focus bottom bar shows the model/context
  // INSTANTLY on a resumed pane, and the live agent emits the authoritative init
  // (also carrying permissionMode/tools/slashCommands) on the first turn. Prefer
  // the LIVE one: a replayed init may land first (it has only model/cwd), so the
  // first live init must be allowed to UPGRADE it — else slash commands / the
  // auto-mode badge would never populate.
  if (
    e.type === 'session_init' &&
    (
      c.sessionInit === null ||
      (c.sessionInit.fromReplay && !e.fromReplay) ||
      (c.sessionInit.fromReplay === e.fromReplay && e.seq > c.sessionInit.seq)
    )
  ) {
    c.sessionInit = e;
  }

  // user_text dedup — SEED-vs-resume-echo only. At resume the transcript seed
  // (FromReplay) carries the prior prompts AND the live `--replay-user-messages`
  // stream re-echoes the most recent one, so a seeded prompt's AUTOMATIC live
  // re-echo (the user typed nothing — the daemon re-delivered history) is the
  // same turn and must be dropped. But a prompt the user JUST TYPED must always
  // render even when its text happens to match prior history ("hi", "yes",
  // "continue", "ok" after a term→native switch). The authoritative signal that
  // an echo is a fresh local send is that it reconciles a PENDING optimistic row
  // (only sendOptimistic creates those). So: a reconciled echo is never dropped,
  // and a SEED never consumes a pending row (that race would orphan the just-sent
  // prompt — the long-standing "first native message vanishes / renders below the
  // reply after switching from term" bug).
  if (e.type === 'user_text') {
    // Reconcile the optimistic echo FIRST: drop exactly one pending row whose
    // text matches (content-FIFO; see reconcilePending). Only a LIVE echo may
    // settle a pending local turn — a transcript seed must never consume one.
    // The pending bump is signalled via the module flag so the hook re-mirrors
    // `pending` on this fold.
    const reconciled = !e.fromReplay && reconcilePending(e.paneId, e);
    if (reconciled) pendingReconciledByPane.add(e.paneId);
    const t = e.text ?? '';
    if (t) {
      if (e.fromReplay) {
        // Seed prompt: remember it so a later AUTOMATIC resume re-echo of the
        // same text can be dropped once (see below).
        c.userTexts.add(t);
      } else if (!reconciled && c.userTexts.has(t)) {
        // Live echo of a SEEDED prompt that the user did NOT just send (no
        // pending row matched) — an automatic resume re-delivery of history;
        // drop it and consume the entry. A genuinely-new prompt that merely
        // REUSES prior text reconciled a pending row above (`reconciled` true),
        // so it skips this branch and always renders.
        //
        // Phase-0 vanish instrumentation: this is the single highest-suspicion
        // client drop for a vanished prompt. If a repro shows this firing with
        // hadPending=true (a row WAS waiting but reconcile missed it), that's the
        // bug; hadPending=false is the legitimate automatic-resume-echo case.
        vlog('seed-dedup drop user_text', {
          paneId: e.paneId,
          textHash: textHash(t),
          seq: e.seq,
          hadPending: (pendingByPane.get(e.paneId)?.length ?? 0) > 0,
        });
        c.userTexts.delete(t);
        return false;
      }
    }
    if (!e.fromReplay) {
      // A LIVE user_text anchors at the START of the turn it triggered — the
      // CLI's replay echo can arrive AFTER the reply deltas, so appending at the
      // tail would render it below the answer (see insertLiveUserTextAtTurnStart).
      // Seed (replay) user_text is already correctly ordered in the transcript,
      // so it falls through to the normal append below.
      insertLiveUserTextAtTurnStart(c, e);
      return true;
    }
  }

  // Coalesce streaming deltas into their owning block. The daemon emits the
  // same messageId across a message's delta run; we keep ONE block event in the
  // list and append delta text to it. The final non-delta `text`/`thinking`
  // event (if any) carries the full text and supersedes the accumulated block.
  if (e.type === 'text_delta' || e.type === 'thinking_delta') {
    // Phase-0 perf: stamp time-to-first-token on the FIRST LIVE streamed delta of a
    // turn (no-op unless instrumentation is enabled). Self-guards to fire once per
    // turn, but gate on !e.fromReplay so a transcript-seed replay never stamps a
    // bogus "first token" for a turn the user didn't just submit.
    if (!e.fromReplay) perfFirstDelta(e.paneId);
    const mid = e.messageId;
    if (mid) {
      const idx = c.blockByMessageId.get(mid);
      if (idx !== undefined) {
        const block = c.events[idx];
        // Mutate a fresh object so React identity changes for that row.
        const merged: NativeEvent = {
          ...block,
          text: (block.text ?? '') + (e.text ?? ''),
          streaming: true,
        };
        c.events[idx] = merged;
        return true;
      }
      // First delta for this messageId — seed a block (normalize delta → block
      // type so the renderer treats it as a coalesced block).
      const blockType = e.type === 'text_delta' ? 'text' : 'thinking';
      const seed: NativeEvent = { ...e, type: blockType, text: e.text ?? '', streaming: true };
      c.events.push(seed);
      c.blockByMessageId.set(mid, c.events.length - 1);
      return true;
    }
    // Delta with no messageId — append as-is.
    c.events.push(e);
    return true;
  }

  // A terminal `text`/`thinking` event with a messageId we've been
  // accumulating: replace the in-progress block with the authoritative full
  // text rather than appending a duplicate.
  if ((e.type === 'text' || e.type === 'thinking') && e.messageId) {
    const idx = c.blockByMessageId.get(e.messageId);
    if (idx !== undefined) {
      // Preserve the in-progress block's start time + stamp an end time so a
      // collapsed thinking block can show "thought for Xs". The authoritative
      // event carries no clientTs of its own (it may be this same supersede).
      const prev = c.events[idx];
      c.events[idx] = { ...e, clientTs: prev.clientTs ?? e.clientTs, clientTsEnd: Date.now() };
      return true;
    }
    c.events.push(e);
    c.blockByMessageId.set(e.messageId, c.events.length - 1);
    return true;
  }

  // A turn boundary settles any still-streaming block: the daemon won't always
  // send an authoritative non-delta text to supersede the coalesced deltas, so
  // clear the caret here too. (turn_end/usage/error settle the spinner; mirror
  // that for the caret.)
  if (e.type === 'turn_end' || e.type === 'usage' || e.type === 'error') {
    // Phase-0 perf: log turn duration + publish count on settle (no-op unless enabled).
    perfTurnEnd(e.paneId);
    for (let i = 0; i < c.events.length; i++) {
      const b = c.events[i];
      // Stamp an end time on settle (best-effort, live blocks only) so a
      // collapsed thinking block can show its duration.
      if (b.streaming)
        c.events[i] = { ...b, streaming: false, clientTsEnd: b.clientTsEnd ?? Date.now() };
    }
  }

  c.events.push(e);
  return true;
}

// Hoist all FromReplay (transcript-seed) events to the FRONT of the folded
// list, stably preserving their relative order and the relative order of the
// live events after them. Seeded events carry high, live-disjoint seqs (the
// daemon stamps them into a reserved block) and are meant to render as prior
// history AHEAD of the live conversation. They normally fold first (into an
// empty cache on mount), but if a live SSE snapshot frame raced in before the
// seed fetch resolved, foldEvent would have appended the seeds after the live
// rows. This one-shot reorder (run only when a seed just landed) restores the
// history-first ordering. Returns true if the order changed.
function hoistSeedToFront(c: PaneCache): boolean {
  const evts = c.events;
  // Fast path: already partitioned (a prefix of replay then no more replay).
  let firstLive = -1;
  let needsReorder = false;
  for (let i = 0; i < evts.length; i++) {
    if (evts[i].fromReplay) {
      if (firstLive !== -1) {
        needsReorder = true;
        break;
      }
    } else if (firstLive === -1) {
      firstLive = i;
    }
  }
  if (!needsReorder) return false;
  const replay: NativeEvent[] = [];
  const live: NativeEvent[] = [];
  for (const e of evts) (e.fromReplay ? replay : live).push(e);
  c.events = replay.concat(live);
  // Block indices are now stale; rebuild them from the new order so subsequent
  // delta coalescing still finds its in-progress block.
  c.blockByMessageId.clear();
  for (let i = 0; i < c.events.length; i++) {
    const ev = c.events[i];
    if ((ev.type === 'text' || ev.type === 'thinking') && ev.messageId) {
      c.blockByMessageId.set(ev.messageId, i);
    }
  }
  return true;
}

// Apply a since=0 transcript-seed response into a pane cache. Folds the seed +
// ring (hoisting the FromReplay rows to the front) and returns whether anything
// changed (caller publishes). Latches `c.seeded` ONLY when the response carried
// events — an EMPTY-but-successful 200 must NOT consume the one-shot seed.
//
// Right after a daemon auto-update swap the reattached daemon can momentarily
// return 200 with no events (the on-disk transcript isn't resolvable for an
// instant and the rebuilt in-memory ring is still empty — seedFromTranscript
// returns ([],nil), NOT an error). If we latched `seeded` there, the one-shot
// seed is consumed and the pane stays blank/partial until a manual hard refresh:
// the agent's byte-replay advances lastSeq>0, so every later resync pulls
// since=lastSeq and never re-fetches the since=0 transcript. Leaving `seeded`
// false lets es.onopen / the idle-resync re-attempt the seed until it lands
// content. A genuinely-empty pane (brand-new, no history) stays false harmlessly
// — the cheap since=0 re-poll keeps returning empty and latches the instant the
// first event exists. (We fold even when the daemon flags `truncated`: at
// since==0 the transcript seed carries the ENTIRE pre-floor history, so the
// folded result is complete regardless of ring rotation.)
function applySeedResult(c: PaneCache, events: NativeEvent[]): boolean {
  if (events.length === 0) return false; // don't latch — leave seeded=false to retry
  c.seeded = true;
  let changed = false;
  for (const ev of events) {
    if (foldEvent(c, ev)) changed = true;
  }
  if (hoistSeedToFront(c)) changed = true;
  return changed;
}

// Rebuild blockByMessageId after the events array is reordered (partition or
// splice) so in-progress delta coalescing still finds its block at the new index.
function rebuildBlockIndex(c: PaneCache): void {
  c.blockByMessageId.clear();
  for (let i = 0; i < c.events.length; i++) {
    const ev = c.events[i];
    if ((ev.type === 'text' || ev.type === 'thinking') && ev.messageId) {
      c.blockByMessageId.set(ev.messageId, i);
    }
  }
}

// Insert a LIVE user_text at the START of the turn it triggered, instead of at
// the array tail. Claude's `--replay-user-messages` echo is NOT guaranteed to
// precede the assistant reply on stdout — observed live, the reply's text_delta
// run can stream FIRST and the user echo arrive several seqs later (e.g. reply
// at seq 3-9, the "hi there" echo at seq 10). The daemon stamps seqs in stdout
// arrival order, so a late echo appended at the tail renders BELOW the reply it
// prompted (the recurring "my message is under Claude's answer" bug). The client
// knows the real turn structure — a user message opens its turn — so splice the
// echo just before the current in-flight turn's FIRST assistant-content event,
// walking back only within that turn (stop at a turn boundary, a prior
// user_text, or the seed/replay block; none may be crossed). When the echo
// arrives BEFORE any reply (the other side of the race) the scan finds no
// assistant content and this degrades to a plain append — correct either way.
function insertLiveUserTextAtTurnStart(c: PaneCache, e: NativeEvent): void {
  let insertIdx = c.events.length;
  for (let i = c.events.length - 1; i >= 0; i--) {
    const ev = c.events[i];
    // Never reorder across a turn boundary or into seeded history.
    if (ev.fromReplay || ev.type === 'turn_end' || ev.type === 'user_text') break;
    // Track the EARLIEST assistant-content row of this in-flight turn; the
    // user_text belongs just before it. (Render-filtered rows like session_init
    // / status are stepped past harmlessly.)
    if (
      ev.type === 'text' ||
      ev.type === 'thinking' ||
      ev.type === 'tool_call' ||
      ev.type === 'tool_result'
    ) {
      insertIdx = i;
    }
  }
  if (insertIdx === c.events.length) {
    c.events.push(e); // echo arrived first (or empty turn) — plain append
    return;
  }
  c.events.splice(insertIdx, 0, e);
  rebuildBlockIndex(c); // positions shifted; keep delta coalescing valid
}

// Compact an oversized cache by dropping the OLDEST live (non-replay) events,
// keeping the ENTIRE seed block (FromReplay history) plus the most recent
// `keepRecent` live events. Prunes `seen` for the dropped live seqs and rebuilds
// `blockByMessageId` (indices shift). Returns true if it dropped anything.
//
// Why this is safe against the resync/seed invariants (studied before writing):
//  • lastSeq (the onopen-resync / gap cursor) = the HIGHEST live seq, which lives
//    in the most-recent live events we KEEP — so dropping the oldest live rows
//    never moves it. The resync `backfill(lastSeq)` only requests seq>lastSeq, so
//    it can never re-deliver a dropped (older) event.
//  • We drop only LIVE events, never FromReplay — so the seed block stays intact
//    and hoistSeedToFront's history-first ordering is preserved. The since=0 seed
//    is only re-fetched on a truncated rebuild (resetCache throws the whole cache
//    away) or the once-guarded seedOnce, never for these old live seqs.
//  • `seen` is pruned ONLY for the dropped seqs; the kept live seqs + all seed
//    seqs remain, so dedup of the snapshot/live overlap and seed re-emits is
//    unchanged for everything still in the list.
//  • `userTexts` is deliberately NOT pruned: it only ever holds SEED prompt texts
//    (foldEvent adds FromReplay user_text only), and we never drop seeds — so
//    every userTexts entry still corresponds to a live, un-consumed seed prompt.
//    Pruning it would un-dedup a seed prompt whose live echo hasn't arrived yet.
//  • blockByMessageId is rebuilt from the new indices (a dropped in-progress block
//    simply won't be found, which is correct — its deltas are gone too).
function compactCache(c: PaneCache, keepRecent: number): boolean {
  const evts = c.events;
  // Partition preserving order: all replay rows are kept; live rows are trimmed
  // to the most recent `keepRecent`. (Replay is hoisted to the front, but we
  // partition rather than assume the layout to stay robust to a pre-hoist call.)
  const replay: NativeEvent[] = [];
  const live: NativeEvent[] = [];
  for (const e of evts) (e.fromReplay ? replay : live).push(e);
  if (live.length <= keepRecent) return false; // nothing to drop
  const dropCount = live.length - keepRecent;
  const dropped = live.slice(0, dropCount);
  const keptLive = live.slice(dropCount);
  // Prune `seen` for the dropped live seqs (never for kept/seed seqs).
  for (const e of dropped) c.seen.delete(e.seq);
  c.events = replay.concat(keptLive);
  // Indices shifted; rebuild the in-progress-block map from the new order.
  c.blockByMessageId.clear();
  for (let i = 0; i < c.events.length; i++) {
    const ev = c.events[i];
    if ((ev.type === 'text' || ev.type === 'thinking') && ev.messageId) {
      c.blockByMessageId.set(ev.messageId, i);
    }
  }
  return true;
}

// ---------------------------------------------------------------------------
// Pending permission_request derivation.
// ---------------------------------------------------------------------------

// ALL unresolved permission_requests, oldest-first. Claude fires parallel tool
// calls, so SEVERAL approvals can be open at once (the daemon now parks them in a
// map keyed by requestId; a single slot used to orphan all but the last and hang
// the agent). A request is resolved when: we locally POSTed a resolution
// (resolvedIds), a permission_resolved event for its requestId has landed (the
// daemon accepted an allow/deny response), a tool_result for its callId has
// landed, OR a later turn_end/error closed its turn. permission_resolved is the
// immediate durable signal for server-side ARIA/allowlist/manual resolutions;
// tool_result remains the compatibility fallback for older daemons.
function derivePendingApprovals(events: NativeEvent[], resolvedIds: Set<string>): NativeEvent[] {
  const ranCallIds = new Set<string>();
  const resolvedRequestIds = new Set<string>();
  let lastTurnEndIdx = -1;
  for (let i = 0; i < events.length; i++) {
    const e = events[i];
    if (e.type === 'tool_result' && e.callId) ranCallIds.add(e.callId);
    if (e.type === 'permission_resolved' && e.requestId) resolvedRequestIds.add(e.requestId);
    if (e.type === 'turn_end' || e.type === 'error') lastTurnEndIdx = i;
  }
  const out: NativeEvent[] = [];
  for (let i = 0; i < events.length; i++) {
    const e = events[i];
    if (e.type !== 'permission_request') continue;
    if (i < lastTurnEndIdx) continue; // a later turn boundary closed this approval
    if (e.requestId && resolvedIds.has(e.requestId)) continue; // locally resolved
    if (e.requestId && resolvedRequestIds.has(e.requestId)) continue; // daemon resolved
    if (e.callId && ranCallIds.has(e.callId)) continue; // tool ran / answered
    out.push(e);
  }
  return out;
}

// The current "next-prompt" suggestion for a pane: the most recent LIVE (not
// fromReplay) prompt_suggestion that is newer than the last turn boundary
// (turn_end / user_text / error — a later turn or the user's next message
// supersedes it) and newer than the consumed seq (Tab-accepted). Returns the
// event (carries .text) or null. Single pass like derivePendingApprovals.
function derivePromptSuggestion(events: NativeEvent[], consumedSeq: number): NativeEvent | null {
  let lastBoundarySeq = 0;
  let candidate: NativeEvent | null = null;
  for (let i = 0; i < events.length; i++) {
    const e = events[i];
    if (e.type === 'turn_end' || e.type === 'user_text' || e.type === 'error') {
      if (e.seq > lastBoundarySeq) lastBoundarySeq = e.seq;
      continue;
    }
    if (e.type === 'prompt_suggestion' && !e.fromReplay && e.seq > consumedSeq) {
      candidate = e; // keep the latest qualifying one
    }
  }
  if (!candidate) return null;
  // A turn boundary AFTER the candidate supersedes it.
  return candidate.seq > lastBoundarySeq ? candidate : null;
}

// ---------------------------------------------------------------------------
// The hook.
// ---------------------------------------------------------------------------

export interface UseNativePaneResult {
  events: NativeEvent[];
  pending: PendingMsg[];
  sessionInit: NativeEvent | null;
  // pendingApprovals: ALL open permission requests (parallel tool calls can open
  // several at once). pendingApproval is the first, kept for the simple consumers
  // (header dot, composer focus) that only need "is anything pending".
  pendingApprovals: NativeEvent[];
  pendingApproval: NativeEvent | null;
  connected: boolean;
  send: (text: string, localMsgId?: string) => Promise<NativeInputResponse>;
  sendOptimistic: (text: string) => Promise<NativeInputResponse>;
  // resend — send `text` as a NEW optimistic user turn (message-action "resend"
  // / "retry"). Thin alias of sendOptimistic, named for intent at the call site.
  resend: (text: string) => Promise<NativeInputResponse>;
  retryPending: (localMsgId: string) => Promise<NativeInputResponse | void>;
  resolve: (reqId: string, allow: boolean, reason?: string) => Promise<ResolveApprovalResponse>;
  interrupt: () => Promise<NativeInterruptResponse>;
  // Per-pane "always allow" tool list (exact tool names). The daemon
  // auto-approves future permission_requests for any tool on it. Loaded once on
  // mount/connect (module-cached so it survives remounts); add/remove update
  // optimistically and reconcile with the daemon's returned list.
  allowlist: string[];
  addToAllowlist: (tool: string) => Promise<void>;
  removeFromAllowlist: (tool: string) => Promise<void>;
  // promptSuggestion: Claude's own model-generated "next prompt" for this pane
  // (null when none / consumed / superseded by a newer turn). Rendered as composer
  // ghost text; Tab calls consumeSuggestion to dismiss it.
  promptSuggestion: NativeEvent | null;
  consumeSuggestion: () => void;
  // hydrated: a since=0 transcript seed has completed against a NATIVE pane
  // (even an empty one). False during the Term→Focus kill→respawn window,
  // where /native/history 409s ("pane is not native") — the transcript is
  // NOT trustworthy yet, so the empty state must read as "restoring", not
  // "message Claude to start".
  hydrated: boolean;
}

export function useNativePane(paneId: string, pid?: number, epoch?: number, active: boolean = true): UseNativePaneResult {
  // activeBackend + cloud JWT are stream deps: a backend swap or a Supabase
  // token rotation must tear down and rebuild the native stream so it
  // reconnects with current routing and credentials. Mirrors App.tsx's
  // uiEvents effect deps.
  const activeBackend = useStore((s) => s.activeBackend);
  const cloudSessionToken = useCloud((s) => s.session?.access_token ?? null);

  // Snapshot of the folded list. Seeded from the module cache so a remount
  // shows the prior conversation immediately (no blank flash).
  const [events, setEvents] = useState<NativeEvent[]>(() => getCache(paneId).events.slice());
  const [connected, setConnected] = useState(false);

  // Whether a since=0 seed has completed for the CURRENT pane incarnation.
  // Initialized from the cache's seeded latch so a warm remount of an
  // already-populated pane doesn't flash the "restoring" state.
  const [hydrated, setHydrated] = useState(() => getCache(paneId).seeded);

  // Mirror of the optimistic pending list. Bumped whenever we push/fail/reconcile
  // a pending row. Kept as state (not just the module map) so render re-runs.
  const [pending, setPending] = useState<PendingMsg[]>(() => getPending(paneId).slice());

  // Locally-resolved approval ids — drives optimistic pending dismissal so the
  // buttons disable-on-click before the daemon's tool_result lands.
  const resolvedIdsRef = useRef<Set<string>>(new Set());
  const [resolvedTick, setResolvedTick] = useState(0);

  // Seq of the prompt-suggestion the user Tab-accepted (or a newer turn
  // superseded). Module-cached (getConsumedSuggestionSeq) so a warm-pane remount
  // within the ~10-20s suggestion window does not re-show an accepted one.
  const consumedSuggestionSeqRef = useRef(getConsumedSuggestionSeq(paneId));
  const [suggestionTick, setSuggestionTick] = useState(0);

  // sseEpoch forces a teardown+rebuild after a fatal close, exactly like
  // App.tsx (EventSource bakes its URL, so a stale-token loop needs a fresh
  // construction to recover).
  const [sseEpoch, setSseEpoch] = useState(0);

  // Mirror of the per-pane allowlist module cache. Seeded from the cache so a
  // remount shows the prior list immediately; loaded from the daemon once per
  // pane (the effect below) and reconciled on each add/remove.
  const [allowlist, setAllowlistState] = useState<string[]>(
    () => getAllowlistCache(paneId).tools.slice(),
  );

  // Re-seed local state whenever the target pane changes (warm cache for the
  // new pane, fresh resolved set).
  useEffect(() => {
    resolvedIdsRef.current = new Set();
    consumedSuggestionSeqRef.current = getConsumedSuggestionSeq(paneId);
    setEvents(getCache(paneId).events.slice());
    setPending(getPending(paneId).slice());
    setAllowlistState(getAllowlistCache(paneId).tools.slice());
    setHydrated(getCache(paneId).seeded);
  }, [paneId]);

  // BUG 2 — respawn detection (Terminal↔Native mode switch). The daemon kills +
  // respawns the pane as a NEW agent process for the SAME pane id, resetting its
  // ring to seq 1,2,3…; the surviving client cache would dedup those low seqs as
  // "already seen" and the conversation would freeze/blank after the switch. A
  // respawn always lands a NEW os pid, so on a pid CHANGE (never the initial
  // observation — see notePidAndDetectRespawn) we fully reset the cache
  // (seeded=false so the since=0 seed re-runs for the fresh ring), re-mirror the
  // now-empty state, and bump sseEpoch to tear down + rebuild the EventSource so
  // it re-snapshots the fresh ring (and the SSE effect re-runs seedOnce). A
  // warm-pane remount with the SAME pid does NOT reset — that's the whole point
  // of the persistent cache, so the conversation never blanks on a workspace
  // switch / zoom / drag. Keyed on [paneId, pid, epoch]; paneId in the dep list
  // keeps a pane swap from leaking the prior pane's pid into the new pane's
  // detection (notePidAndDetectRespawn keys by paneId, so the first pid seen for a
  // brand-new pane id is recorded, not flagged).
  //
  // The SAME reset also fires on a daemon EPOCH change (a daemon restart/swap): the
  // surviving agent keeps its pid, so the pid check alone misses it, but the daemon
  // rebuilt the ring from seq 1 just the same — making a mid-turn reattach lossless
  // requires the identical rebuild-from-transcript+ring path. Both note* calls must
  // run UNCONDITIONALLY (they record the latest value as a side effect) before the
  // OR, or whichever is short-circuited would miss its next change.
  useEffect(() => {
    const pidReset = notePidAndDetectRespawn(paneId, pid);
    // hadContent = the cache already holds a live session, which lets the epoch
    // detector catch the epoch-less→epoch transitional swap (see its doc). Read
    // BEFORE resetCacheForRespawn clears it below.
    const epochReset = noteEpochAndDetectReset(paneId, epoch, getCache(paneId).lastSeq > 0);
    if (!pidReset && !epochReset) return;
    resolvedIdsRef.current = new Set();
    resetCacheForRespawn(paneId);
    setEvents([]);
    setPending([]);
    // The fresh incarnation hasn't been seeded yet — drop back to "restoring"
    // so the empty transcript doesn't read as a brand-new session.
    setHydrated(false);
    // Force the SSE teardown+rebuild; the rebuilt effect's seedOnce re-runs
    // (seeded is now false) and onopen backfills from the cleared lastSeq=0.
    setSseEpoch((n) => n + 1);
  }, [paneId, pid, epoch]);

  // Load the pane's allowlist ONCE per pane (module-cached `loaded` guard so a
  // warm-pane remount doesn't re-fetch or blank the mirror). Writes through the
  // cache then mirrors to state. Transient failures leave `loaded` false so a
  // later remount retries.
  useEffect(() => {
    const cache = getAllowlistCache(paneId);
    if (cache.loaded) {
      setAllowlistState(cache.tools.slice());
      return;
    }
    let cancelled = false;
    void getAllowlist(paneId)
      .then((res) => {
        if (cancelled) return;
        const c = getAllowlistCache(paneId);
        c.tools = res.tools ?? [];
        c.loaded = true;
        setAllowlistState(c.tools.slice());
      })
      .catch(() => {
        // Transient — leave `loaded` false; the next remount retries.
      });
    return () => {
      cancelled = true;
    };
  }, [paneId]);

  // Bump the pending mirror from the module map (after a push/fail/reconcile).
  const bumpPending = useCallback(() => {
    setPending(getPending(paneId).slice());
  }, [paneId]);

  useEffect(() => {
    // A native SSE is a held-open HTTP/1.1 connection. The desktop in Local mode
    // talks to the daemon over plain HTTP/1.1 (http://127.0.0.1:8787), which the
    // browser caps at 6 connections per host. Each native pane holds one SSE,
    // plus one global UI-events SSE — so with several native panes (warm-parked
    // ones keep their stream open too) the 6 slots fill up and the browser can no
    // longer open ANY new HTTP connection: the health poll fails ("daemon
    // disconnect"), a reload's navigation can't connect (dead reload), and API
    // polls stall — while WebSockets (a separate pool) keep panes + hotkeys
    // working. (The phone is immune: the cloud is HTTP/2, multiplexed.) So only
    // hold the SSE while this pane is in the ACTIVE layout; a parked pane keeps
    // its DOM warm (instant switch-back) but drops the stream and re-subscribes
    // — ring snapshot + seq-dedup — when shown again.
    if (!active) {
      setConnected(false);
      return;
    }
    let closed = false;
    let backfilling = false;
    let seeding = false;
    const es = openNativeStream(paneId);

    // Re-mirror the module cache into React state on EVERY (re-)entry of this
    // effect. It re-runs — component still MOUNTED — when sseEpoch / the cloud token
    // / the active backend change; on such a re-run the lazy useState init
    // (mount-only) does NOT re-read the cache and the [paneId] re-seed effect does
    // NOT fire (paneId unchanged). Since the teardown cancels any rAF-pending flush,
    // a re-run landing within a frame of a turn's final settle would otherwise
    // STRAND that last frame (final delta + cleared caret) until the next live event.
    // This synchronous re-mirror closes that gap — idempotent, one extra render only
    // on the (infrequent) dep changes.
    setEvents(getCache(paneId).events.slice());

    // --- rAF-coalesced publish -------------------------------------------------
    //
    // A streaming turn folds ~60 deltas/sec (one per token), and EVERY fold used to
    // call publish() synchronously → a fresh `events` array identity → a full
    // transcript rebuild + React re-render PER TOKEN. That render storm is the "Focus
    // feels laggier" cost. The fold itself is cheap (it grows a block string in the
    // module cache); only the React state bump is expensive, and the screen paints at
    // most once per frame anyway. So we COALESCE: many publish() calls within one
    // animation frame collapse into a single setEvents on the next frame. The cache
    // is the source of truth, so the coalesced flush always mirrors the LATEST folded
    // state, never a stale intermediate.
    //
    // LAST-FRAME CORRECTNESS: a turn's settle events (turn_end / usage / error) also
    // flow through foldEvent → publish, so each schedules a flush; the final settle
    // leaves a pending rAF that flushes the last delta + cleared caret. The teardown
    // cancels any still-pending rAF; on a true unmount there's nothing to paint, and
    // on an effect RE-RUN the entry re-mirror above repaints — so cancelling never
    // strands content.
    let pubRaf = 0;

    const flushPublish = () => {
      if (closed) return; // effect torn down — never setState on a dead instance
      pubRaf = 0;
      const c = getCache(paneId);
      // Safety valve: keep the unbounded client cache from growing forever on a
      // very long-lived pane. Only trips well above the daemon ring cap (4000);
      // drops the oldest LIVE rows, never the seed block or the resync cursor.
      if (c.events.length > COMPACT_HIGH_WATER) compactCache(c, COMPACT_KEEP_RECENT);
      setEvents(c.events.slice());
      // Count the React state bump (no-op unless instrumentation is on) so the
      // rAF-coalescing win is measurable.
      perfPublish(paneId);
      // foldEvent may have reconciled (dropped) an optimistic pending row when a
      // user_text landed; re-mirror the pending list so the synthetic row clears
      // the instant the real echo arrives (no double-render). Per-pane so a
      // coalesced flush only mirrors when THIS pane reconciled (never a sibling's).
      if (pendingReconciledByPane.has(paneId)) {
        pendingReconciledByPane.delete(paneId);
        setPending(getPending(paneId).slice());
      }
    };

    // publish — schedule at most ONE flushPublish per animation frame. Falls back to
    // a synchronous flush when requestAnimationFrame is unavailable (jsdom / SSR).
    const publish = () => {
      if (typeof requestAnimationFrame !== 'function') {
        flushPublish();
        return;
      }
      if (pubRaf) return;
      pubRaf = requestAnimationFrame(flushPublish);
    };

    // One-time transcript seed. On mount for a native pane we pull
    // /native/history?since=0 ONCE: the daemon PREPENDS the pane's prior
    // conversation re-derived from the on-disk Claude transcript (marked
    // FromReplay, stamped into a high live-disjoint seq block) ahead of the live
    // ring. `claude --resume` restores CONTEXT but replays NO events, so without
    // this a resumed/flipped native pane is blank until the next message.
    //
    // Idempotent: guarded by the per-pane `seeded` flag so a warm-pane remount
    // doesn't re-seed an already-populated cache. After folding, hoist the
    // FromReplay rows to the front in case a live SSE snapshot frame raced ahead
    // of this fetch. foldEvent dedups seed-vs-live user_text by content, so the
    // seeded last prompt never doubles the live replay-echo (or vice versa).
    let seedRetryTimer: number | undefined;
    let seedRetries = 0;
    const seedOnce = async () => {
      const c0 = getCache(paneId);
      if (c0.seeded || seeding) return;
      seeding = true;
      try {
        const res = await fetchNativeHistory(paneId, 0);
        if (closed) return;
        // A 200 (even empty) means the daemon answered for a NATIVE pane — the
        // transcript state is trustworthy now, so the empty state may read as a
        // genuinely-fresh session instead of "restoring".
        seedRetries = 0;
        setHydrated(true);
        // applySeedResult latches `seeded` ONLY on a non-empty response, so an empty
        // 200 (daemon mid-swap reattach) leaves the seed un-consumed for es.onopen /
        // the idle-resync to re-attempt — the app-update blank/partial-pane fix.
        if (applySeedResult(getCache(paneId), res.events)) publish();
      } catch (err) {
        // 409 = "pane is not native": the Term→Focus kill→respawn is still in
        // flight (worst case ~5.5s across the daemon's settle+retry+grace
        // budget). The WS is closed for the same reason, so the usual onopen
        // re-seed can't fire — poll on a short timer instead of leaving the
        // transcript blank until the ~3s pid-poll heal. Everything else stays
        // swallow-and-wait: the onopen resync retries the same since=0 pull.
        if (
          !closed &&
          err instanceof ApiError &&
          err.status === 409 &&
          seedRetries < SEED_409_MAX_RETRIES
        ) {
          seedRetries++;
          seedRetryTimer = window.setTimeout(() => {
            seedRetryTimer = undefined;
            void seedOnce();
          }, SEED_409_RETRY_MS);
        }
      } finally {
        seeding = false;
      }
    };
    void seedOnce();

    // Backfill the gap between our cursor and `since` via /native/history.
    // If the history came back truncated (ring floor passed us) we cannot heal
    // contiguously: drop the cache and let the next SSE snapshot re-seed.
    const backfill = async (sinceSeq: number) => {
      if (backfilling) return;
      backfilling = true;
      try {
        const res = await fetchNativeHistory(paneId, sinceSeq);
        if (closed) return;
        if (res.truncated) {
          // The ring floor passed our cursor — events between `since` and the
          // floor aged out, so our folded live events are no longer contiguous.
          // The daemon includes the FULL transcript seed in a truncated response,
          // so REBUILD from scratch: reset the cache, then fold the seed + the
          // current ring tail. This restores the early history the ring no longer
          // holds (without it the conversation would appear to start at the floor).
          const c = resetCache(paneId);
          for (const ev of res.events) foldEvent(c, ev);
          hoistSeedToFront(c);
          publish();
          return;
        }
        const c = getCache(paneId);
        let changed = false;
        for (const ev of res.events) {
          if (foldEvent(c, ev)) changed = true;
        }
        if (changed) publish();
      } catch {
        // Transient — the next onopen resync or live delta will retry.
      } finally {
        backfilling = false;
      }
    };

    es.onopen = () => {
      if (closed) return;
      setConnected(true);
      // If the one-shot transcript seed hasn't landed content yet (e.g. it raced a
      // daemon-swap window and got an empty 200), re-attempt it now. The backfill
      // below uses since=lastSeq, which won't re-pull the since=0 transcript once the
      // agent byte-replay has advanced lastSeq past 0 — so seedOnce is the only path
      // that restores pre-swap history. No-op once seeded.
      if (!getCache(paneId).seeded) void seedOnce();
      // Resync: SSE never replays frames fired while we were disconnected, so
      // pull anything past our cursor. On the very first connect lastSeq is 0
      // and the daemon's own snapshot covers it; the history call is then a
      // cheap no-op (since=0 → full/empty depending on ring).
      vlog('onopen → backfill', {
        paneId,
        since: getCache(paneId).lastSeq,
        seeded: getCache(paneId).seeded,
        pendingCount: getPending(paneId).length,
      });
      void backfill(getCache(paneId).lastSeq);
    };

    es.onmessage = (raw) => {
      if (closed) return;
      let frame: unknown;
      try {
        frame = JSON.parse(raw.data as string);
      } catch {
        return; // swallow malformed frames, like App.tsx
      }
      // Epoch control frame (replaces the SSE named `epoch` event — WS has no named
      // events). The daemon sends it FIRST, before the ring snapshot, so a
      // daemon-swap reset lands BEFORE the fresh low-seq events fold (else the
      // "missing latest messages until a Term toggle" bug). A NativeEvent always
      // has seq+type and never `kind`, so this discriminates unambiguously. Resets
      // the cache + bumps sseEpoch to teardown+reopen against the fresh ring.
      if (frame && typeof frame === 'object' && (frame as { kind?: string }).kind === 'epoch') {
        const ep = (frame as { nativeEpoch?: number }).nativeEpoch ?? 0;
        // hadContent read BEFORE the reset clears it (matches the prop-effect order).
        if (noteEpochAndDetectReset(paneId, ep, getCache(paneId).lastSeq > 0)) {
          resolvedIdsRef.current = new Set();
          resetCacheForRespawn(paneId);
          setEvents([]);
          setPending([]);
          setSseEpoch((n) => n + 1);
        }
        return;
      }
      const e = frame as NativeEvent;
      const c = getCache(paneId);
      // Gap detection: a live event whose seq jumps past our cursor+1 means we
      // missed events between them. We MUST backfill the hole BEFORE folding
      // this newer event — foldEvent appends in call order, so folding the
      // newer event first would leave the older backfilled events appended
      // after it, rendering the transcript out of order. So: backfill the gap
      // (await), then fold this event on top. foldEvent dedups by seq, so if
      // the backfill already contained this event (race) the later fold is a
      // benign no-op.
      if (c.lastSeq > 0 && e.seq > c.lastSeq + 1 && !c.seen.has(e.seq)) {
        const sinceSeq = c.lastSeq;
        void backfill(sinceSeq).then(() => {
          if (closed) return;
          // Re-fetch the cache: backfill may have reset it (truncated history),
          // in which case the daemon's own snapshot replay re-seeds and folding
          // this now-stale event would corrupt the fresh ordering — skip it.
          const cc = getCache(paneId);
          if (cc !== c) {
            // Phase-0 vanish instrumentation: the triggering event (possibly a
            // user_text) is dropped here and we rely on the snapshot re-seed to
            // re-supply it. Logs so a vanish that traces to this path self-reports.
            vlog('gap-detect drop (cache reset under backfill)', {
              paneId,
              seq: e.seq,
              type: e.type,
              sinceSeq,
            });
            return; // cache was reset under us; let the snapshot heal
          }
          // If the gap closed (cursor advanced to e.seq-1), e now appends in
          // order. If the backfill was a no-op (in-flight guard) the gap may
          // still be open, but folding here keeps liveness; foldEvent dedups so
          // a later contiguous arrival is harmless.
          if (foldEvent(cc, e)) publish();
        });
        return;
      }
      if (foldEvent(c, e)) publish();
    };

    // (The epoch reset is now handled inline in onmessage above — WS has no named
    // events, so the daemon sends {"kind":"epoch",…} as the FIRST frame and
    // onmessage discriminates it from NativeEvents. The [paneId,pid,epoch] prop
    // effect remains the ~3s poll-driven backstop.)

    let rebuildTimer: number | undefined;
    es.onerror = () => {
      // onclose fires right after onerror; reconnect is handled there.
    };
    es.onclose = () => {
      if (closed) return;
      setConnected(false);
      // Phase-0 vanish instrumentation: every close→2s-rebuild. The lastSeq logged
      // here is the cursor the rebuilt stream's onopen will hand to backfill — if a
      // prompt vanished across a reconnect, this is the blind window it fell into.
      vlog('onclose → scheduling rebuild', {
        paneId,
        lastSeq: getCache(paneId).lastSeq,
        seeded: getCache(paneId).seeded,
        pendingCount: getPending(paneId).length,
      });
      // WebSocket does NOT auto-reconnect (unlike EventSource). Reconnect by
      // bumping sseEpoch (re-runs this effect → fresh socket via openNativeStream,
      // which also re-reads the current token), debounced ~2s. This reuses the
      // existing rebuild path as the reconnect loop. A normal teardown sets
      // closed=true before es.close(), so its onclose returns here without
      // reconnecting. ~2s is gentler than EventSource's own auto-retry, so this
      // is no more storm-prone than the SSE path was during a daemon swap.
      if (rebuildTimer === undefined) {
        rebuildTimer = window.setTimeout(() => setSseEpoch((n) => n + 1), 2000);
      }
    };

    // BUG 1 — periodic idle-resync. The daemon's SSE fanout is non-blocking
    // drop-on-full, and a HALF-OPEN connection (TCP looks alive, nothing flows)
    // delivers no frames and never trips es.onopen or es.onerror — so the two
    // existing resync triggers can both miss a dropped TAIL: gap-detection only
    // fires when a LATER live event arrives (but turn_end is usually the last
    // event of a turn, so nothing later ever comes) and onopen only fires on a
    // real reconnect (which a half-open stall doesn't do). The spinner then
    // sticks on "Claude is thinking…" forever though the daemon HAS the reply.
    // A steady backfill(lastSeq) self-heals any dropped/stalled tail within a
    // few seconds regardless of cause: it's idempotent (guarded by `backfilling`)
    // and fetches /native/history?since=lastSeq — almost always a no-op (nothing
    // past the cursor), so the cost is one cheap GET every interval. Only runs
    // while the EventSource is OPEN so a backgrounded/closing stream isn't polled.
    let resyncTick = 0;
    const resyncTimer = window.setInterval(() => {
      if (!shouldIdleResync(closed, es.readyState)) return;
      resyncTick++;
      // Mid-turn (a block is still streaming in the cache — incl. the stuck case
      // where a dropped turn_end left it streaming forever): resync EVERY tick
      // (~RESYNC_ACTIVE_MS) to heal a dropped tail fast. Idle: only every
      // RESYNC_IDLE_TICKS-th tick (~7.5s). The predicate reads the module cache, so
      // it costs nothing on the SSE effect's dependency list.
      const midTurn = getCache(paneId).events.some((e) => e.streaming);
      if (!midTurn && resyncTick % RESYNC_IDLE_TICKS !== 0) return;
      // Keep re-attempting the one-shot transcript seed until it lands content — a
      // seed that raced a daemon-swap window and got an empty 200 left `seeded`
      // false, and backfill(since=lastSeq) can't restore pre-swap history once the
      // byte-replay advanced lastSeq>0. This is what self-heals the blank/partial
      // native pane after an app update without a manual refresh. No-op once seeded.
      if (!getCache(paneId).seeded) void seedOnce();
      void backfill(getCache(paneId).lastSeq);
    }, RESYNC_ACTIVE_MS);

    return () => {
      closed = true;
      // Cancel any publish scheduled for the next frame: we're tearing down, so a
      // setEvents on an unmounted/replaced effect is wasted. The cache keeps the
      // latest folded state; a fresh mount mirrors it and an effect re-run hits the
      // entry re-mirror above — so cancelling loses no content.
      if (pubRaf && typeof cancelAnimationFrame === 'function') cancelAnimationFrame(pubRaf);
      pubRaf = 0;
      if (rebuildTimer !== undefined) clearTimeout(rebuildTimer);
      if (seedRetryTimer !== undefined) clearTimeout(seedRetryTimer);
      clearInterval(resyncTimer);
      es.close();
    };
  }, [paneId, activeBackend, cloudSessionToken, sseEpoch, active]);

  // --- derived ---
  // session_init is captured on the cache as it folds, so this is an O(1) cache
  // read instead of an O(n) forward scan of `events` on every render (every
  // streaming text_delta bumps `events`). Keyed on [events, paneId] so it re-reads
  // the cache when the list grows — which also picks up the ONE upgrade a replayed
  // init undergoes when the first live init folds (see foldEvent) — or the pane
  // swaps. Returns a stable reference once the live init has landed.
  const sessionInit = useMemo(
    () => getCache(paneId).sessionInit,
    // events participates so the first mirror after the init folds picks it up.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [events, paneId],
  );
  // pendingApprovals does TWO O(n) passes; memoize it so it runs once per render
  // and only recomputes when the inputs change: `events` (the source list) or
  // `resolvedTick` (bumped on every mutation of resolvedIdsRef — a local resolve
  // or its rollback). Without this it re-derived on every render, including every
  // streaming text_delta that has no bearing on the approval set.
  const pendingApprovals = useMemo(
    () =>
      derivePendingApprovals(events, resolvedIdsRef.current).filter(
        // Suppress the approval card for a request the user already opted to
        // AUTO-ALLOW: the daemon auto-approves it on arrival
        // (maybeAutoAllowFromList), so showing the card just to clear it
        // ~instantly is the "auto-approve flash". Dropping it here also keeps its
        // callId OUT of pendingApprovalCallIds, so the tool_call card renders
        // normally (the tool just runs).
        //
        // Read the daemon's own verdict — do NOT re-derive it here. This used to
        // be `!allowlist.includes(a.toolName)`, which only ever matched WHOLE-TOOL
        // rules: every argument-scoped rule ("Bash(git status)") fell through and
        // flashed a full card. Matching those means parsing shell argv exactly as
        // the daemon does, forever — and drifting MORE permissive than it would
        // hide a card for a request nobody approved, hanging the agent on a
        // question the user can never answer. wouldAutoAllow also folds in the
        // cautious-mode veto, which the tool-name match ignored (so a withheld
        // request stayed hidden — the same hang, shipping today).
        (a) => !a.wouldAutoAllow,
      ),
    // resolvedIdsRef is a mutable ref; resolvedTick is its change signal.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [events, resolvedTick],
  );
  const pendingApproval = pendingApprovals.length ? pendingApprovals[0] : null;

  // The current prompt-suggestion ghost (most recent LIVE one not yet consumed
  // and not superseded by a newer turn). Recomputes on events change or on
  // suggestionTick (bumped by consumeSuggestion). Mirrors the pendingApprovals
  // memo's ref-as-dep pattern.
  const promptSuggestion = useMemo(
    () => derivePromptSuggestion(events, consumedSuggestionSeqRef.current),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [events, suggestionTick],
  );

  // consumeSuggestion — mark the current prompt-suggestion as Tab-accepted so it
  // stops deriving until a newer one arrives. Stamps the consumed seq into the
  // ref + module cache and bumps suggestionTick to re-derive this render.
  const consumeSuggestion = useCallback(() => {
    const cur = derivePromptSuggestion(events, consumedSuggestionSeqRef.current);
    if (!cur) return;
    consumedSuggestionSeqRef.current = cur.seq;
    setConsumedSuggestionSeq(paneId, cur.seq);
    setSuggestionTick((t) => t + 1);
  }, [events, paneId]);

  // --- actions ---
  const send = useCallback(
    (text: string, localMsgId?: string) => sendNativeInput(paneId, text, localMsgId),
    [paneId],
  );

  // sendOptimistic — push the user's message into the pending list SYNCHRONOUSLY
  // (so it renders this frame), then POST. On failure mark the entry .failed (do
  // NOT remove it — it stays as the retry anchor). The matching user_text replay
  // echo later reconciles + removes the (succeeded) entry in foldEvent.
  const sendOptimistic = useCallback(
    async (text: string): Promise<NativeInputResponse> => {
      const localMsgId =
        typeof crypto !== 'undefined' && crypto.randomUUID
          ? crypto.randomUUID()
          : `m-${Date.now()}-${Math.random().toString(36).slice(2)}`;
      const list = getPending(paneId);
      list.push({ localMsgId, text });
      // Phase-0 perf: stamp the per-pane turn clock at the optimistic send (no-op
      // unless enabled) so first-token / turn-duration measure from the user's Enter.
      perfTurnSubmit(paneId);
      bumpPending();
      try {
        return await sendNativeInput(paneId, text, localMsgId);
      } catch (err) {
        const cur = getPending(paneId);
        const idx = cur.findIndex((m) => m.localMsgId === localMsgId);
        if (idx >= 0) cur[idx] = { ...cur[idx], failed: true };
        bumpPending();
        throw err;
      }
    },
    [paneId, bumpPending],
  );

  // resend — send `text` as a NEW optimistic user turn. Used by the message-action
  // toolbar: "edit & resend" (after pushing into the composer the user may instead
  // send as-is), the user-message "resend" action, and the failed/error "retry"
  // (replays the last user prompt). It is exactly sendOptimistic — a separate name
  // so the call sites read by intent and we can evolve the two independently.
  const resend = useCallback(
    (text: string): Promise<NativeInputResponse> => sendOptimistic(text),
    [sendOptimistic],
  );

  // retryPending — re-send a failed optimistic row with a FRESH localMsgId, and
  // clear its failed flag while in flight (so it reads as sending again).
  const retryPending = useCallback(
    async (localMsgId: string): Promise<NativeInputResponse | void> => {
      const list = getPending(paneId);
      const idx = list.findIndex((m) => m.localMsgId === localMsgId);
      if (idx < 0) return;
      const text = list[idx].text;
      const fresh =
        typeof crypto !== 'undefined' && crypto.randomUUID
          ? crypto.randomUUID()
          : `m-${Date.now()}-${Math.random().toString(36).slice(2)}`;
      list[idx] = { localMsgId: fresh, text };
      bumpPending();
      try {
        return await sendNativeInput(paneId, text, fresh);
      } catch (err) {
        const cur = getPending(paneId);
        const j = cur.findIndex((m) => m.localMsgId === fresh);
        if (j >= 0) cur[j] = { ...cur[j], failed: true };
        bumpPending();
        throw err;
      }
    },
    [paneId, bumpPending],
  );

  const resolve = useCallback(
    async (reqId: string, allow: boolean, reason?: string) => {
      // Optimistically mark resolved so the pending banner dismisses on click;
      // the daemon is idempotent so a redundant resolve is a benign no-op.
      resolvedIdsRef.current.add(reqId);
      setResolvedTick((n) => n + 1);
      try {
        return await resolveNativeApproval(paneId, reqId, allow, reason);
      } catch (err) {
        // Roll back the optimistic dismissal so the user can retry.
        resolvedIdsRef.current.delete(reqId);
        setResolvedTick((n) => n + 1);
        throw err;
      }
    },
    [paneId],
  );

  const interrupt = useCallback(() => interruptNative(paneId), [paneId]);

  // addToAllowlist / removeFromAllowlist — mutate the per-pane allowlist
  // OPTIMISTICALLY (write the cache + mirror state synchronously so the UI
  // updates this frame), then POST and reconcile with the daemon's returned
  // authoritative list. On failure roll back to the pre-mutation list.
  const mutateAllowlist = useCallback(
    async (tool: string, action: 'add' | 'remove'): Promise<void> => {
      if (!tool) return;
      const cache = getAllowlistCache(paneId);
      const prev = cache.tools;
      // Optimistic next list (dedup on add; filter on remove). No-op early-out.
      let next: string[];
      if (action === 'add') {
        if (prev.includes(tool)) return;
        next = [...prev, tool];
      } else {
        if (!prev.includes(tool)) return;
        next = prev.filter((t) => t !== tool);
      }
      cache.tools = next;
      cache.loaded = true;
      setAllowlistState(next.slice());
      try {
        const res = await setAllowlist(paneId, tool, action);
        const c = getAllowlistCache(paneId);
        c.tools = res.tools ?? next;
        c.loaded = true;
        setAllowlistState(c.tools.slice());
      } catch (err) {
        // Roll back to the pre-mutation list so the chip/state reflects truth.
        const c = getAllowlistCache(paneId);
        c.tools = prev;
        setAllowlistState(prev.slice());
        throw err;
      }
    },
    [paneId],
  );
  const addToAllowlist = useCallback(
    (tool: string) => mutateAllowlist(tool, 'add'),
    [mutateAllowlist],
  );
  const removeFromAllowlist = useCallback(
    (tool: string) => mutateAllowlist(tool, 'remove'),
    [mutateAllowlist],
  );

  return {
    events,
    pending,
    sessionInit,
    pendingApprovals,
    pendingApproval,
    connected,
    send,
    sendOptimistic,
    resend,
    retryPending,
    resolve,
    interrupt,
    allowlist,
    addToAllowlist,
    removeFromAllowlist,
    promptSuggestion,
    consumeSuggestion,
    hydrated,
  };
}

// ---------------------------------------------------------------------------
// Test-only surface. Exposes the module-private fold cache so the
// respawn-reset behavior can be unit-tested without rendering useNativePane
// (the hook pulls the whole store/cloud graph into a React render, which OOM'd
// a prior agent under jsdom). Not imported by production code.
// ---------------------------------------------------------------------------
export const __useNativePaneTest = {
  getCache,
  foldEvent,
  applySeedResult,
  // Pure prompt-suggestion derivation (most-recent LIVE one not consumed and not
  // superseded by a later turn_end/user_text/error). Exposed for unit testing
  // without rendering the hook.
  derivePromptSuggestion,
  derivePendingApprovals,
  // The optimistic-pending list for a pane (sendOptimistic pushes here). Exposed
  // so tests can simulate a just-typed send and assert its echo reconciles
  // instead of being dropped as a seed duplicate.
  getPending,
  // Wipe ALL module caches so a test starts from a clean slate (caches survive
  // remount by design, so they also survive between tests in the same file).
  resetAll(): void {
    paneCaches.clear();
    panePids.clear();
    paneEpochs.clear();
    pendingByPane.clear();
    pendingReconciledByPane.clear();
  },
};
