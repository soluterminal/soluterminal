import { useLayoutEffect } from 'react';
import { createPortal } from 'react-dom';
import { effectiveVisiblePaneIds, useStore, warmPanesActive } from '../store';
import { workspaceViewLayout } from '../lib/workspaceView';
import { currentRoutingScope, routingPrincipalAllowed } from '../api';
import { acquirePaneHost } from '../lib/paneHosts';
import { ensureTerminalSession, registerTerminalPortal, forgetTerminalSession, reconcileTerminalSessions, useTerminalSessions, type TerminalViewSession } from '../lib/terminalViewSessions';
import { paneIdsInOrder } from '../lib/layout';
import { ErrorBoundary } from './ErrorBoundary';
import { Pane } from './Pane';
import { PaneRecovery } from './PaneRecovery';

// The pane type a "Report this" names: the agent when one runs, else the shell.
function paneTypeLabel(p: TerminalViewSession['pane']): string {
  if (p.hasClaude) return 'Claude';
  if (p.hasCodex) return 'Codex';
  return p.backend === 'win' ? 'PowerShell' : 'Terminal';
}

function TerminalPortal({ session }: { session: TerminalViewSession }) {
  useLayoutEffect(() => registerTerminalPortal(session.key), [session.key]);
  // Match Alt+1…9, which count the panes the grid shows; panes outside it
  // keep zero (no badge).
  const layout = useStore(workspaceViewLayout);
  const paneIndex = paneIdsInOrder(layout).indexOf(session.pane.id) + 1;
  return createPortal(
    <ErrorBoundary label={`Pane ${session.pane.id}`} reportAs={paneTypeLabel(session.pane)}>
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
 // What the grid shows (memoised), so view, All panes and tree changes all
 // mount or retire terminals.
 const shown = useStore(workspaceViewLayout);
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
  // Only while the grid hides saved panes (an overview with hidden workers,
  // or a pane/team view): a hidden worker keeps its core until it leaves the
  // layout. With nothing hidden, mobile and zoom still mount one terminal.
  if (view.kind === 'workspace' && shown !== layout) for (const id of paneIdsInOrder(layout)) opportunistic.add(id);
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
 },[panes,view,layout,shown,focus,zoom,parked,backend]);
 return <>{sessions.map(session=><TerminalPortal key={session.key} session={session} />)}</>;
}
