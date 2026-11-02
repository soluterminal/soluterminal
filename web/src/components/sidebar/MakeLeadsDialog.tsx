import { useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { api, currentRoutingScope } from '../../api';
import { useStore } from '../../store';
import { paneRowLabel } from '../../lib/paneRowLabel';
import { paneTeamKind } from '../../lib/paneTeam';
import type { PaneTreeNode } from '../../lib/workspacePaneTree';
import { useFocusTrap } from '../useFocusTrap';

interface Candidate { id: string; pid: number; name: string; role: string; checked: boolean }

/** Your own running panes in a workspace that could become leads. A pane that
 *  has already started workers is acting as a lead, so it starts ticked. */
export function leadCandidates(nodes: PaneTreeNode[]): Candidate[] {
  return nodes
    .filter(({ pane }) => pane.status === 'running' && pane.role !== 'task_owner' && paneTeamKind(pane) === '')
    .map(({ pane, children }) => {
      const label = paneRowLabel(pane);
      return { id: pane.id, pid: pane.pid, name: label.text, role: label.handle.replace(/^@/, ''), checked: children.length > 0 };
    });
}

/** Make several of your own panes leads at once, each with its role, through
 *  the same daemon call as the pane menu's Make lead… */
export function MakeLeadsDialog({ workspace, nodes, onClose }: { workspace: string; nodes: PaneTreeNode[]; onClose: () => void }) {
  const [rows, setRows] = useState(() => leadCandidates(nodes));
  // The rows are this daemon's panes as of opening: every call goes to that
  // daemon, and the dialog closes if the app moves to another machine,
  // backend or view while it is open.
  const [opened] = useState(() => ({ scope: currentRoutingScope(), generation: useStore.getState().navigationGeneration }));
  const stillHere = () => opened.scope.key === currentRoutingScope().key && opened.generation === useStore.getState().navigationGeneration;
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const cardRef = useRef<HTMLFormElement>(null);
  useFocusTrap(cardRef, true);
  useEffect(() => useStore.subscribe(() => { if (!stillHere()) onClose(); }));
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => { if (event.key === 'Escape' && !event.defaultPrevented) onClose(); };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [onClose]);
  const chosen = rows.filter(row => row.checked);
  const roleOf = (row: Candidate) => row.role.trim().replace(/^@/, '');
  const ready = chosen.length > 0 && chosen.every(row => roleOf(row));
  const update = (id: string, change: Partial<Candidate>) => setRows(previous => previous.map(row => row.id === id ? { ...row, ...change } : row));

  const save = async () => {
    setSaving(true);
    setError(null);
    const failed = new Map<string, string>();
    for (const row of chosen) {
      if (!stillHere()) return;
      try {
        const { origin } = await api.setPaneTeam(row.id, 'lead', roleOf(row), opened.scope);
        if (stillHere()) useStore.setState(s => s.panes[row.id]?.pid === row.pid ? { panes: { ...s.panes, [row.id]: { ...s.panes[row.id], origin } } } : {});
      } catch (err) {
        failed.set(row.id, `${row.name}: ${(err as Error).message || 'failed'}`);
      }
    }
    if (!stillHere()) return;
    setSaving(false);
    const made = chosen.length - failed.size;
    if (made > 0) useStore.getState().toast(`Made ${made} lead${made === 1 ? '' : 's'} in ${workspace}`, { kind: 'info' });
    if (failed.size === 0) { onClose(); return; }
    // Keep only what failed, so trying again does not repeat the rest.
    setError(`Could not make ${failed.size === 1 ? 'this pane' : 'these panes'} a lead:\n${[...failed.values()].join('\n')}`);
    setRows(previous => previous.filter(row => failed.has(row.id)));
  };

  return createPortal(
    <div className="dialog-backdrop" role="presentation" onMouseDown={onClose}>
      <form ref={cardRef} className="dialog make-leads-dialog" role="dialog" aria-modal="true" aria-label={`Make leads in ${workspace}`}
        onSubmit={event => { event.preventDefault(); if (ready && !saving) void save(); }}
        onMouseDown={event => event.stopPropagation()}>
        <div className="dialog-header">
          <h2 className="dialog-title">Make leads in {workspace}</h2>
        </div>
        <div className="dialog-help">
          A lead is a long-lived role pane: it owns an area, keeps its name and is never closed automatically.
          Panes that already started workers are ticked.
        </div>
        {rows.length === 0 ? <div className="dialog-help">Every pane of yours here is already a lead or a worker.</div> : (
          <ul className="make-leads-list">
            {rows.map(row => (
              <li key={row.id}>
                <label className="make-leads-pane">
                  <input type="checkbox" checked={row.checked} onChange={e => update(row.id, { checked: e.target.checked })} />
                  <span title={row.name}>{row.name}</span>
                </label>
                <input className="make-leads-role" aria-label={`Role for ${row.name}`} placeholder="role, e.g. security"
                  value={row.role} disabled={!row.checked} onChange={e => update(row.id, { role: e.target.value })} />
              </li>
            ))}
          </ul>
        )}
        {error && <div className="dialog-error" style={{ whiteSpace: 'pre-line' }}>{error}</div>}
        <div className="dialog-actions">
          <button type="button" onClick={onClose} disabled={saving}>Cancel</button>
          <button type="submit" className="primary" disabled={!ready || saving}>
            {saving ? 'Saving…' : chosen.length ? `Make ${chosen.length} lead${chosen.length === 1 ? '' : 's'}` : 'Make leads'}
          </button>
        </div>
      </form>
    </div>,
    document.body,
  );
}
