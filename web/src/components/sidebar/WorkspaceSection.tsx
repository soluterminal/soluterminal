import { useStore } from '../../store';
import { createWorkspacePaneTreeSelector, type WorkspacePaneTree } from '../../lib/workspacePaneTree';
import { workspaceViewKey } from '../../lib/workspaceView';
import { paneRowStatus } from '../../lib/paneStatus';
import { WorkspacePaneRows, branchKeys, foldedByDefault, isFolded, paneTreeKey } from './WorkspacePaneRows';
import { useEffect, useMemo, useRef, useState, type Dispatch, type DragEvent as ReactDragEvent, type MutableRefObject, type RefObject, type SetStateAction } from 'react';
import { paneUnread } from '../../lib/turnChip';
import { isRouterMode } from '../../dualBackend';
import type { Layout, PaneInfo, Workspace } from '../../types';
import { useAttentionBadge } from '../AttentionInbox';
import { useIsMobileView } from '../useIsMobileView';
import { SectionHeader } from './SectionHeader';
import { IconLayers, IconPlus, IconX, IconFlag, IconCollapseAll, IconExpandAll } from '../ui/icons';
import { relativeTime } from './time';
import { confirmDialog } from '../../lib/dialog';
import { useClampedMenuPosition } from '../useClampedMenuPosition';
import { ContextPackDialog } from '../ContextPackDialog';
import { TreePaneMenu } from './TreePaneMenu';
import { MakeLeadsDialog, leadCandidates } from './MakeLeadsDialog';

const WS_DRAG_MIME = 'application/x-solu-workspace';
const TREE_COLLAPSED_KEY = 'solu.sidebarTreeCollapsed.v1';

type WorkspaceMenu = { x: number; y: number; wsName: string } | null;

// Where a dragged workspace would land: the hovered row + which edge. The
// SAME halfway rule decides both this (the indicator line) and the actual
// drop, so the line is always an honest preview.
export type WorkspaceDropTarget = { key: string; pos: 'before' | 'after' } | null;

function dropPosition(e: ReactDragEvent<HTMLElement>): 'before' | 'after' {
  const r = e.currentTarget.getBoundingClientRect();
  return (e.clientY - r.top) < r.height / 2 ? 'before' : 'after';
}

interface WorkspaceSectionProps {
  collapsed: boolean;
  named: Workspace[];
  trees?: WorkspacePaneTree[];
  panes: Record<string, PaneInfo>;
  activeWorkspace: string;
  activeBackend: 'wsl' | 'win' | null;
  layout: Layout | null;
  focusedPaneId: string | null;
  wsLastViewedMs: Record<string, number>;
  creating: boolean;
  createDraft: string;
  createCancelRef: MutableRefObject<boolean>;
  dropTarget: WorkspaceDropTarget;
  renamingName: string | null;
  renameDraft: string;
  ctxMenu: WorkspaceMenu;
  ctxMenuRef: RefObject<HTMLDivElement>;
  onToggle: () => void;
  onToggleCollapse?: () => void;
  onStartCreate: () => void;
  onCommitCreate: (name: string) => void;
  setCreateDraft: Dispatch<SetStateAction<string>>;
  setCreating: Dispatch<SetStateAction<boolean>>;
  setDropTarget: Dispatch<SetStateAction<WorkspaceDropTarget>>;
  setRenamingName: Dispatch<SetStateAction<string | null>>;
  setRenameDraft: Dispatch<SetStateAction<string>>;
  setCtxMenu: Dispatch<SetStateAction<WorkspaceMenu>>;
  onLoadWorkspace: (workspace: Workspace) => void | Promise<void>;
  onRenameWorkspace: (from: string, to: string) => Promise<void>;
  onSetWorkspaceContextPack: (name: string, contextPack: string) => Promise<void>;
  onDeleteWorkspace: (name: string) => void;
  onClearLayout: () => void;
  onReorderWorkspaces: (fromKey: string, toKey: string, position: 'before' | 'after') => void;
}

function workspaceKey(w: Workspace): string {
  return w.backend ? `${w.backend}:${w.name}` : w.name;
}

