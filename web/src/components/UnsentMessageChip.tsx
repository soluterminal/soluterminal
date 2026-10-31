import { useState } from 'react';
import { api } from '../api';
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
      const result = await api.actOnUnsentMessage(pane.id, msg.id, action);
      await useStore.getState().refreshPanes();
      if (result.outcome !== 'submitted' && result.outcome !== 'cleared') {
        useStore.getState().toast(result.outcome === 'left_in_composer' ? 'The message is still in the input box.' : 'Could not confirm the input box changed.', { kind: 'warn' });
      }
    } catch (err) {
      useStore.getState().toast(String(err), { kind: 'warn' });
    } finally { setPending(false); }
  };
  return <span className="pane-unsent-message" role="status" onClick={(e) => e.stopPropagation()} onDoubleClick={(e) => e.stopPropagation()}>
    <span className="pane-unsent-message-label">Unsent message from {msg.sender} —{' '}</span>
    <button type="button" disabled={pending} onClick={() => void act('send')}>Send</button>{' / '}
    <button type="button" disabled={pending} title="Clear only if the complete unchanged message is visible. Hidden pastes must be edited in the pane." onClick={() => void act('clear')}>Clear</button>
  </span>;
}
