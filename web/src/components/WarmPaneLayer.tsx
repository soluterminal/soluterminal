import { useLayoutEffect } from 'react';
import { createPortal } from 'react-dom';
import { effectiveVisiblePaneIds, useStore, warmPanesActive } from '../store';
import { currentRoutingScope, routingPrincipalAllowed } from '../api';
import { acquirePaneHost } from '../lib/paneHosts';
import { ensureTerminalSession, registerTerminalPortal, forgetTerminalSession, reconcileTerminalSessions, useTerminalSessions, type TerminalViewSession } from '../lib/terminalViewSessions';
import { paneIdsInOrder } from '../lib/layout';
import { ErrorBoundary } from './ErrorBoundary';
import { Pane } from './Pane';
import { PaneRecovery } from './PaneRecovery';

function TerminalPortal({ session }: { session: TerminalViewSession }) {
  useLayoutEffect(() => registerTerminalPortal(session.key), [session.key]);
  const layout = useStore((s) => s.layout);
  // Match Alt+1…9; panes outside this workspace keep zero (no badge).
  const paneIndex = paneIdsInOrder(layout).indexOf(session.pane.id) + 1;
  return createPortal(
    <ErrorBoundary label={`Pane ${session.pane.id}`}>
      {session.pane.recoveryError ? (
        <PaneRecovery pane={session.pane} />
      ) : (
        <Pane
          key={session.processIdentity}
          pane={session.pane}
          paneIndex={paneIndex}
          session={session}
        />
      )}
    </ErrorBoundary>,
    acquirePaneHost(session.key),
  );
}
// Sole terminal component owner on every layout. NativePane remains in the
// active grid; unvisited task rows and historical attempts mount no terminal.
export function WarmPaneLayer() {
 const panes = useStore(s=>s.panes);
 const view = useStore(s=>s.mainPresentation);
 const layout = useStore(s=>s.layout);
 const workspaceView = useStore(s=>s.workspaceView);
 const focus = useStore(s=>s.focusedPaneId);
 const zoom = useStore(s=>s.zoomedPaneId);
 const parked = useStore(s=>s.parkedPaneIds);
 const backend = useStore(s=>s.activeBackend);
 const sessions = useTerminalSessions();
 useLayoutEffect(()=> {
  if(!routingPrincipalAllowed())return;
  const state = useStore.getState(); const scope = currentRoutingScope();
  const visibleIds = effectiveVisiblePaneIds(state);
  const visible = new Set(visibleIds);
  const opportunistic = new Set(warmPanesActive() ? parked : []);
  // Filtering the workspace must not dispose its hidden terminal cores/drafts.
  if (workspaceView) for (const id of paneIdsInOrder(layout)) opportunistic.add(id);
  const retain = view.kind === 'owner';
  // Confirmed active-scope metadata may replace a process; absence from a
  // partial/failed fetch never disposes a retained draft.
  for(const cached of sessions) {
   if(cached.scope.key !== scope.key)continue;
   const confirmed=panes[cached.pane.id];if(!confirmed)continue;
   if(confirmed.kind === 'native')forgetTerminalSession(cached.key);
   else ensureTerminalSession(scope,confirmed,cached.retained);
  }
  // Pin already visited workspace cores before hiding the grid.
  const participating = new Set(state.ownerReturnContext?.scope.key === scope.key ? state.ownerReturnContext.visiblePaneIds ?? paneIdsInOrder(layout) : []);
  if (retain) for (const s of sessions) if(s.scope.key === scope.key && participating.has(s.pane.id)) s.retained = true;
  for (const id of [...visible, ...opportunistic]) {
   const pane = panes[id] ?? (view.kind === 'owner' && view.pane?.id === id ? view.pane : undefined);
   if (pane && pane.kind !== 'native') ensureTerminalSession(scope,pane,retain || pane.role === 'task_owner');
  }
  reconcileTerminalSessions(scope,visible,opportunistic);
 },[panes,view,layout,workspaceView,focus,zoom,parked,backend]);
 return <>{sessions.map(session=><TerminalPortal key={session.key} session={session} />)}</>;
}
