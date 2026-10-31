// Per-socket input ownership for a terminal pane.
//
// The daemon only writes keystrokes from the socket that owns the pane, and it
// forgets the owner the moment that socket drops (every relayed socket drops
// on a cloud deploy). Ownership therefore belongs to ONE socket: a reconnect
// starts unconfirmed, whatever the page remembers, until the daemon says
// owned:true on this socket. This tracker keeps that per-socket truth and makes
// the claim self-healing:
//   - a claim requested while the pane is not visible is kept and sent on the
//     next visible edge, never silently dropped. It is resumed as the
//     conditional adopt-unowned: by then the user may have taken the pane
//     elsewhere, and a late unconditional takeover must never steal it back;
//   - an unanswered claim is cleared after CLAIM_ACK_TIMEOUT_MS and retried;
//   - keystrokes typed before ownership is confirmed claim (adopt-unowned) and
//     are held up to INPUT_HOLD_MS, then sent once owned — or reported (log +
//     console.warn + the claim banner) instead of vanishing.

export type PaneOwnerState = 'you' | 'other' | 'none' | undefined;
export type PaneClaimReason = 'takeover' | 'first-open' | 'reconnect' | 'adopt-unowned';

export const CLAIM_ACK_TIMEOUT_MS = 3000;
// Cover the entire acknowledgement window, with a fixed deadline from the
// first held byte. Further typing and claim retries never extend it.
export const INPUT_HOLD_MS = CLAIM_ACK_TIMEOUT_MS;
export const CLAIM_MAX_RETRIES = 2;
const KEYSTROKE_CLAIM_INTERVAL_MS = 1000;
const HELD_INPUT_MAX_BYTES = 64 * 1024;

export interface PaneInputOwnershipDeps {
  paneId: string;
  /** Writes a claim frame to this socket. False when the socket can't send. */
  sendClaim: (reason: PaneClaimReason) => boolean;
  /** Delivers held bytes once owned (through the pane's normal input gates).
   *  False when the pane can no longer take them (hidden, socket closing). */
  sendInput: (bytes: Uint8Array) => boolean;
  /** This pane is on screen and allowed to act (Pane's currentVisible()). */
  visible: () => boolean;
  /** Typing was not delivered and the daemon did not grant input: show the
   *  existing claim banner ("Type here"). */
  onInputUnclaimed: (owner: PaneOwnerState) => void;
  /** Lifecycle trail (debug.ts lifecycleLog). */
  log: (name: string, detail: Record<string, unknown>) => void;
  warn?: (message: string, detail: Record<string, unknown>) => void;
  now?: () => number;
  setTimer?: (fn: () => void, ms: number) => unknown;
  clearTimer?: (handle: unknown) => void;
}

export interface PaneInputOwnership {
  /** owned:true seen on THIS socket and not revoked since. */
  readonly confirmed: boolean;
  /** A claim was sent on this socket and is not yet answered. */
  readonly claimPending: boolean;
  /** Last owner state the daemon reported on this socket. */
  readonly owner: PaneOwnerState;
  /** Ask for ownership: sent now when visible, else kept for the next visible edge. */
  claim: (reason: PaneClaimReason) => 'sent' | 'deferred' | 'failed';
  /** Call on any visible edge (visibility frame, reattach, ping): sends a kept claim. */
  onVisible: () => void;
  /** Feed every {type:"ownership"} frame received on this socket. */
  onOwnership: (owned: boolean, owner: PaneOwnerState) => void;
  /** Bytes to send while NOT confirmed. `user` marks input a person produced
   *  (typing, keybar) as opposed to programmatic nudges (Ctrl+L repaint).
   *  A user keypress claims (adopt-unowned) and is held; other bytes are held
   *  only while a claim is already in flight. Emulator replies are always
   *  dropped while unconfirmed, never queued. Returns true when held. */
  holdInput: (bytes: Uint8Array, user: boolean) => boolean;
  /** Socket closed: drops timers; held input is reported as dropped. */
  close: () => void;
}

