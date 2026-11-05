import { useEffect, useState } from 'react';
import { currentRoutingScope } from '../api';
import { isEmbedded } from '../embed';
import { beginTypeHere, clearClaimFeedback, useClaimFeedback, type ClaimRefusal } from '../lib/claimFeedback';
import { terminalSessionKey, effectiveVisiblePaneIds, useStore } from '../store';
import { openApproveThisScreenPrompt } from './ApproveThisScreenPrompt';

/** This screen, as the person knows it. */
function thisScreen(): string {
  return isEmbedded() ? 'This Shinoque Code window' : 'This screen';
}

/** What a refused Type here says, and the one next step it offers. */
export function claimRefusalCopy(reason: ClaimRefusal): { text: string; action: 'approve' | 'retry' } {
  switch (reason) {
    case 'not-person':
      return { text: `${thisScreen()} isn't approved yet, so it can't take typing from another device.`, action: 'approve' };
    case 'view-only':
      return { text: `${thisScreen()} can only view this computer's panes until it's approved.`, action: 'approve' };
    case 'owner-active':
      return { text: 'Another device is typing now. Ask it to stop, or try again.', action: 'retry' };
    case 'regrabbed':
      return { text: 'Another device took typing back. Ask it to stop, then try again.', action: 'retry' };
    case 'unreachable':
      return { text: "Couldn't reach this pane. Check the connection, then try again.", action: 'retry' };
  }
}

// Where to approve when the prompt can't open here (a remote machine in view,
// or this screen's standing isn't known yet).
const APPROVE_ELSEWHERE =
  'Approve it in Settings → Privacy & security → Remote devices on a browser or app you’ve already approved.';

// Single session-level claim chip. Renders once over the grid when ANY
// pane reports owned=false, so the user sees one unified "take over"
// control rather than one-per-pane. Deliberately NOT a blocking modal:
// watching another device drive is a legitimate long-lived state, and
// each locked pane already blocks its own pointer/keys via
// .pane-state-input-locked — a full-screen scrim added nothing but got
// in the way of reading the streams. One tap fires claim on every pane
// this browser has connected.
export function SessionClaimOverlay() {
  const claimAllPanes = useStore((s) => s.claimAllPanes);
  // Only consider panes in the CURRENT layout. With warm-pane keep-alive,
  // parked panes from other workspaces keep their paneOwnership entries
  // alive — a phone taking over a backgrounded pane would otherwise throw
  // the "Session in use on another device" modal over the workspace the
  // user is actually looking at, where nothing changed hands. Boolean
  // selector keeps the subscription referentially stable.
  const anyUnowned = useStore((s) => {
    for (const id of effectiveVisiblePaneIds(s)) {
      if (s.paneOwnership[terminalSessionKey(currentRoutingScope(),id)] === false) return true;
    }
    return false;
  });
  // Truthful chip copy: if at least one locked pane is genuinely held by a
  // peer device, keep the "another device" wording; when every locked pane
  // is merely UNOWNED (daemon restarted / controller disconnected — the
  // owner:"none" provenance), saying "input is on another device" is false
  // and sends the user hunting for a phantom phone. Boolean selector keeps
  // the subscription referentially stable.
  const anyHeldByPeer = useStore((s) => {
    for (const id of effectiveVisiblePaneIds(s)) {
      if (s.paneOwnership[terminalSessionKey(currentRoutingScope(),id)] === false && s.paneOwnerState[terminalSessionKey(currentRoutingScope(),id)] !== 'none') return true;
    }
    return false;
  });
  // A click's outcome. Every Type here ends either with the chip gone (typing
  // is here now) or with the reason it was refused and the next step.
  const feedback = useClaimFeedback();
  // The prompt couldn't open here: say where to approve instead.
  const [approveElsewhere, setApproveElsewhere] = useState(false);
  useEffect(() => setApproveElsewhere(false), [feedback]);
  // The click belongs to the view it was made in: another workspace or
  // backend in view ends it, so its outcome never shows over unrelated panes.
  const viewKey = useStore((s) => `${currentRoutingScope().key}|${effectiveVisiblePaneIds(s).join(',')}`);
  useEffect(() => () => clearClaimFeedback(true), [viewKey]);
  useEffect(() => {
    if (!anyUnowned) clearClaimFeedback();
  }, [anyUnowned]);
  if (!anyUnowned) return null;
  const typeHere = () => {
    beginTypeHere();
    claimAllPanes();
  };
  const refused = feedback.state === 'refused' ? claimRefusalCopy(feedback.reason) : null;
  return (
    <div className="claim-chip" role="status" aria-live="polite">
      <span className="claim-chip-dot" aria-hidden="true" />
      <span className="claim-chip-text">
        {refused
          ? approveElsewhere ? `${refused.text} ${APPROVE_ELSEWHERE}` : refused.text
          : anyHeldByPeer
            ? 'Typing is on another device. You can watch here.'
            : 'Typing is paused on this screen.'}
      </span>
      {refused?.action === 'approve' && !approveElsewhere && (
        <button
          type="button"
          className="claim-chip-btn"
          title="Ask for this screen to be approved"
          onClick={() => {
            if (!openApproveThisScreenPrompt()) setApproveElsewhere(true);
          }}
        >Approve this screen</button>
      )}
      {/* Once approved (or the other device stops), the same click works. */}
      <button
        type="button"
        className="claim-chip-btn"
        title="Send your keystrokes from this screen"
        disabled={feedback.state === 'pending'}
        onClick={typeHere}
      >{feedback.state === 'pending' ? 'Taking over…' : refused ? 'Try again' : 'Type here'}</button>
    </div>
  );
}

