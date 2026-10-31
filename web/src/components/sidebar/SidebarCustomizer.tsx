
import { useState } from 'react';
import { usePrefs } from '../../prefs';
import {
  MIDDLE_SECTION_LABELS,
  effectiveHidden,
  reorderMiddle,
  resolvedMiddleOrder,
  sectionOffered,
  withoutTeamOnly,
  type MiddleKey,
} from './sidebarSections';
import { useOperatorFeatures, usePersonalView, useTeamFeatures } from '../../entitlements';

// Reusable "customize sidebar sections" panel: drag to reorder, check to
// show/hide. Writes straight to the daemon-backed prefs store, so it's live —
// no separate Save. Rendered in a lightweight dialog from the sidebar's
// right-click menu today, and embedded as a Settings tab (Phase 4). Content
// only — the caller supplies any surrounding modal chrome.

const DRAG_MIME = 'application/x-solu-section';

export const PERSONAL_VIEW_SIDEBAR_NOTE = 'Sidebar changes are off in personal view';

export function SidebarCustomizer() {
  const sidebar = usePrefs((s) => s.prefs.sidebar);
  const setPrefs = usePrefs((s) => s.setPrefs);
  const [dragOver, setDragOver] = useState<MiddleKey | null>(null);

  const teamFeatures = useOperatorFeatures();

  // In personal view the list shows the public arrangement, which isn't the
  // user's own: editing it would overwrite their real sidebar. Read-only.
  const readOnly = usePersonalView();
  const teamPlan = useTeamFeatures();
  const order = withoutTeamOnly(resolvedMiddleOrder(sidebar?.order), teamFeatures, teamPlan).filter(key => key !== 'owners' && sectionOffered(key));
  // Reflect the EFFECTIVE hidden set so a never-customized user sees the shipped
  // default-hidden sections unchecked (not everything checked). Editing from
  // here materializes that default into an explicit set.
  const hidden = new Set(effectiveHidden(sidebar?.order, sidebar?.hidden, teamFeatures));
  const customized = Boolean(sidebar?.order?.length) || Boolean(sidebar?.hidden?.length);

  const setHidden = (key: MiddleKey, hide: boolean) => {
    if (readOnly) return;
    setPrefs((p) => {
      const next = new Set(effectiveHidden(p.sidebar?.order, p.sidebar?.hidden, teamFeatures));
      if (hide) next.add(key);
      else next.delete(key);
      return { ...p, sidebar: { ...p.sidebar, hidden: [...next] } };
    });
  };

  const doReorder = (from: MiddleKey, to: MiddleKey, pos: 'before' | 'after') => {
    if (readOnly) return;
    setPrefs((p) => ({
      ...p,
      sidebar: { ...p.sidebar, order: reorderMiddle(p.sidebar?.order, from, to, pos) },
    }));
  };

  const reset = () => {
    if (!readOnly) setPrefs((p) => ({ ...p, sidebar: {} }));
  };

  return (
    <div className="sidebar-customizer">
      {readOnly ? (
        <p className="dialog-help">{PERSONAL_VIEW_SIDEBAR_NOTE}</p>
      ) : (
        <p className="dialog-help">
          Drag to reorder. Uncheck to hide a section — you can bring it back anytime
          from the sidebar’s right-click menu. Only Workspaces stays pinned.
        </p>
      )}
      <ul className="sidebar-customizer-list">
        {order.map((key) => (
          <li
            key={key}
            className={`sidebar-customizer-row${dragOver === key ? ' drag-over' : ''}`}
            draggable={!readOnly}
            onDragStart={(e) => {
              e.dataTransfer.setData(DRAG_MIME, key);
              e.dataTransfer.effectAllowed = 'move';
            }}
            onDragOver={(e) => {
              if (readOnly || !e.dataTransfer.types.includes(DRAG_MIME)) return;
              e.preventDefault();
              e.dataTransfer.dropEffect = 'move';
              setDragOver(key);
            }}
            onDragLeave={(e) => {
              const related = e.relatedTarget as Node | null;
              if (!e.currentTarget.contains(related)) setDragOver(null);
            }}
            onDrop={(e) => {
              if (readOnly || !e.dataTransfer.types.includes(DRAG_MIME)) return;
              e.preventDefault();
              const src = e.dataTransfer.getData(DRAG_MIME) as MiddleKey;
              const r = e.currentTarget.getBoundingClientRect();
              const pos: 'before' | 'after' = e.clientY - r.top < r.height / 2 ? 'before' : 'after';
              setDragOver(null);
              if (src && src !== key) doReorder(src, key, pos);
            }}
          >
            <span className="sidebar-customizer-handle" aria-hidden>⋮⋮</span>
            <label className="sidebar-customizer-label">
              <input
                type="checkbox"
                checked={!hidden.has(key)}
                disabled={readOnly}
                onChange={(e) => setHidden(key, !e.target.checked)}
              />
              <span>{MIDDLE_SECTION_LABELS[key]}</span>
            </label>
          </li>
        ))}
      </ul>
      <div className="sidebar-customizer-actions">
        <button type="button" onClick={reset} disabled={readOnly || !customized}>
          Reset to default
        </button>
      </div>
    </div>
  );
}
