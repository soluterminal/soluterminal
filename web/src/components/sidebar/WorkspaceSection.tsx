import { useMemo, useState, type Dispatch, type DragEvent as ReactDragEvent, type MutableRefObject, type RefObject, type SetStateAction } from 'react';
import { paneUnread } from '../../lib/turnChip';
import { isRouterMode } from '../../dualBackend';
import { paneIdsInOrder } from '../../lib/layout';
import type { Layout, PaneInfo, Workspace } from '../../types';
import { useAttentionBadge } from '../AttentionInbox';
import { useIsMobileView } from '../useIsMobileView';
import { SectionHeader } from './SectionHeader';
import { IconLayers, IconPlus, IconX, IconFlag } from '../ui/icons';
import { relativeTime } from './time';
import { confirmDialog } from '../../lib/dialog';
import { useClampedMenuPosition } from '../useClampedMenuPosition';
import { ContextPackDialog } from '../ContextPackDialog';

const WS_DRAG_MIME = 'application/x-solu-workspace';

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
  onLoadWorkspace: (workspace: Workspace) => void;
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
    for (const w of named) {
      let count = 0;
      let mostRecent = 0;
      let badgeKind: WorkspaceRowSummary['badgeKind'] = null;
      let attentionCount = 0;
      const unreadPanes: string[] = [];
      const flaggedSummaries: string[] = [];
      const viewedAt = wsLastViewedMs[w.name] ?? 0;

      // The active workspace's saved paneIds trail the live layout until the
      // next autosave lands; count what is on screen instead.
      const active = activeWorkspace === w.name && (!isRouterMode() || !w.backend || w.backend === activeBackend);
      const ids = active && layout ? paneIdsInOrder(layout) : w.paneIds;
      for (const id of ids) {
        const p = panes[id];
        if (!p) continue;
        if (p.status === 'running') count++;
        const lastActivity = p.lastActivityMs ?? 0;
        if (lastActivity > mostRecent) mostRecent = lastActivity;

        if (p.status === 'running') {
          let flagged = false;
          if (p.state === 'choice-prompt') {
            badgeKind = 'choice';
            flagged = true;
            attentionCount++;
          } else if (p.state === 'awaiting-input') {
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
  }, [focusedPaneId, named, panes, wsLastViewedMs, activeWorkspace, activeBackend, layout]);
  return (
    <>
      <div
        className="sidebar-section sidebar-section-fill"
        // No height:100% — the card shrink-wraps to its rows (flex-basis
        // auto within the zone) and ends flush with the last one, like the
        // middle-zone is-fill slots; a longer list shrinks to the zone and
        // scrolls internally (min-height 0 + overflow auto).
        style={{ minHeight: 0, overflow: 'auto' }}
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
          const dropPos = dropTarget?.key === wKey ? dropTarget.pos : null;
          const metaParts = [
            `${summary.count} pane${summary.count === 1 ? '' : 's'}`,
            summary.recencyLabel ? summary.recencyFull : null,
            summary.badgeKind === 'awaiting' ? 'waiting' : null,
            summary.badgeKind === 'choice' ? 'approval needed' : null,
            summary.flaggedCount > 0 ? `${summary.flaggedCount} flagged` : null,
          ].filter(Boolean);
          return (
            <div
              key={wKey}
              className={`row ws-row ${isActive ? 'active' : ''} ${dropPos ? `ws-drop-${dropPos}` : ''} ${summary.hasUnread ? 'ws-unread' : ''}`}
              title={summary.tooltip}
              onClick={() => onLoadWorkspace(w)}
              draggable
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
              <span className="ws-status">
                <span
                  className={`ws-dot ws-dot-${statusKind}`}
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
                      title={`${w.name} — double-click to rename`}
                      onDoubleClick={(e) => {
                        e.stopPropagation();
                        setRenameDraft(w.name);
                        setRenamingName(w.name);
                      }}
                    >
                      {w.name}
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
              {!w.protected && (
                <button
                  className="iconbtn ws-delete"
                  title="Delete workspace"
                  aria-label="Delete workspace"
                  onClick={(e) => { e.stopPropagation(); void confirmDelete(w.name); }}
                ><IconX size={13} /></button>
              )}
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
            <button
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
            >Context pack…</button>
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
