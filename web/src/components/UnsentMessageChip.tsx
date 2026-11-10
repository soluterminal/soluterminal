import { useRef, useState } from 'react';
import { api, ApiError } from '../api';
import { confirmDialog } from '../lib/dialog';
import { canHashText, textSha256 } from '../lib/textSha256';
import { useStore } from '../store';
import type { PaneInfo } from '../types';

export function UnsentMessageChip({ pane, actionGuard }: { pane: PaneInfo; actionGuard?: () => boolean }) {
  return <>
    <ReceiptChip pane={pane} actionGuard={actionGuard} />
    <HeldMessageChip pane={pane} actionGuard={actionGuard} />
  </>;
}

/** An agent's message the daemon holds while the person types in the pane.
 *  It goes in by itself once the input box is empty and they stop typing;
 *  Send now types it at once (never into text in the box), Discard drops it. */
function HeldMessageChip({ pane, actionGuard }: { pane: PaneInfo; actionGuard?: () => boolean }) {
  const [pending, setPending] = useState(false);
  const held = pane.heldMessages ?? [];
  const msg = held[0];
  if (!msg) return null;
  const act = async (action: 'send' | 'clear') => {
    if (pending || actionGuard?.() === false) return;
    setPending(true);
    try {
      const result = await api.actOnUnsentMessage(pane.id, msg.id, action);
      await useStore.getState().refreshPanes();
      if (result.outcome === 'left_in_composer' || result.outcome === 'sent_unconfirmed') {
        useStore.getState().toast(result.outcome === 'left_in_composer' ? 'The message is still in the input box.' : 'Could not confirm the input box changed.', { kind: 'warn' });
      }
    } catch (err) {
      useStore.getState().toast(String(err), { kind: 'warn' });
    } finally { setPending(false); }
  };
  const more = held.length > 1 ? ` (+${held.length - 1} more)` : '';
  // The box has text and nobody is typing: "while you type" would be wrong,
  // and Send now could only be refused.
  const blocked = behindUnsent(pane) || !!pane.heldStuck;
  const where = behindUnsent(pane) ? 'waits behind the unsent message' : pane.heldStuck ? 'waits behind the text in the input box' : 'waits while you type';
  return <span className="pane-unsent-message" role="status" title={msg.text} onClick={(e) => e.stopPropagation()} onDoubleClick={(e) => e.stopPropagation()}>
    <span className="pane-unsent-message-label">{`Message from ${msg.sender} ${where}${more}`} —{' '}</span>
    {!blocked && <><button type="button" disabled={pending} title="Type it now. Your input box must be empty." onClick={() => void act('send')}>Send now</button>{' / '}</>}
    <button type="button" disabled={pending} title="Drop the message; nothing is typed." onClick={() => void act('clear')}>Discard</button>
  </span>;
}

/** Held messages wait behind an agent's unsent message in the input box. */
function behindUnsent(pane: PaneInfo): boolean {
  return !!pane.unsentMessage && !pane.unsentMessage.notTyped;
}

/** The daemon's refusal of a Send whose text the person did not review:
 *  its message, or null for any other error. */
function reviewRefusal(err: unknown): string | null {
  if (!(err instanceof ApiError) || err.status !== 409) return null;
  const body = err.body as { code?: unknown; error?: unknown } | undefined;
  if (body?.code !== 'unsent_text_changed' && body?.code !== 'unsent_text_unconfirmed') return null;
  return typeof body.error === 'string' ? body.error : "The message changed or wasn't shown; review it again";
}

