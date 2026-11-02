import { workspaceViewLayout } from '../lib/workspaceView';

import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { IconMenu } from './ui/icons';
import { createPortal } from 'react-dom';
import { useStore } from '../store';
import { collectLeaves } from '../lib/layout';
import { AttentionInbox, useAttentionBadge, useAttentionItems } from './AttentionInbox';
import type { Layout, PaneRuntimeState } from '../types';
import { slotList } from '../extensions';

interface Props {
  // Opens the sidebar drawer. Placed at the leftmost of the bottom bar
  // so phone thumbs naturally rest near it.
  onOpenDrawer: () => void;
  // Lit when any off-screen pane needs attention.
  drawerHasAttention?: boolean;
  // Controlled visibility of the on-screen key bar. Paired with the ⌨
  // toggle button on the right edge of this bottom bar.
  keybarOpen: boolean;
  onToggleKeybar: () => void;
}

// Unified mobile bottom bar. Replaces the old horizontally-scrolling
// tab strip + separate voice/keybar FABs. Layout:
//   [☰ menu]  [current-tile ▾]  [🎤 mic]  [🚀/✎]  [⌨ keys]
//
// The terminal picker is a drop-up: tap → a menu opens anchored above
// the button listing every visible leaf in the current layout with its
// title + state dot. This keeps the bar from ever overflowing horizontally,
// regardless of how many leaves you have.
type MobileLeafTab = {
  id: string;
  type: 'pane' | 'browser' | 'file' | 'diff' | 'tasks';
  icon: ReactNode;
  title: string;
  state: PaneRuntimeState;
  dead: boolean;
};

type VisibleLeaf = Exclude<Layout, { type: 'split' }>;

function fileName(path: string): string {
  const cleaned = path.replace(/[/\\]+$/, '');
  const tail = cleaned.split(/[/\\]/).pop();
  return tail || path || 'File';
}

function browserLabel(url: string | undefined, title: string | undefined): string {
  if (title?.trim()) return title.trim();
  if (!url) return 'Browser';
  try {
    const u = new URL(url);
    return u.hostname || url;
  } catch {
    return url;
  }
}

// Bottom sheet listing every pane that needs the user (approvals, questions,
// waiting for input) — the mobile counterpart of the desktop footer's
// "N need you" chip + popover, reusing the same AttentionInbox rows and
// revealPane jump. Mounted only while open so useAttentionItems' store
// subscriptions don't tax the bar; portaled to body so no bottombar stacking
// context can clip it.
function MobileAttentionSheet({ count, onClose }: { count: number; onClose: () => void }) {
  const items = useAttentionItems();
  const zoomedPaneId = useStore((s) => s.zoomedPaneId);
  const toggleZoom = useStore((s) => s.toggleZoom);
  return createPortal(
    <>
      <div className="mobile-attn-backdrop" onClick={onClose} />
      <div className="mobile-attn-sheet" role="dialog" aria-label="Waiting on you">
        <div className="mobile-attn-head">
          <span className="mobile-attn-title">
            {count} waiting on you
          </span>
          <button type="button" className="mobile-attn-close" onClick={onClose} aria-label="Close">
            ✕
          </button>
        </div>
        <AttentionInbox
          items={items}
          onSelect={(paneId) => {
            // Exit zoom when revealing a different leaf — otherwise the zoomed
            // one would still cover it (same rule as the leaf picker).
            if (zoomedPaneId && zoomedPaneId !== paneId) toggleZoom();
            onClose();
          }}
        />
      </div>
    </>,
    document.body,
  );
}

