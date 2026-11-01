import { useSyncExternalStore, type ReactNode } from 'react';
import { useStore } from '../store';
import {
  subscribeWorkspaceHandoff,
  workspaceMovedTo,
  workspaceTransferring,
} from '../lib/workspaceHandoff';

export function WorkspaceHandoffBoundary({ children }: { children: ReactNode }) {
  const name = useStore((state) => state.activeWorkspace);
  const owner = useSyncExternalStore(
    subscribeWorkspaceHandoff,
    () => workspaceMovedTo(name),
    () => null,
  );
  const preparing = useSyncExternalStore(
    subscribeWorkspaceHandoff,
    () => workspaceTransferring(name),
    () => false,
  );
  const load = useStore((state) => state.loadWorkspace);
  return (
    // .workspace-handoff is the grid column's flex item (styles.css): it must
    // shrink to leave the footer StatusBar its row.
    <div className="workspace-handoff" style={{ position: 'relative', width: '100%', height: '100%' }}>
      <div
        style={{
          width: '100%',
          height: '100%',
          display: owner ? 'none' : undefined,
        }}
        {...(owner || preparing ? { inert: '' } : {})}
        aria-hidden={!!owner}
      >
        {children}
      </div>
      {preparing && !owner && (
        <div
          role="status"
          style={{
            position: 'absolute',
            inset: 0,
            display: 'grid',
            placeItems: 'center',
            background: 'var(--bg)',
          }}
        >
          Moving workspace…
        </div>
      )}
      {owner && (
        <div
          className="empty-workspace"
          style={{
            height: '100%',
            display: 'flex',
            flexDirection: 'column',
            alignItems: 'center',
            justifyContent: 'center',
            gap: 16,
          }}
        >
          <h2>Workspace moved to another window</h2>
          <p>Your terminals and agents keep running. Your local drafts stay in this window.</p>
          <button className="btn" onClick={() => void load(name, true)}>
            Move here
          </button>
        </div>
      )}
    </div>
  );
}
