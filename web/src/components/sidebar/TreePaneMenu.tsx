import { useRef, useState } from 'react';
import { useStore } from '../../store';
import { declaredPaneTeamKind } from '../../lib/paneTeam';
import type { PaneInfo } from '../../types';
import { PaneIdentityDialog } from '../PaneIdentityDialog';
import { useClampedMenuPosition } from '../useClampedMenuPosition';
import { useDismiss } from '../useDismiss';
import { usePaneContextMenu } from '../usePaneContextMenu';

const noop = () => {};

/** The right-click menu of a pane row in the sidebar tree: the pane menu's
 *  naming, team and close items, acting on that pane without opening it. */
export function TreePaneMenu({ pane, x, y, onClose }: { pane: PaneInfo; x: number; y: number; onClose: () => void }) {
  const [editingIdentity, setEditingIdentity] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const position = useClampedMenuPosition(ref, editingIdentity ? null : { x, y });
  useDismiss(onClose, { enabled: !editingIdentity, inside: [ref] });
  // The header menu's own team and close actions: same prompts, guards and
  // daemon call (POST /api/panes/{id}/team). Inline title editing belongs to
  // the pane header, so those setters do nothing; the identity dialog renames.
  const { actions } = usePaneContextMenu({
    pane, killPane: id => useStore.getState().killPane(id), setFocus: noop,
    setTitleDraft: noop, setEditingTitle: noop, setFlagMemoDraft: noop, setEditingFlagMemo: noop,
  });
  if (editingIdentity) return <PaneIdentityDialog pane={pane} onClose={onClose} />;
  const running = pane.status === 'running' && pane.role !== 'task_owner';
  const kind = declaredPaneTeamKind(pane);
  const agentStarted = pane.origin?.source === 'agent';
  const act = (run: () => unknown) => () => { onClose(); void run(); };
  return (
    <div ref={ref} className="pane-ctx-menu" role="menu" aria-label={`${pane.identity?.handle ? `@${pane.identity.handle}` : pane.title} actions`}
      style={{ position: 'fixed', left: position.left, top: position.top, zIndex: 'var(--z-menu)' }}
      onClick={e => e.stopPropagation()}>
      {running && <button role="menuitem" onClick={() => setEditingIdentity(true)}
        title="Its title, a handle to address it by, and what it is working on">Rename, handle and working on…</button>}
      {running && <>
        {kind !== 'lead' && <button role="menuitem" onClick={act(() => actions.setTeamKind('lead'))}
          title="A long-lived role pane (security, CMO…): never closed automatically">Make lead…</button>}
        {kind === 'lead' && <button role="menuitem" onClick={act(() => actions.setTeamKind('lead'))} title="Change this lead's role name">Rename lead role…</button>}
        {agentStarted && kind !== 'worker' && <button role="menuitem" onClick={act(() => actions.setTeamKind('worker'))}
          title="A task for its lead: archived automatically once finished, read by its lead, and committed and pushed">Make worker</button>}
        {!agentStarted && kind === 'lead' && <button role="menuitem" onClick={act(() => actions.setTeamKind('none'))}
          title="Back to a plain pane of yours">Remove lead</button>}
      </>}
      <div className="pane-ctx-menu-sep" />
      <button role="menuitem" className="danger" onClick={act(actions.closePane)}>Close…</button>
    </div>
  );
}
