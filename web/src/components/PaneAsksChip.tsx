import { useState } from 'react';
import { api } from '../api';
import { useStore } from '../store';
import type { PaneInfo } from '../types';

/** The pane's open asks to the user (daemon asks.go), in the header: the ask
 *  itself when there is one, else how many. Hover lists them; Dismiss is the
 *  person's (the agent resolves its own). Plain text only. */
export function PaneAsksChip({ pane }: { pane: PaneInfo }) {
  const [pending, setPending] = useState(false);
  const asks = pane.asks ?? [];
  if (asks.length === 0) return null;
  const one = asks.length === 1 ? asks[0] : null;
  const dismiss = async () => {
    if (!one || pending) return;
    setPending(true);
    try {
      await api.dismissAsk(one.id);
      await useStore.getState().refreshPanes();
    } catch (err) {
      useStore.getState().toast(`Could not dismiss: ${String(err)}`, { kind: 'warn' });
    } finally { setPending(false); }
  };
  return <span className="pane-unsent-message pane-asks-chip" role="status"
    title={asks.map((a) => `• ${a.text}`).join('\n')}
    onClick={(e) => e.stopPropagation()} onDoubleClick={(e) => e.stopPropagation()}>
    <span className="pane-unsent-message-label">{one ? `Waiting on you: ${one.text}` : `${asks.length} asks waiting on you`}</span>
    {one && <>{' — '}<button type="button" disabled={pending} title="Dismiss this ask" onClick={() => void dismiss()}>Dismiss</button></>}
  </span>;
}
