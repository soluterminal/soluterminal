import { create } from 'zustand';

// What became of the last "Type here" click (SessionClaimOverlay). A claim the
// daemon refuses used to look exactly like no click at all: the ack is the
// same owned:false frame the chip was already showing. The daemon now names
// the refusal (daemon pane_subscribe.go claimDenied*), and this keeps the
// click's outcome so the chip can say why and what to do next. It only
// reports: who may type stays the daemon's decision.

/** The daemon's refusal reasons, plus the two the page detects itself. */
export type ClaimRefusal =
  | 'not-person' // the socket isn't the person's (an unapproved screen)
  | 'view-only' // the socket may only view this computer's panes
  | 'owner-active' // another device holds the pane and is in use
  | 'regrabbed' // granted, then another device took it straight back
  | 'unreachable'; // the claim never reached the daemon, or got no answer

export type ClaimFeedback =
  | { state: 'idle' }
  | { state: 'pending' }
  | { state: 'refused'; reason: ClaimRefusal };

// Covers the claim tracker's whole retry run (paneInputOwnership:
// CLAIM_ACK_TIMEOUT_MS × (1 + CLAIM_MAX_RETRIES)) with a little slack.
export const CLAIM_FEEDBACK_TIMEOUT_MS = 10_000;
// Losing the pane this soon after a Type here was granted reads as a re-grab.
export const CLAIM_REGRAB_WINDOW_MS = 10_000;

// When panes refuse for different reasons, the chip shows the one the person
// can act on first: approving this screen fixes the most.
const PRIORITY: ClaimRefusal[] = ['view-only', 'not-person', 'owner-active', 'regrabbed', 'unreachable'];

/** The daemon's "denied" field; anything else is ignored. */
export function parseClaimDenied(v: unknown): ClaimRefusal | null {
  return v === 'not-person' || v === 'view-only' || v === 'owner-active' ? v : null;
}

export const useClaimFeedback = create<ClaimFeedback>(() => ({ state: 'idle' }));

let timer: ReturnType<typeof setTimeout> | null = null;
// Each click is one attempt. A pane reports against the attempt it claimed
// for, so a late answer from an earlier click, or from a workspace no longer
// in view, never lands on the chip now showing.
let attempt = 0;

/** The attempt a pane's claim belongs to (taken when it sends the claim). */
export function claimAttempt(): number {
  return attempt;
}

function clearTimer(): void {
  if (timer !== null) clearTimeout(timer);
  timer = null;
}

/** A Type here click: pending until a pane reports a refusal or the chip goes. */
export function beginTypeHere(): void {
  clearTimer();
  attempt++;
  useClaimFeedback.setState({ state: 'pending' }, true);
  timer = setTimeout(() => {
    timer = null;
    if (useClaimFeedback.getState().state === 'pending') {
      useClaimFeedback.setState({ state: 'refused', reason: 'unreachable' }, true);
    }
  }, CLAIM_FEEDBACK_TIMEOUT_MS);
}

/** A pane's claim from the last click was refused. Ignored with no click,
 *  except a re-grab: the chip had gone with the grant, and comes back. `from`
 *  is the attempt the claim was sent for; one that has ended is ignored. */
export function reportClaimRefused(reason: ClaimRefusal, from?: number): void {
  if (from !== undefined && from !== attempt) return;
  const cur = useClaimFeedback.getState();
  if (cur.state === 'idle' && reason !== 'regrabbed') return;
  if (cur.state === 'refused' && PRIORITY.indexOf(cur.reason) <= PRIORITY.indexOf(reason)) return;
  clearTimer();
  useClaimFeedback.setState({ state: 'refused', reason }, true);
}

/** The chip went away (every pane is typeable here: a re-grab may still
 *  report), or, with endAttempt, the view changed and the click is over. */
export function clearClaimFeedback(endAttempt = false): void {
  clearTimer();
  if (endAttempt) attempt++;
  if (useClaimFeedback.getState().state !== 'idle') useClaimFeedback.setState({ state: 'idle' }, true);
}
