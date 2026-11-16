import { useState } from 'react';
import { api, currentRoutingScope } from '../api';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { confirmClosePane } from './usePaneContextMenu';

// Focus (the native renderer) was removed from the web app. A daemon that
// still runs a Focus pane moves it back to a terminal on its own once its
// turn settles, but a turn that is waiting on an approval or a question never
// settles, and its answering UI is gone. This card is that pane's way out:
// "Open as a terminal" switches it now (interrupting the wait) and resumes the
// same conversation in a terminal pane.
export function RetiredFocusPane({ pane }: { pane: PaneInfo }) {
  const killPane = useStore((s) => s.killPane);
  const toast = useStore((s) => s.toast);
  const [switching, setSwitching] = useState(false);

  const openAsTerminal = async () => {
    setSwitching(true);
    const scope = currentRoutingScope();
    try {
      const saved = await api.switchPaneMode(pane.id, '', { when: 'now' }, scope);
      if (scope.key === currentRoutingScope().key) {
        useStore.setState((s) => (s.panes[pane.id] ? { panes: { ...s.panes, [pane.id]: saved } } : {}));
      }
    } catch (e) {
      toast(`Couldn't open it as a terminal: ${(e as Error).message || 'try again'}`, { kind: 'error', icon: '⚠', ttlMs: 5000 });
    } finally {
      setSwitching(false);
    }
  };

  return (
    <div className="pane">
      <div className="pane-header">
        <span className="pane-title">{pane.title || 'Focus pane'}</span>
      </div>
      <div className="pane-recovery">
        <strong className="pane-recovery-title">This pane uses Focus, which has been removed</strong>
        <p>
          It moves to a terminal pane by itself when its current turn finishes. Opening it as a
          terminal now stops whatever it is doing, including a question or approval it is waiting
          on, and resumes the same conversation.
        </p>
        <div className="pane-recovery-actions">
          <button className="primary" disabled={switching} onClick={() => void openAsTerminal()}>
            {switching ? 'Opening…' : 'Open as a terminal'}
          </button>
          <button className="secondary" disabled={switching} onClick={async () => { if (await confirmClosePane(pane)) void killPane(pane.id); }}>
            Close
          </button>
        </div>
      </div>
    </div>
  );
}