function ReceiptChip({ pane, actionGuard }: { pane: PaneInfo; actionGuard?: () => boolean }) {
  const [pending, setPending] = useState(false);
  // "Review again" opens Send… on the receipt as it is after the refresh.
  const reviewAgain = useRef<(() => void) | null>(null);
  const msg = pane.unsentMessage;
  reviewAgain.current = null;
  if (!msg) return null;
  const act = async (action: 'send' | 'clear' | 'keep') => {
    if (pending || actionGuard?.() === false) return;
    setPending(true);
    try {
      // A message the pane was never given is an agent's text: the person
      // reads all of it before Send types it, and the daemon types it only
      // if it is still exactly the text shown (its hash goes with Send).
      let shownSha256: string | undefined;
      if (action === 'send' && msg.notTyped) {
        const shown = msg.text;
        if (!shown) {
          useStore.getState().toast('The message text is not available; ask the sender to send it again.', { kind: 'warn' });
          return;
        }
        if (!canHashText()) {
          useStore.getState().toast('This message can only be sent from a secure page. Open SoluTerminal over https or on this computer, then send it there.', { kind: 'warn' });
          return;
        }
        const ok = await confirmDialog({
          title: `Send ${msg.sender}'s message?`,
          message: `This is typed into the pane, then Enter is pressed:\n\n${shown}`,
          confirmLabel: 'Type it and press Enter',
        });
        if (!ok) return;
        shownSha256 = await textSha256(shown);
      }
      const result = await (shownSha256 ? api.actOnUnsentMessage(pane.id, msg.id, action, shownSha256) : api.actOnUnsentMessage(pane.id, msg.id, action));
      await useStore.getState().refreshPanes();
      if (result.outcome !== 'submitted' && result.outcome !== 'cleared' && result.outcome !== 'kept') {
        useStore.getState().toast(result.outcome === 'partly_cleared' ? 'Partly cleared: text may remain in the input box. Agents are still blocked; check the pane before acting again.' : result.outcome === 'left_in_composer' ? 'The message is still in the input box.' : 'Could not confirm the input box changed.', { kind: 'warn' });
      }
    } catch (err) {
      const refusal = reviewRefusal(err);
      if (refusal) {
        await useStore.getState().refreshPanes().catch(() => undefined);
        useStore.getState().toast(refusal, { kind: 'warn', action: { label: 'Review again', run: () => reviewAgain.current?.() } });
        return;
      }
      useStore.getState().toast(String(err), { kind: 'warn' });
    } finally { setPending(false); }
  };
  if (msg.notTyped) reviewAgain.current = () => void act('send');
  // An agent's message put back in the box blocks agents until it is gone;
  // once the person edits it, Send would send their edit too, so they Clear
  // it or keep it as their own draft.
  if (msg.putBack && msg.edited) {
    return <span className="pane-unsent-message" role="status" onClick={(e) => e.stopPropagation()} onDoubleClick={(e) => e.stopPropagation()}>
      <span className="pane-unsent-message-label">{`${msg.sender}'s unsent message is still in the input box after your edit. Agents can't type into this pane until it's gone`} —{' '}</span>
      <button type="button" disabled={pending} title="Clear the visible input, your edit too. Wrapped and multi-line text requires a supported agent version and a completely readable box. Any remainder stays guarded." onClick={() => void act('clear')}>Clear</button>{' / '}
      <button type="button" disabled={pending} title="Hide this and use your own message box again. Agents still can't type into this pane until the text is gone." onClick={() => void act('keep')}>Keep as my draft</button>
    </span>;
  }
  return <span className="pane-unsent-message" role="status" title={msg.notTyped ? msg.text : undefined} onClick={(e) => e.stopPropagation()} onDoubleClick={(e) => e.stopPropagation()}>
    <span className="pane-unsent-message-label">{msg.notTyped ? `Message from ${msg.sender} not sent (the pane was busy)` : msg.putBack ? `Unsent message from ${msg.sender} is back in the input box. Agents can't type into this pane until it's sent or cleared` : `Unsent message from ${msg.sender}`} —{' '}</span>
    <button type="button" disabled={pending} title={msg.notTyped ? 'Read the message, then type it into the pane and press Enter.' : undefined} onClick={() => void act('send')}>{msg.notTyped ? 'Send…' : 'Send'}</button>{' / '}
    {msg.notTyped
      ? <button type="button" disabled={pending} title="Discard the message; nothing was typed." onClick={() => void act('clear')}>Discard</button>
      : <button type="button" disabled={pending} title="Clear only if the complete unchanged message is visible. Hidden pastes must be edited in the pane." onClick={() => void act('clear')}>Clear</button>}
  </span>;
}
