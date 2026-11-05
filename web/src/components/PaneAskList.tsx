import { useState } from 'react';
import { api } from '../api';
import { useStore } from '../store';
import type { PaneAsk, PaneInfo } from '../types';
import { formatAgo } from '../lib/turnTime';

/** Dismiss one of the pane's asks, the same call the bell's × makes. */
export function useDismissAsk() {
  const [pendingId, setPendingId] = useState('');
  const dismiss = async (id: string) => {
    if (pendingId) return;
    setPendingId(id);
    try {
      await api.dismissAsk(id);
      await useStore.getState().refreshPanes();
    } catch (err) {
      useStore.getState().toast(`Could not dismiss: ${String(err)}`, { kind: 'warn' });
    } finally { setPendingId(''); }
  };
  return { pendingId, dismiss };
}

/** Every open ask of a pane with its full text, in a header card. Same actions
 *  as the bell's rows: Dismiss, and open the pane. Text is plain text, never
 *  markdown or links. */
export function PaneAskList({ pane, asks }: { pane: PaneInfo; asks: PaneAsk[] }) {
  const { pendingId, dismiss } = useDismissAsk();
  return <ul className="pane-ask-list">
    {asks.map((a) => <li key={a.id} className="pane-ask-item">
      <div className="pane-ask-meta">{a.source} · {formatAgo(a.createdAt)}</div>
      <div className="pane-ask-text">{a.text}</div>
      <div className="pane-ask-actions">
        <button type="button" disabled={!!pendingId} title="Dismiss this ask" aria-label={`Dismiss ask from ${a.source}`}
          onClick={() => void dismiss(a.id)}>Dismiss</button>
        <button type="button" title="Go to this pane" onClick={() => void useStore.getState().revealPane(pane.id)}>Open pane</button>
      </div>
    </li>)}
  </ul>;
}