function shortRelativeTime(ms: number): string {
  const s = Math.max(0, Math.floor((Date.now() - ms) / 1000));
  if (s < 5) return 'now';
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

type WorkspaceRowSummary = {
  count: number;
  mostRecent: number;
  recencyLabel: string;
  recencyFull: string;
  badgeKind: 'choice' | 'awaiting' | 'activity' | null;
  badgeLabel: string;
  attentionCount: number;
  flaggedCount: number;
  flaggedSummaries: string[];
  tooltip: string;
  hasUnread: boolean;
};

export function WorkspaceSection({
  collapsed,
  named,
  trees: providedTrees,
  panes,
  activeWorkspace,
  activeBackend,
  layout,
  focusedPaneId,
  wsLastViewedMs,
  creating,
  createDraft,
  createCancelRef,
  dropTarget,
  renamingName,
  renameDraft,
  ctxMenu,
  ctxMenuRef,
  onToggle,
  onToggleCollapse,
  onStartCreate,
  onCommitCreate,
  setCreateDraft,
  setCreating,
  setDropTarget,
  setRenamingName,
  setRenameDraft,
  setCtxMenu,
  onLoadWorkspace,
  onRenameWorkspace,
  onSetWorkspaceContextPack,
  onDeleteWorkspace,
  onClearLayout,
  onReorderWorkspaces,
}: WorkspaceSectionProps) {
  const [packWorkspaceName, setPackWorkspaceName] = useState<string | null>(null);
  const [paneMenu, setPaneMenu] = useState<{ paneId: string; x: number; y: number } | null>(null);
  const [leadsWorkspaceKey, setLeadsWorkspaceKey] = useState<string | null>(null);
  const selectTrees = useMemo(createWorkspacePaneTreeSelector, []);
  const trees = providedTrees ?? selectTrees(named, panes, activeWorkspace, layout, activeBackend);
  const [treeCollapsed, setTreeCollapsed] = useState<Record<string, boolean>>(() => {
    try {
      const saved: unknown = JSON.parse(localStorage.getItem(TREE_COLLAPSED_KEY) || '{}');
      return saved && typeof saved === 'object' && !Array.isArray(saved)
        ? Object.fromEntries(Object.entries(saved).filter(([, value]) => typeof value === 'boolean')) : {};
    } catch { return {}; }
  });
  const [focusedTreeKey, setFocusedTreeKey] = useState(() => named[0] ? workspaceKey(named[0]) : '');
  const workspaceView = useStore(s => s.workspaceView);
  const allPanesPreferences = useStore(s => s.workspaceAllPanes);
  const treeRef = useRef<HTMLDivElement>(null);
  const selectionVersion = useRef(0);
  useEffect(() => {
    const visibleRows = Array.from(treeRef.current?.querySelectorAll<HTMLElement>('[data-tree-key]') ?? []);
    if (!visibleRows.some(row => row.dataset.treeKey === focusedTreeKey)) setFocusedTreeKey(named[0] ? workspaceKey(named[0]) : '');
  }, [trees, treeCollapsed, workspaceView, collapsed, named, focusedTreeKey]);
  // Every branch that can open or close: workspaces with panes, and panes
  // with panes below them.
  const allBranches = useMemo(() => trees.flatMap(tree => {
    const key = workspaceKey(tree.workspace);
    return tree.nodes.length > 0 ? [key, ...branchKeys(key, tree.roots)] : [];
  }), [trees]);
  // Folded unless the person opened them: a top-level pane's workers.
  const foldedDefaults = useMemo(() => new Set(trees.flatMap(tree => {
    const key = workspaceKey(tree.workspace);
    return tree.nodes.filter(foldedByDefault).map(node => paneTreeKey(key, node.pane.id));
  })), [trees]);
  const folded = (map: Record<string, boolean>, key: string) => isFolded(map, key, foldedDefaults.has(key));
  // Workers start folded, so the header offers Expand all while anything is
  // folded and Collapse all once everything is open.
  const allOpen = allBranches.every(key => !folded(treeCollapsed, key));
  const setBranches = (keys: string[], close: boolean | ((previous: Record<string, boolean>) => boolean)) => setTreeCollapsed(previous => {
    const closing = typeof close === 'function' ? close(previous) : close;
    const next: Record<string, boolean> = {};
    // The person's open/fold choices are stored; drop panes that have closed
    // and workspaces that no longer exist so the map does not grow forever.
    // With two backends the other daemon's panes are not loaded, so keep those.
    const workspaces = new Set(named.map(workspaceKey));
    const live = Object.keys(panes).length > 0 && !isRouterMode();
    for (const [key, value] of Object.entries(previous)) {
      const [ws, paneId] = key.split(':pane:');
      if (typeof value !== 'boolean' || (named.length > 0 && !workspaces.has(ws)) || (paneId && live && !panes[paneId])) continue;
      next[key] = value;
    }
    for (const key of keys) next[key] = closing;
    try { localStorage.setItem(TREE_COLLAPSED_KEY, JSON.stringify(next)); } catch { /* storage may be unavailable */ }
    return next;
  });
  const toggleTree = (key: string) => setBranches([key], previous => !folded(previous, key));
  const isActiveWorkspace = (w: Workspace) => {
    const state = useStore.getState();
    return state.activeWorkspace === w.name && (!w.backend || state.activeBackend === w.backend);
  };
  // A failed load already reports itself; it must not leave a stale view
  // request behind or surface as an unhandled rejection.
  const load = async (w: Workspace): Promise<boolean> => {
    try { await onLoadWorkspace(w); return true; } catch { return false; }
  };
  const selectPane = async (w: Workspace, paneId: string, team: boolean) => {
    const version = ++selectionVersion.current;
    const state = useStore.getState();
    if (isActiveWorkspace(w) && state.mainPresentation.kind === 'workspace') { state.showWorkspacePaneView(paneId, team); return; }
    // Arrive directly in the pane's view, without the overview in between.
    state.queueWorkspaceReveal({ workspace: w.name, backend: w.backend ?? state.activeBackend ?? null, paneId, kind: team ? 'team' : 'single' });
    const loaded = await load(w);
    useStore.getState().queueWorkspaceReveal(null);
    if (!loaded || version !== selectionVersion.current || !isActiveWorkspace(w)) return;
    useStore.getState().showWorkspacePaneView(paneId, team);
  };
  const selectWorkspace = async (w: Workspace) => {
    const version = ++selectionVersion.current;
    const state = useStore.getState();
    // The newest click wins over a pane view still waiting for its load.
    state.queueWorkspaceReveal(null);
    if (isActiveWorkspace(w) && state.mainPresentation.kind === 'workspace') state.showWorkspaceOverview();
    if (!await load(w) || version !== selectionVersion.current || !isActiveWorkspace(w)) return;
    useStore.getState().showWorkspaceOverview();
  };

  // Every delete path (the row's × and right-click › Delete) confirms first:
  // a deleted workspace's layout, tiles and context pack can't be undone.
  const confirmDelete = async (name: string) => {
    if (await confirmDialog({ message: `Delete workspace "${name}"?`, confirmLabel: 'Delete', danger: true })) onDeleteWorkspace(name);
  };
  const isMobileView = useIsMobileView();
  const ctxMenuPos = useClampedMenuPosition(ctxMenuRef, ctxMenu);
  const { count: attnCount, hasChoice: attnHasChoice } = useAttentionBadge();
  const attnClass = attnCount > 0 ? (attnHasChoice ? 'is-choice' : 'is-awaiting') : '';
  const attnTitle = attnHasChoice
    ? `${attnCount} pane${attnCount === 1 ? '' : 's'} need approval`
    : `${attnCount} pane${attnCount === 1 ? '' : 's'} waiting for input`;
  const rowSummaries = useMemo(() => {
    const now = Date.now();
    const out = new Map<string, WorkspaceRowSummary>();
    for (const tree of trees) {
      const w = tree.workspace;
      let count = 0;
      let mostRecent = 0;
      let badgeKind: WorkspaceRowSummary['badgeKind'] = null;
      let attentionCount = 0;
      const unreadPanes: string[] = [];
      const flaggedSummaries: string[] = [];
      const viewedAt = wsLastViewedMs[w.name] ?? 0;

      const ids = tree.paneIds;
      for (const id of ids) {
        const p = panes[id];
        if (!p) continue;
        if (p.status === 'running') count++;
        const lastActivity = p.lastActivityMs ?? 0;
        if (lastActivity > mostRecent) mostRecent = lastActivity;

        if (p.status === 'running') {
          let flagged = false;
          if (p.state === 'choice-prompt' && paneRowStatus(p) === 'needs-you') {
            badgeKind = 'choice';
            flagged = true;
            attentionCount++;
          } else if (p.state === 'awaiting-input' && paneRowStatus(p) === 'needs-you') {
            if (badgeKind !== 'choice') badgeKind = 'awaiting';
            flagged = true;
            attentionCount++;
          } else if ((p.asks?.length ?? 0) > 0) {
            // An agent's open ask (daemon asks.go) waits on you like a prompt.
            if (badgeKind !== 'choice') badgeKind = 'awaiting';
            flagged = true;
            attentionCount++;
          } else if (
            focusedPaneId !== id &&
            paneUnread(p, viewedAt, lastActivity) !== null
          ) {
            if (badgeKind === null) badgeKind = 'activity';
            flagged = true;
          }
          if (flagged) {
            unreadPanes.push(p.title || p.cmd.join(' '));
          }
        }

        if (p.flagged) {
          const title = p.title || p.cmd.join(' ');
          flaggedSummaries.push(p.flagMemo ? `${title} — ${p.flagMemo}` : title);
        }
      }

      const hasUnread = badgeKind !== null;
      const badgeLabel = badgeKind === 'choice' ? 'approval needed'
        : badgeKind === 'awaiting' ? 'waiting for input'
        : badgeKind === 'activity' ? 'activity'
        : '';
      const recencyLabel = mostRecent > 0 ? shortRelativeTime(mostRecent) : '';
      const recencyFull = mostRecent > 0 ? `last active ${relativeTime(mostRecent)}` : 'no activity yet';
      const tooltipParts = [
        `${count} pane${count === 1 ? '' : 's'} · ${recencyFull}`,
      ];
      if (hasUnread) tooltipParts.push(`${badgeLabel}: ${unreadPanes.join(', ')}`);
      if (flaggedSummaries.length > 0) tooltipParts.push(`flagged: ${flaggedSummaries.join(' · ')}`);
      if (!hasUnread && flaggedSummaries.length === 0) tooltipParts.push('drag to reorder · right-click for more');
      out.set(workspaceKey(w), {
        count,
        mostRecent,
        recencyLabel,
        recencyFull,
        badgeKind,
        badgeLabel,
        attentionCount,
        flaggedCount: flaggedSummaries.length,
        flaggedSummaries,
        tooltip: tooltipParts.join(' · '),
        hasUnread,
      });
    }
    return { summaries: out, now };
  }, [focusedPaneId, panes, wsLastViewedMs, trees]);
  return (
    <>
      <div
        className="sidebar-section sidebar-section-fill"
        // No height:100% — the card shrink-wraps to its rows (flex-basis
        // auto within the zone) and ends flush with the last one, like the
        // middle-zone is-fill slots; a longer list shrinks to the zone and
        // scrolls internally (min-height 0 + overflow auto).
        style={{ minHeight: 0, overflow: 'auto' }}
        ref={treeRef}
        role="tree"
        aria-label="Workspace panes"
        onFocus={e => {
          const row = (e.target as HTMLElement).closest<HTMLElement>('[data-tree-key]');
          if (row?.dataset.treeKey) setFocusedTreeKey(row.dataset.treeKey);
        }}
        onKeyDown={e => {
          if ((e.target as HTMLElement).closest('input, button')) return;
          const row = (e.target as HTMLElement).closest<HTMLElement>('[data-tree-key]');
          if (!row) return;
          const rows = Array.from(treeRef.current?.querySelectorAll<HTMLElement>('[data-tree-key]') ?? []);
          const index = rows.indexOf(row);
          const key = row.dataset.treeKey!;
          let next: HTMLElement | undefined;
          if (e.key === 'ArrowDown') next = rows[index + 1];
          else if (e.key === 'ArrowUp') next = rows[index - 1];
          else if (e.key === 'Home') next = rows[0];
          else if (e.key === 'End') next = rows.at(-1);
          else if (e.key === 'ArrowRight') {
            if (row.getAttribute('aria-expanded') === 'false') toggleTree(key);
            else if (row.getAttribute('aria-expanded') === 'true') next = rows[index + 1];
          } else if (e.key === 'ArrowLeft') {
            if (row.getAttribute('aria-expanded') === 'true') toggleTree(key);
            else next = rows.find(r => r.dataset.treeKey === row.dataset.treeParent);
          } else if (e.key === '*' || e.key === '-') {
            // Open (*) or close (-) this row and its siblings that have branches.
            const parent = row.dataset.treeParent;
            setBranches(rows.filter(r => r.dataset.treeParent === parent && r.hasAttribute('aria-expanded')).map(r => r.dataset.treeKey!), e.key === '-');
          } else if (e.key === 'ContextMenu' || (e.shiftKey && e.key === 'F10')) {
            const box = row.getBoundingClientRect();
            const at = { x: box.left + 24, y: box.top + Math.min(box.height, 30) };
            if (row.dataset.treeParent) setPaneMenu({ paneId: key.split(':pane:')[1], ...at });
            else {
              const w = named.find(item => workspaceKey(item) === key);
              if (w) setCtxMenu({ ...at, wsName: w.name });
            }
          } else if (e.key === 'Enter') row.click();
          else return;
          e.preventDefault();
          e.stopPropagation();
          next?.focus();
        }}
        // Scroll cue for the pinned header (CSS .is-scrolled draws the
        // hairline + shadow). Imperative toggle — no re-render per scroll.
        onScroll={(e) => {
          const el = e.currentTarget;
          el.classList.toggle('is-scrolled', el.scrollTop > 0);
        }}
      >
        <SectionHeader
          icon={<IconLayers size={14} />}
          title="Workspaces"
          count={named.length || undefined}
          collapsed={collapsed}
          onToggle={onToggle}
          extra={
            <>
              {!collapsed && allBranches.length > 0 && (
                <button
                  type="button"
                  className="iconbtn"
                  onClick={(e) => {
                    e.stopPropagation();
                    setBranches(allBranches, allOpen);
                  }}
                  title={allOpen ? 'Collapse all' : 'Expand all'}
                  aria-label={allOpen ? 'Collapse all' : 'Expand all'}
                >{allOpen ? <IconCollapseAll size={14} /> : <IconExpandAll size={14} />}</button>
              )}
              <button
                className="iconbtn"
                onClick={(e) => {
                  e.stopPropagation();
                  onStartCreate();
                }}
                title="New workspace (name it inline — Enter saves, Escape cancels)"
                aria-label="New workspace"
              ><IconPlus size={14} /></button>
              {/* Desktop's collapse control now lives in the topbar (the
                  always-visible IconPanelLeft toggle). Keep this in-header
                  chevron only for mobile, where that topbar toggle is hidden. */}
              {onToggleCollapse && isMobileView && (
                <button
                  className={`iconbtn sidebar-collapse-btn ${attnCount > 0 ? `has-attention ${attnClass}` : ''}`}
                  onClick={(e) => { e.stopPropagation(); onToggleCollapse(); }}
                  title={attnCount > 0 ? `Collapse sidebar (Ctrl+Q) — ${attnTitle}` : 'Collapse sidebar (Ctrl+Q)'}
                  aria-label="Collapse sidebar"
                >
                  {attnCount > 0 && (
                    <span className={`aria-attn-indicator ${attnClass}`} aria-hidden />
                  )}
                  ‹
                </button>
              )}
            </>
          }
        />
        {!collapsed && creating && (
          <div className="row ws-row ws-creating">
            <span className="ws-drag-handle" aria-hidden>＋</span>
            <input
              className="ws-rename-input"
              autoFocus
              placeholder={
                activeWorkspace === '_current' && layout
                  ? 'Name this workspace (saves current panes)'
                  : 'Name this workspace (starts blank)'
              }
              value={createDraft}
              onChange={(e) => setCreateDraft(e.target.value)}
              onBlur={() => {
                const cancelled = createCancelRef.current;
                createCancelRef.current = false;
                const next = createDraft.trim();
                setCreating(false);
                setCreateDraft('');
                if (!cancelled && next) onCommitCreate(next);
              }}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  (e.target as HTMLInputElement).blur();
                } else if (e.key === 'Escape') {
                  createCancelRef.current = true;
                  (e.target as HTMLInputElement).blur();
                }
                e.stopPropagation();
              }}
              onClick={(e) => e.stopPropagation()}
              onMouseDownCapture={(e) => e.stopPropagation()}
            />
          </div>
        )}
        {!collapsed && named.length === 0 && !creating && (
          <div style={{ fontSize: 11, color: 'var(--fg-dim)' }}>
            No saved workspaces yet. Current layout autosaves.
          </div>
        )}
        {!collapsed && named.map((w) => {
          const wKey = workspaceKey(w);
          const summary = rowSummaries.summaries.get(wKey);
          if (!summary) return null;
          // One at-a-glance status dot per row (Warp-style): the highest-priority
          // live state — approval > waiting > fresh activity > running > idle.
          const statusKind = summary.badgeKind ?? (summary.count > 0 ? 'running' : 'idle');
          const isActive = activeWorkspace === w.name &&
            (!isRouterMode() || !w.backend || w.backend === activeBackend);
          const tree = trees.find(t => workspaceKey(t.workspace) === wKey);
          const nodes = tree?.nodes ?? [];
          const treeStatus = tree?.status ?? 'idle';
          const paneSelected = isActive && (workspaceView?.kind === 'team' || workspaceView?.kind === 'single');
          const workspaceSelected = isActive && !paneSelected;
          const allPanes = allPanesPreferences[workspaceViewKey(w.name, w.backend ?? activeBackend)] === true;
          const treeOpen = !folded(treeCollapsed, wKey);
          const dropPos = dropTarget?.key === wKey ? dropTarget.pos : null;
          const metaParts = [
            `${summary.count} pane${summary.count === 1 ? '' : 's'}`,
            summary.recencyLabel ? summary.recencyFull : null,
            summary.badgeKind === 'awaiting' ? 'waiting' : null,
            summary.badgeKind === 'choice' ? 'approval needed' : null,
            summary.flaggedCount > 0 ? `${summary.flaggedCount} flagged` : null,
          ].filter(Boolean);
          return (
            <div key={wKey}
              role="treeitem"
              aria-level={1}
              aria-label={w.name === '_current' ? 'Current workspace' : w.name}
              aria-expanded={nodes.length > 0 ? treeOpen : undefined}
              aria-selected={workspaceSelected} aria-current={workspaceSelected ? 'true' : undefined}
              data-tree-key={wKey}
              tabIndex={focusedTreeKey === wKey || (!focusedTreeKey && w === named[0]) ? 0 : -1}
              onClick={() => { void selectWorkspace(w); }}>
            <div
              className={`row ws-row workspace-tree-row ${workspaceSelected ? 'active' : paneSelected ? 'contains-selection' : ''} ${dropPos ? `ws-drop-${dropPos}` : ''} ${summary.hasUnread ? 'ws-unread' : ''}`}
              title={summary.tooltip}
              draggable={w.name !== '_current'}
              onDragStart={(e) => {
                e.dataTransfer.setData(WS_DRAG_MIME, wKey);
                e.dataTransfer.effectAllowed = 'move';
              }}
              onDragOver={(e) => {
                if (!e.dataTransfer.types.includes(WS_DRAG_MIME)) return;
                e.preventDefault();
                e.dataTransfer.dropEffect = 'move';
                const pos = dropPosition(e);
                setDropTarget((prev) =>
                  prev?.key === wKey && prev.pos === pos ? prev : { key: wKey, pos },
                );
              }}
              onDragLeave={(e) => {
                const related = e.relatedTarget as Node | null;
                if (!e.currentTarget.contains(related)) setDropTarget(null);
              }}
              onDrop={(e) => {
                if (!e.dataTransfer.types.includes(WS_DRAG_MIME)) return;
                e.preventDefault();
                const src = e.dataTransfer.getData(WS_DRAG_MIME);
                const pos = dropPosition(e);
                setDropTarget(null);
                if (src && src !== wKey) onReorderWorkspaces(src, wKey, pos);
              }}
              onContextMenu={(e) => {
                e.preventDefault();
                e.stopPropagation();
                setCtxMenu({ x: e.clientX, y: e.clientY, wsName: w.name });
              }}
            >
              {nodes.length > 0 ? <button type="button" className="tree-chevron" tabIndex={-1}
                aria-label={`${treeOpen ? 'Collapse' : 'Expand'} ${w.name}`}
                title={`${treeOpen ? 'Collapse' : 'Expand'} (Alt+click: with everything below)`}
                onClick={e => {
                  e.stopPropagation();
                  if (e.altKey) setBranches([wKey, ...branchKeys(wKey, tree?.roots ?? [])], treeOpen);
                  else toggleTree(wKey);
                }}>{treeOpen ? '▾' : '▸'}</button> : <span className="tree-chevron" aria-hidden />}
              <span className="ws-status">
                <span
                  className={`ws-dot ws-dot-${statusKind} pane-tree-dot is-${treeStatus}`}
                  aria-hidden
                  title={summary.badgeKind ? summary.badgeLabel : summary.count > 0 ? 'running' : 'idle'}
                />
                <span className="ws-drag-handle" aria-hidden>⋮⋮</span>
                {summary.badgeKind && summary.badgeKind !== 'activity' && summary.attentionCount > 0 && (
                  <span
                    className={`ws-badge ws-badge-${summary.badgeKind}`}
                    title={`${summary.attentionCount} pane${summary.attentionCount === 1 ? '' : 's'} need${summary.attentionCount === 1 ? 's' : ''} attention`}
                  >
                    {summary.attentionCount}
                  </span>
                )}
                {summary.flaggedCount > 0 && (
                  <span
                    className="ws-badge ws-badge-flag"
                    title={`${summary.flaggedCount} flagged pane${summary.flaggedCount === 1 ? '' : 's'}:\n${summary.flaggedSummaries.join('\n')}`}
                  >
                    <IconFlag size={11} />{summary.flaggedCount > 1 ? summary.flaggedCount : ''}
                  </span>
                )}
                {w.backend && (
                  <span
                    className={`ws-backend-tag ws-backend-${w.backend}`}
                    title={w.backend === 'wsl'
                      ? 'Linux workspace (WSL backend)'
                      : 'Windows workspace (Windows-native backend)'}
                  >
                    {w.backend === 'wsl' ? 'L' : 'W'}
                  </span>
                )}
              </span>
              {renamingName === w.name ? (
                <input
                  className="ws-rename-input"
                  autoFocus
                  value={renameDraft}
                  onChange={(e) => setRenameDraft(e.target.value)}
                  onBlur={async () => {
                    const next = renameDraft.trim();
                    setRenamingName(null);
                    if (next && next !== w.name) {
                      await onRenameWorkspace(w.name, next);
                    }
                  }}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') (e.target as HTMLInputElement).blur();
                    if (e.key === 'Escape') {
                      setRenameDraft(w.name);
                      setRenamingName(null);
                    }
                    e.stopPropagation();
                  }}
                  onClick={(e) => e.stopPropagation()}
                  onMouseDownCapture={(e) => e.stopPropagation()}
                />
              ) : (
                <span className="ws-text">
                  <span className="ws-title-line">
                    <span
                      className="ws-name"
                      title={w.name === '_current' ? 'Current workspace' : `${w.name} — double-click to rename`}
                      onDoubleClick={(e) => {
                        e.stopPropagation();
                        if (w.name === '_current') return;
                        setRenameDraft(w.name);
                        setRenamingName(w.name);
                      }}
                    >
                      {w.name === '_current' ? 'Current workspace' : w.name}
                    </span>
                    {w.contextPack && (
                      <span className="ws-pack-tag" title={w.contextPack}>pack</span>
                    )}
                    {summary.recencyLabel && (
                      <span
                        className="ws-recency"
                        title={summary.recencyFull}
                        style={summary.mostRecent && rowSummaries.now - summary.mostRecent < 10000 ? { color: 'var(--ok)' } : undefined}
                      >{summary.recencyLabel}</span>
                    )}
                  </span>
                  {metaParts.length > 0 && (
                    <span className="ws-meta" title={summary.tooltip}>
                      {metaParts.join(' · ')}
                    </span>
                  )}
                </span>
              )}
              <button type="button" className="tree-leads-toggle" aria-label={`All panes in ${w.name}`} aria-pressed={allPanes}
                onClick={e => {
                  e.stopPropagation();
                  // Save the setting first so the workspace opens straight
                  // into it instead of switching views after loading.
                  useStore.getState().toggleWorkspaceAllPanes(w.name, w.backend ?? activeBackend);
                  void selectWorkspace(w);
                }}>All panes</button>
              {!w.protected && (
                <button
                  className="iconbtn ws-delete"
                  title="Delete workspace"
                  aria-label="Delete workspace"
                  onClick={(e) => { e.stopPropagation(); void confirmDelete(w.name); }}
                ><IconX size={13} /></button>
              )}
            </div>
            {treeOpen && <div role="group">
              <WorkspacePaneRows nodes={(tree?.roots ?? []).filter(node => node.isTopLevel)} workspaceKey={wKey} collapsed={treeCollapsed} toggle={toggleTree} setBranches={setBranches} onMenu={(paneId, x, y) => setPaneMenu({ paneId, x, y })}
                focusedKey={focusedTreeKey} select={node => { void selectPane(w, node.pane.id, node.children.length > 0 || node.isTopLevel); }} />
              {!!tree?.unattached.length && <div role="group" aria-label="Unattached">
                <div role="presentation" className="pane-tree-unattached">Unattached</div>
                <WorkspacePaneRows nodes={tree.unattached} workspaceKey={wKey} collapsed={treeCollapsed} toggle={toggleTree} setBranches={setBranches} onMenu={(paneId, x, y) => setPaneMenu({ paneId, x, y })}
                  focusedKey={focusedTreeKey} select={node => { void selectPane(w, node.pane.id, node.children.length > 0); }} />
              </div>}
            </div>}
            </div>
          );
        })}
      </div>

      {ctxMenu && (() => {
        const ws = named.find((w) => w.name === ctxMenu.wsName);
        if (!ws) return null;
        const isActive = activeWorkspace === ws.name;
        return (
          <div
            ref={ctxMenuRef}
            className="pane-ctx-menu"
            role="menu"
            style={{ position: 'fixed', left: ctxMenuPos.left, top: ctxMenuPos.top, zIndex: 'var(--z-menu)' }}
          >
            {ws.name !== '_current' && <><button
              role="menuitem"
              onClick={() => {
                setCtxMenu(null);
                setRenameDraft(ws.name);
                setRenamingName(ws.name);
              }}
            >Rename</button>
            <button
              role="menuitem"
              onClick={() => {
                setCtxMenu(null);
                setPackWorkspaceName(ws.name);
              }}
            >Context pack…</button></>}
            {(() => {
              const tree = trees.find(t => workspaceKey(t.workspace) === workspaceKey(ws));
              // Only this daemon's panes can be changed from here.
              const local = !isRouterMode() || !ws.backend || ws.backend === activeBackend;
              return local && tree && leadCandidates(tree.nodes).length > 0 && (
                <button role="menuitem" title="Make several of your own panes here leads at once, each with its role"
                  onClick={() => { setCtxMenu(null); setLeadsWorkspaceKey(workspaceKey(ws)); }}>Make leads…</button>
              );
            })()}
            {isActive && layout && (
              <button
                role="menuitem"
                title="Start a blank layout. Panes keep running; ones no workspace holds appear under Detached sessions."
                onClick={() => {
                  setCtxMenu(null);
                  onClearLayout();
                }}
              >Clear layout</button>
            )}
            {!ws.protected && (
              <>
                <div className="pane-ctx-menu-sep" />
                <button
                  role="menuitem"
                  className="danger"
                  onClick={() => {
                    setCtxMenu(null);
                    void confirmDelete(ws.name);
                  }}
                >Delete</button>
              </>
            )}
          </div>
        );
      })()}
      {paneMenu && panes[paneMenu.paneId] && (
        <TreePaneMenu pane={panes[paneMenu.paneId]} x={paneMenu.x} y={paneMenu.y} onClose={() => setPaneMenu(null)} />
      )}
      {leadsWorkspaceKey && (() => {
        const tree = trees.find(t => workspaceKey(t.workspace) === leadsWorkspaceKey);
        if (!tree) return null;
        const name = tree.workspace.name === '_current' ? 'Current workspace' : tree.workspace.name;
        return <MakeLeadsDialog workspace={name} nodes={tree.nodes} onClose={() => setLeadsWorkspaceKey(null)} />;
      })()}
      {packWorkspaceName && (() => {
        const workspace = named.find((item) => item.name === packWorkspaceName);
        if (!workspace) return null;
        return (
          <ContextPackDialog
            title={`Context pack — ${workspace.name}`}
            value={workspace.contextPack ?? ''}
            onSave={(dir) => onSetWorkspaceContextPack(workspace.name, dir)}
            onClose={() => setPackWorkspaceName(null)}
          />
        );
      })()}
    </>
  );
}