export function MobileTabBar({
  onOpenDrawer,
  drawerHasAttention,
  keybarOpen,
  onToggleKeybar,
}: Props) {
  // Task Owners are team-only on the public beta; dictation needs a free
  // account (cloud voice).


  const ownerView = useStore(s=>s.mainPresentation);
  const focusedPaneId = useStore((s) => s.focusedPaneId);
  const setFocus = useStore((s) => s.setFocus);
  const toggleZoom = useStore((s) => s.toggleZoom);

  // Narrow subscription trick: return a primitive string hash of
  // the tab descriptors. Strings compare by value, so if no
  // display field changed the return is === previous and Zustand
  // skips re-rendering. When it DOES change, useMemo keyed on
  // the hash rebuilds the actual tab objects (cheap). This avoids
  // subscribing to the full `panes` map which swaps reference on
  // every WS byte / every refreshPanes poll.
  const tabsHash = useStore((s) => {
    const leaves = collectLeaves(workspaceViewLayout(s));
    let h = '';
    for (const leaf of leaves) {
      if (leaf.type === 'pane') {
        const p = s.panes[leaf.paneId];
        h += `pane\t${leaf.paneId}\t${p?.title || ''}\t${p?.cmd?.[0] || ''}\t${p?.state || ''}\t${p?.status || ''}\n`;
      } else if (leaf.type === 'browser') {
        const b = s.browsers[leaf.browserId];
        h += `browser\t${leaf.browserId}\t${b?.title || ''}\t${b?.url || ''}\n`;
      } else if (leaf.type === 'file') {
        const f = s.files[leaf.fileId];
        h += `file\t${leaf.fileId}\t${f?.path || ''}\n`;
      } else if (leaf.type === 'diff') {
        h += `diff\t${leaf.diffId}\t${leaf.path}\t${leaf.staged}\n`;
      } else if (leaf.type === 'tasks') {
        h += `tasks\t${leaf.tasksId}\t${leaf.selectedTaskId || ''}\t${leaf.composing}\t${leaf.ownerTaskId || ''}\n`;
      }
    }
    return h;
  });
  const tabs = useMemo(() => {
    const s = useStore.getState();
    const leaves = collectLeaves(workspaceViewLayout(s)) as VisibleLeaf[];
    return leaves.filter(leaf => leaf.type !== 'tasks').map((leaf): MobileLeafTab => {
      if (leaf.type === 'browser') {
        const b = s.browsers[leaf.browserId];
        return {
          id: leaf.browserId,
          type: 'browser',
          icon: 'W',
          title: browserLabel(b?.url, b?.title),
          state: b ? 'idle' : 'exited',
          dead: !b,
        };
      }
      if (leaf.type === 'file') {
        const f = s.files[leaf.fileId];
        return {
          id: leaf.fileId,
          type: 'file',
          icon: 'F',
          title: f ? fileName(f.path) : leaf.fileId.slice(-6),
          state: f ? 'idle' : 'exited',
          dead: !f,
        };
      }
      if (leaf.type === 'diff') {
        return {
          id: leaf.diffId,
          type: 'diff',
          icon: 'Δ',
          title: `diff · ${fileName(leaf.path)}`,
          state: 'idle',
          dead: false,
        };
      }
      const p = s.panes[leaf.paneId];
      return {
        id: leaf.paneId,
        type: 'pane',
        icon: '$',
        title: p?.title || p?.cmd?.[0] || leaf.paneId.slice(-6),
        state: p?.state || 'exited',
        dead: !p || p.status !== 'running',
      };
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tabsHash]);

  const [pickerOpen, setPickerOpen] = useState(false);
  const pickerRootRef = useRef<HTMLDivElement | null>(null);

  // "N need you" bell — appears only when something actually needs the user,
  // so the bar stays uncluttered in the common case. Tapping opens the
  // attention sheet; the sheet closes itself when the count drains to zero.
  const { count: attnCount, hasChoice, firstPaneId: attnPaneId } = useAttentionBadge();
  const revealPane = useStore((s) => s.revealPane);
  const zoomedPaneId = useStore((s) => s.zoomedPaneId);
  const [attnOpen, setAttnOpen] = useState(false);
  useEffect(() => {
    if (attnCount === 0) setAttnOpen(false);
  }, [attnCount]);

  // Close the picker on any tap outside it. Listening on pointerdown
  // (capture phase) so we catch the event before children stopPropagate
  // for some reason. Also close on Escape for keyboard users.
  useEffect(() => {
    if (!pickerOpen) return;
    const onDown = (e: PointerEvent) => {
      const root = pickerRootRef.current;
      if (root && !root.contains(e.target as Node)) setPickerOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setPickerOpen(false);
    };
    document.addEventListener('pointerdown', onDown, { capture: true });
    window.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('pointerdown', onDown, { capture: true });
      window.removeEventListener('keydown', onKey);
    };
  }, [pickerOpen]);

  // Mirror picker visibility to a body class so the bottom bar's
  // stacking context can rise above .mobile-keybar (z=50) while the
  // drop-up menu is open. The bottombar itself is z=45 and creates a
  // stacking context that traps the menu's own z-index (60) below the
  // keybar otherwise — the menu visually drops behind the keys.
  useEffect(() => {
    const cls = 'bottombar-picker-open';
    if (pickerOpen) document.body.classList.add(cls);
    else document.body.classList.remove(cls);
    return () => document.body.classList.remove(cls);
  }, [pickerOpen]);

  const current = tabs.find((t) => t.id === focusedPaneId) || null;
  const currentTitle = ownerView.kind === 'owner' ? 'Retained terminal' : current?.title || (focusedPaneId ? focusedPaneId.slice(-6) : 'No tile');
  const currentState = ownerView.kind === 'owner' ? ownerView.pane?.state || 'idle' : current?.state || 'idle';
  const currentIcon = ownerView.kind === 'owner' ? ownerView.history || !ownerView.pane ? '◷' : '$' : current?.icon || '$';
  const currentIdx = focusedPaneId ? tabs.findIndex((t) => t.id === focusedPaneId) : -1;

  return (
    <div className="mobile-bottombar" role="toolbar" aria-label="Bottom bar">
      <button
        type="button"
        className="bottombar-btn bottombar-menu"
        onClick={onOpenDrawer}
        title="Open workspaces"
        aria-label="Open workspaces"
      >
        <span aria-hidden><IconMenu size={18} /></span>
        {drawerHasAttention && <span className="bottombar-dot" aria-hidden />}
      </button>

      <div className="bottombar-picker-wrap" ref={pickerRootRef}>
        <button
          type="button"
          className="bottombar-picker"
          onClick={() => setPickerOpen((v) => !v)}
          aria-haspopup="listbox"
          aria-expanded={pickerOpen}
          title={currentTitle}
        >
          <span className={`bottombar-picker-icon leaf-${current?.type || 'pane'}`} aria-hidden>{currentIcon}</span>
          <span className={`mobile-tab-dot state-${currentState}`} aria-hidden />
          <span className="bottombar-picker-title">{currentTitle}</span>
          <span className="bottombar-picker-count">
            {ownerView.kind === 'owner' ? ' · Owner' : tabs.length > 0 ? ` · ${currentIdx >= 0 ? currentIdx + 1 : '?'}/${tabs.length}` : ''}
          </span>
          <span className="bottombar-picker-chevron" aria-hidden>▴</span>
        </button>
        {pickerOpen && (
          <div className="bottombar-picker-menu" role="listbox">

            {ownerView.kind === 'owner' && <button type="button" className="bottombar-picker-item" onClick={()=>{useStore.getState().returnToWorkspace();setPickerOpen(false);}}>Return to workspace</button>}
            {tabs.length === 0 && (
              <div className="bottombar-picker-empty">No tiles in this workspace</div>
            )}
            {tabs.map(({ id, type, icon, title, state, dead }) => {
              const focused = ownerView.kind === 'workspace' && focusedPaneId === id;
              const kind = type === 'pane' ? 'Terminal' : type === 'browser' ? 'Browser' : type === 'diff' ? 'Diff' : type === 'tasks' ? 'Tasks' : 'File';
              return (
                <button
                  key={id}
                  type="button"
                  role="option"
                  aria-selected={focused}
                  aria-label={`${kind} ${title}`}
                  className={`bottombar-picker-item ${focused ? 'active' : ''} ${dead ? 'dead' : ''}`}
                  onClick={() => {
                    // Exit zoom if switching to a different leaf — otherwise
                    // the zoomed leaf would still cover whatever we picked.
                    if(ownerView.kind === 'owner') useStore.getState().returnToWorkspace();
                    const restoredZoom=useStore.getState().zoomedPaneId;
                    if (restoredZoom && restoredZoom !== id) toggleZoom();
                    setFocus(id);
                    setPickerOpen(false);
                  }}
                >
                  <span className={`bottombar-picker-icon leaf-${type}`} aria-hidden>{icon}</span>
                  <span className={`mobile-tab-dot state-${state}`} aria-hidden />
                  <span className="bottombar-picker-item-title">
                    <span className="bottombar-picker-item-kind">
                      {kind}
                    </span>
                    <span>{title}</span>
                  </span>
                </button>
              );
            })}
          </div>
        )}
      </div>

      {attnCount > 0 && (
        <button
          type="button"
          className={`bottombar-btn bottombar-attn${hasChoice ? ' choice' : ''}`}
          onClick={() => {
            // One waiting pane: go straight to it. Several: list them.
            if (attnCount === 1 && attnPaneId) {
              if (zoomedPaneId && zoomedPaneId !== attnPaneId) toggleZoom();
              void revealPane(attnPaneId);
            } else {
              setAttnOpen(true);
            }
          }}
          title={`${attnCount} waiting on you`}
          aria-label={attnCount === 1
            ? '1 waiting on you — go to it'
            : `${attnCount} waiting on you — open the list`}
        >
          <span className="bottombar-attn-badge" aria-hidden>
            {attnCount}
          </span>
        </button>
      )}
      {attnOpen && <MobileAttentionSheet count={attnCount} onClose={() => setAttnOpen(false)} />}

      {/* Items of optional features (extensions.ts). */}
      {slotList('mobileBarItems').map((Item, i) => <Item key={i} />)}

      <button
        type="button"
        className="bottombar-btn bottombar-palette"
        onClick={() =>
          window.dispatchEvent(new CustomEvent('solu:open-palette', { detail: { mode: 'actions' } }))
        }
        title="Command palette"
        aria-label="Open command palette"
      >
        <span aria-hidden>⌘</span>
      </button>

      <button
        type="button"
        className={`bottombar-btn bottombar-keybar ${keybarOpen ? 'active' : ''}`}
        onClick={onToggleKeybar}
        title={keybarOpen ? 'Hide on-screen keys' : 'Show on-screen keys'}
        aria-label={keybarOpen ? 'Hide on-screen keys' : 'Show on-screen keys'}
        aria-pressed={keybarOpen}
      >
        <span aria-hidden>⌨</span>
      </button>
    </div>
  );
}
