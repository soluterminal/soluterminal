import { useState } from 'react';
import { api } from '../api';
import { confirmDialog } from '../lib/dialog';
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
  return <span className="pane-unsent-message" role="status" title={msg.text} onClick={(e) => e.stopPropagation()} onDoubleClick={(e) => e.stopPropagation()}>
    <span className="pane-unsent-message-label">{`Message from ${msg.sender} ${behindUnsent(pane) ? 'waits behind the unsent message' : 'waits while you type'}${more}`} —{' '}</span>
    {/* Behind an unsent message in the box, Send now could only be refused. */}
    {!behindUnsent(pane) && <><button type="button" disabled={pending} title="Type it now. Your input box must be empty." onClick={() => void act('send')}>Send now</button>{' / '}</>}
    <button type="button" disabled={pending} title="Drop the message; nothing is typed." onClick={() => void act('clear')}>Discard</button>
  </span>;
}

/** Held messages wait behind an agent's unsent message in the input box. */
function behindUnsent(pane: PaneInfo): boolean {
  return !!pane.unsentMessage && !pane.unsentMessage.notTyped;
}

function ReceiptChip({ pane, actionGuard }: { pane: PaneInfo; actionGuard?: () => boolean }) {
  const [pending, setPending] = useState(false);
  const msg = pane.unsentMessage;
  if (!msg) return null;
  const act = async (action: 'send' | 'clear') => {
    if (pending || actionGuard?.() === false) return;
    setPending(true);
    try {
      // A message the pane was never given is an agent's text: the person
      // reads all of it before Send types it.
      if (action === 'send' && msg.notTyped) {
        if (!msg.text) {
          useStore.getState().toast('The message text is not available; ask the sender to send it again.', { kind: 'warn' });
          return;
        }
        const ok = await confirmDialog({
          title: `Send ${msg.sender}'s message?`,
          message: `This is typed into the pane, then Enter is pressed:\n\n${msg.text}`,
          confirmLabel: 'Type it and press Enter',
        });
        if (!ok) return;
      }
      const result = await api.actOnUnsentMessage(pane.id, msg.id, action);
      await useStore.getState().refreshPanes();
      if (result.outcome !== 'submitted' && result.outcome !== 'cleared') {
        useStore.getState().toast(result.outcome === 'left_in_composer' ? 'The message is still in the input box.' : 'Could not confirm the input box changed.', { kind: 'warn' });
      }
    } catch (err) {
      useStore.getState().toast(String(err), { kind: 'warn' });
    } finally { setPending(false); }
  };
  return <span className="pane-unsent-message" role="status" title={msg.notTyped ? msg.text : undefined} onClick={(e) => e.stopPropagation()} onDoubleClick={(e) => e.stopPropagation()}>
    <span className="pane-unsent-message-label">{msg.notTyped ? `Message from ${msg.sender} not sent (the pane was busy)` : msg.putBack ? `Unsent message from ${msg.sender} in the input box (put back after you stopped it)` : `Unsent message from ${msg.sender}`} —{' '}</span>
    <button type="button" disabled={pending} title={msg.notTyped ? 'Read the message, then type it into the pane and press Enter.' : undefined} onClick={() => void act('send')}>{msg.notTyped ? 'Send…' : 'Send'}</button>{' / '}
    {msg.notTyped
      ? <button type="button" disabled={pending} title="Discard the message; nothing was typed." onClick={() => void act('clear')}>Discard</button>
      : <button type="button" disabled={pending} title="Clear only if the complete unchanged message is visible. Hidden pastes must be edited in the pane." onClick={() => void act('clear')}>Clear</button>}
  </span>;
}
