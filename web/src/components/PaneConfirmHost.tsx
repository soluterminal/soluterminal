import { reviewPaneConfirm, usePaneConfirms } from '../lib/paneConfirm';
import { IconShield } from './ui/icons';

/** Agents' open requests to close or change a pane they did not start
 *  (lib/paneConfirm.ts). Each stays here until it is answered or expires. */
export function PaneConfirmHost() {
  const pending = usePaneConfirms((s) => s.pending);
  if (pending.length === 0) return null;
  return (
    <div className="pane-confirm-host" role="region" aria-label="Agent requests waiting for your OK">
      {pending.map((p) => (
        <div key={p.id} className="toast toast-warn pane-confirm" role="status">
          <span className="toast-icon" aria-hidden="true"><IconShield size={14} style={{ verticalAlign: '-2px' }} /></span>
          <span className="toast-msg">{p.message}</span>
          <button type="button" className="app-dialog-btn" onClick={() => void reviewPaneConfirm(p.id)} aria-label={`Review: ${p.message}`}>
            Review
          </button>
        </div>
      ))}
    </div>
  );
}
