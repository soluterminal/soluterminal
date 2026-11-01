import { useState } from 'react';
import { api } from '../api';
import { confirmDialog } from '../lib/dialog';
import { useStore } from '../store';
import type { PaneInfo } from '../types';

export function UnsentMessageChip({ pane, actionGuard }: { pane: PaneInfo; actionGuard?: () => boolean }) {
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
    <span className="pane-unsent-message-label">{msg.notTyped ? `Message from ${msg.sender} not sent (the pane was busy)` : `Unsent message from ${msg.sender}`} —{' '}</span>
    <button type="button" disabled={pending} title={msg.notTyped ? 'Read the message, then type it into the pane and press Enter.' : undefined} onClick={() => void act('send')}>{msg.notTyped ? 'Send…' : 'Send'}</button>{' / '}
    {msg.notTyped
      ? <button type="button" disabled={pending} title="Discard the message; nothing was typed." onClick={() => void act('clear')}>Discard</button>
      : <button type="button" disabled={pending} title="Clear only if the complete unchanged message is visible. Hidden pastes must be edited in the pane." onClick={() => void act('clear')}>Clear</button>}
  </span>;
}