// Sequences a terminal emulator sends on its own (xterm answering a query a
// background tile received), as opposed to a key a person pressed: OSC/DCS/
// APC/PM strings, focus and mouse reports, and CSI query answers (DA, CPR,
// DSR, DECRPM, window reports). Arrow/function keys, Alt combinations and
// bracketed pastes (ESC[200~) are user input. Mirrors daemon isTerminalReply.
export function isTerminalReply(bytes: Uint8Array): boolean {
  if (bytes.length < 2 || bytes[0] !== 0x1b) return false;
  const kind = bytes[1];
  if (kind === 0x5d || kind === 0x50 || kind === 0x5f || kind === 0x5e) return true; // ] P _ ^
  if (kind !== 0x5b) return false; // not CSI: Alt+key, SS3 arrows
  if (bytes.length >= 3 && (bytes[2] === 0x4d || bytes[2] === 0x3c)) return true; // mouse: M or <
  if (bytes.length === 3 && (bytes[2] === 0x49 || bytes[2] === 0x4f)) return true; // focus: I or O
  for (let i = 2; i < bytes.length; i++) {
    const b = bytes[i];
    if (b < 0x40 || b > 0x7e) continue;
    if (b === 0x63) return bytes[2] === 0x3f || bytes[2] === 0x3e || bytes[2] === 0x3d; // DA answer: c with ? > =
    if (b === 0x52 || b === 0x6e || b === 0x74) return true; // R n t
    if (b === 0x79) return i > 2 && bytes[i - 1] === 0x24; // DECRPM: $y
    return false;
  }
  return false;
}

// A real keypress (anything a person typed or pasted), not terminal machinery.
export function isKeypressInput(bytes: Uint8Array): boolean {
  return bytes.length > 0 && !isTerminalReply(bytes);
}

