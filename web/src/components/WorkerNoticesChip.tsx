import { useState } from 'react';
import { api } from '../api';
import { useStore } from '../store';
import type { PaneInfo } from '../types';

/** Routine lead housekeeping: muted header memo, never an attention signal.
 *  Uses the same durable dismissal IDs as the former worker asks. */
export function WorkerNoticesChip({ pane }: { pane: PaneInfo }) {
  const [pending, setPending] = useState(false);
  const notices = pane.workerNotices ?? [];
  if (notices.length === 0) return null;
  const dismiss = async () => {
    if (pending) return;
    setPending(true);
    try {
      // Only dismiss the notes shown when the person clicked, never new ones.
      const results = await Promise.allSettled(notices.map((note) => api.dismissAsk(note.id)));
      await useStore.getState().refreshPanes();
      if (results.some((r) => r.status === 'rejected')) {
        useStore.getState().toast('Some worker notes could not be dismissed.', { kind: 'warn' });
      }
    } catch (err) {
      useStore.getState().toast(`Could not refresh worker notes: ${String(err)}`, { kind: 'warn' });
    } finally { setPending(false); }
  };
  return <span className="pane-worker-notices" title={notices.map((note) => `• ${note.text}`).join('\n')}
    onClick={(e) => e.stopPropagation()} onDoubleClick={(e) => e.stopPropagation()}>
    <span className="pane-worker-notices-label">{notices.length === 1 ? notices[0].text : `Workers kept open: ${notices.length}`}</span>
    <button type="button" disabled={pending} onClick={() => void dismiss()}>{notices.length === 1 ? 'Dismiss' : 'Dismiss notes'}</button>
  </span>;
}