export function createPaneInputOwnership(deps: PaneInputOwnershipDeps): PaneInputOwnership {
  const now = deps.now ?? (() => Date.now());
  const setTimer = deps.setTimer ?? ((fn: () => void, ms: number) => window.setTimeout(fn, ms));
  const clearTimer = deps.clearTimer ?? ((h: unknown) => window.clearTimeout(h as number));
  const warn = deps.warn ?? ((message: string, detail: Record<string, unknown>) => {
    // eslint-disable-next-line no-console
    console.warn(message, detail);
  });

  let confirmed = false;
  let owner: PaneOwnerState;
  let closed = false;
  let pending: { reason: PaneClaimReason; retries: number; timer: unknown } | null = null;
  let deferred: PaneClaimReason | null = null;
  let held: Uint8Array[] = [];
  let heldBytes = 0;
  let heldUserInput = false;
  let holdTimer: unknown = null;
  let lastKeystrokeClaimAt = Number.NEGATIVE_INFINITY;
  let lastQuietDropLogAt = Number.NEGATIVE_INFINITY;

  const clearPending = () => {
    if (pending) clearTimer(pending.timer);
    pending = null;
  };

  const reportDropped = (reason: string, bytes: number, banner: boolean) => {
    const detail = { paneId: deps.paneId, reason, bytes, owner: owner ?? null, at: new Date(now()).toISOString() };
    deps.log('pane.input.dropped', detail);
    warn('[solu] input dropped', detail);
    if (banner) deps.onInputUnclaimed(owner);
  };

  const dropHeld = (reason: string, banner: boolean) => {
    if (holdTimer !== null) clearTimer(holdTimer);
    holdTimer = null;
    const bytes = heldBytes;
    const user = heldUserInput;
    held = [];
    heldBytes = 0;
    heldUserInput = false;
    if (bytes > 0) reportDropped(reason, bytes, banner && user);
  };

  const send = (reason: PaneClaimReason, retries: number): 'sent' | 'deferred' | 'failed' => {
    if (closed) return 'failed';
    if (!deps.visible()) {
      if (!deferred) deps.log('pane.ownership.claim-deferred', { paneId: deps.paneId, reason });
      deferred = reason;
      return 'deferred';
    }
    if (!deps.sendClaim(reason)) return 'failed';
    clearPending();
    const entry = { reason, retries, timer: null as unknown };
    entry.timer = setTimer(() => {
      if (pending !== entry) return;
      pending = null;
      if (closed || confirmed) return;
      deps.log('pane.ownership.claim-timeout', { paneId: deps.paneId, reason, retries, owner: owner ?? null });
      if (retries < CLAIM_MAX_RETRIES && owner !== 'other') send(reason, retries + 1);
    }, CLAIM_ACK_TIMEOUT_MS);
    pending = entry;
    return 'sent';
  };

  return {
    get confirmed() { return confirmed; },
    get claimPending() { return pending !== null; },
    get owner() { return owner; },
    claim: (reason) => send(reason, 0),
    onVisible: () => {
      if (closed || !deferred || confirmed || !deps.visible()) return;
      const kept = deferred;
      deferred = null;
      deps.log('pane.ownership.claim-resumed', { paneId: deps.paneId, requested: kept, reason: 'adopt-unowned' });
      send('adopt-unowned', 0);
    },
    onOwnership: (owned, state) => {
      if (closed) return;
      const was = confirmed;
      confirmed = owned;
      owner = owned ? 'you' : state;
      if (owned) {
        clearPending();
        deferred = null;
        if (holdTimer !== null) clearTimer(holdTimer);
        holdTimer = null;
        const flush = held;
        const bytes = heldBytes;
        held = [];
        heldBytes = 0;
        heldUserInput = false;
        if (!was) deps.log('pane.ownership.confirmed', { paneId: deps.paneId, heldBytes: bytes });
        if (bytes === 0) return;
        // The pane may have been hidden or parked while the claim was in
        // flight: then the typing can't be delivered, and that is reported,
        // never silently swallowed (nor replayed later into a pane the user
        // has since left).
        let refused = 0;
        const visible = deps.visible();
        if (!visible) {
          refused = bytes;
        } else {
          for (const chunk of flush) {
            if (refused > 0 || !deps.sendInput(chunk)) refused += chunk.length;
          }
        }
        if (refused > 0) reportDropped(visible ? 'delivery-refused' : 'hidden-before-grant', refused, false);
        if (refused < bytes) deps.log('pane.input.reclaimed', { paneId: deps.paneId, bytes: bytes - refused });
        return;
      }
      if (state === 'other') {
        // A peer really holds input (a denial ack or a takeover): our claim
        // is answered, and held typing can't be delivered — say so.
        clearPending();
        dropHeld('owned-by-other', true);
      }
    },
    holdInput: (bytes, user) => {
      if (closed || bytes.length === 0 || isTerminalReply(bytes)) return false;
      const keypress = user && isKeypressInput(bytes);
      if (owner === 'other' && pending === null) {
        // A peer holds input and no claim of ours is in flight: holding
        // can't help. Report it and show the claim banner right away.
        if (keypress) reportDropped('owned-by-other', bytes.length, true);
        return false;
      }
      if (!keypress && pending === null && heldBytes === 0) {
        // Terminal machinery or a programmatic nudge with no claim in
        // flight: nobody is typing, so never claim or raise a banner for
        // it (the daemon would drop it anyway).
        const t = now();
        if (t - lastQuietDropLogAt >= 5000) {
          lastQuietDropLogAt = t;
          deps.log('pane.input.dropped', { paneId: deps.paneId, reason: 'not-owner-unattended', bytes: bytes.length });
        }
        return false;
      }
      if (heldBytes + bytes.length > HELD_INPUT_MAX_BYTES) {
        reportDropped('hold-overflow', bytes.length, false);
        return false;
      }
      held.push(bytes.slice());
      heldBytes += bytes.length;
      if (keypress) heldUserInput = true;
      if (holdTimer === null) {
        holdTimer = setTimer(() => {
          holdTimer = null;
          if (closed || confirmed) return;
          dropHeld(pending ? 'claim-pending' : 'not-owner', true);
        }, INPUT_HOLD_MS);
      }
      if (keypress && pending === null) {
        const t = now();
        if (t - lastKeystrokeClaimAt >= KEYSTROKE_CLAIM_INTERVAL_MS) {
          lastKeystrokeClaimAt = t;
          deps.log('pane.ownership.claim-on-input', { paneId: deps.paneId, owner: owner ?? null });
          send('adopt-unowned', CLAIM_MAX_RETRIES);
        }
      }
      return true;
    },
    close: () => {
      if (closed) return;
      clearPending();
      deferred = null;
      dropHeld('closing', false);
      closed = true;
    },
  };
}
