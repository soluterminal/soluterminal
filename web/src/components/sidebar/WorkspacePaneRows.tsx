import type { MouseEvent as ReactMouseEvent } from 'react';
import { flattenPaneTree, type PaneTreeNode } from '../../lib/workspacePaneTree';
import { confirmClosePane } from '../usePaneContextMenu';
import { useStore } from '../../store';
import { paneLeadRole, paneTeamKind } from '../../lib/paneTeam';
import { paneRowLabel } from '../../lib/paneRowLabel';

/** Indent per tree level; it stops growing past MAX_INDENT_LEVELS so a deep
 *  chain cannot push titles out of a narrow sidebar (the guides still show it). */
export const TREE_INDENT_PX = 14;
const MAX_INDENT_LEVELS = 6;
const ROW_INSET_PX = 3;

/** Left padding of a row at an aria-level (the workspace row is level 1). */
export function treeRowInset(depth: number): number {
  return ROW_INSET_PX + Math.min(depth - 1, MAX_INDENT_LEVELS) * TREE_INDENT_PX;
}

/** Tree key of a pane row, as stored in the collapsed map. */
export function paneTreeKey(workspaceKey: string, paneId: string): string {
  return `${workspaceKey}:pane:${paneId}`;
}

/** A top-level pane's workers start folded (a lead's team stays out of the
 *  way until asked for); deeper branches and workspaces start open. */
export function foldedByDefault(node: PaneTreeNode): boolean {
  return node.isTopLevel && node.children.length > 0;
}

/** Whether a branch is folded: the person's choice when they made one. */
export function isFolded(folded: Record<string, boolean>, key: string, byDefault: boolean): boolean {
  return typeof folded[key] === 'boolean' ? folded[key] : byDefault;
}

/** Keys of `nodes` and every node below them that has children. */
export function branchKeys(workspaceKey: string, nodes: PaneTreeNode[]): string[] {
  return flattenPaneTree(nodes).filter(node => node.children.length > 0).map(node => paneTreeKey(workspaceKey, node.pane.id));
}

interface Props {
  nodes: PaneTreeNode[];
  workspaceKey: string;
  collapsed: Record<string, boolean>;
  toggle: (key: string) => void;
  /** Alt+click on a chevron: open or close a branch together with everything below it. */
  setBranches: (keys: string[], collapsed: boolean) => void;
  select: (node: PaneTreeNode) => void;
  /** Right-click on a row: open its pane menu at the pointer. */
  onMenu: (paneId: string, x: number, y: number) => void;
  focusedKey: string;
  depth?: number;
  parentKey?: string;
}

export function WorkspacePaneRows({ nodes, workspaceKey, collapsed, toggle, setBranches, select, onMenu, focusedKey, depth = 2, parentKey = workspaceKey }: Props) {
  const view = useStore(s => s.workspaceView);
  return <>{nodes.map(node => {
    const { pane, children } = node;
    const key = paneTreeKey(workspaceKey, pane.id);
    const open = !isFolded(collapsed, key, foldedByDefault(node));
    const hasVisibleChildren = children.length > 0;
    const selected = (view?.kind === 'team' || view?.kind === 'single') && view.paneId === pane.id;
    const { status, needsYouBelow, descendantCount } = node;
    const title = pane.title || pane.cmd.join(' ');
    const label = paneRowLabel(pane, title);
    const finishedWorker = paneTeamKind(pane) === 'worker' && status === 'finished';
    const leadRole = paneLeadRole(pane);
    // "@cmo · cmo" next to the CMO chip says the role twice.
    const detail = leadRole && label.detail.trim().toLowerCase() === leadRole.trim().toLowerCase() ? '' : label.detail;
    const below = descendantCount > 0 ? flattenPaneTree(children) : [];
    const subLeads = below.filter(n => paneTeamKind(n.pane) === 'lead').length;
    const workers = below.length - subLeads;
    // What it is doing now beats what it was named for; both are in the tooltip.
    const description = pane.identity?.workingOn?.trim() || (label.handle ? detail : '');
    const counts = [workers > 0 ? `${workers} worker${workers === 1 ? '' : 's'}` : '', subLeads > 0 ? `${subLeads} lead${subLeads === 1 ? '' : 's'}` : ''].filter(Boolean);
    const shortCounts = [workers > 0 ? `${workers}w` : '', subLeads > 0 ? `${subLeads}L` : ''].filter(Boolean).join(' · ');
    const statusId = `tree-status-${encodeURIComponent(key)}`;
    // Counts stand in for a folded branch's rows; an open one shows them.
    const hasMeta = !open && (needsYouBelow > 0 || shortCounts !== '');
    // Solid: this pane needs you. Ring: only something below it does.
    const dot = status === 'needs-you' ? 'needs-you' : needsYouBelow > 0 ? 'needs-you-below' : status;
    const dotTitle = status === 'needs-you' ? 'needs you'
      : needsYouBelow > 0 ? `${status} · ${needsYouBelow} below need${needsYouBelow === 1 ? 's' : ''} you` : status;
    const onChevron = (e: ReactMouseEvent) => {
      e.stopPropagation();
      if (e.altKey) setBranches([key, ...branchKeys(workspaceKey, children)], open);
      else toggle(key);
    };
    return <div key={pane.id} role="treeitem" aria-level={depth}
      aria-label={label.text} aria-expanded={hasVisibleChildren ? open : undefined}
      aria-selected={selected} aria-current={selected ? 'true' : undefined} aria-describedby={statusId}
      data-tree-key={key} data-tree-parent={parentKey} tabIndex={focusedKey === key ? 0 : -1}
      onClick={e => { e.stopPropagation(); select(node); }}
      onContextMenu={e => { e.preventDefault(); e.stopPropagation(); onMenu(pane.id, e.clientX, e.clientY); }}>
      <div className={`workspace-pane-row${selected ? ' is-selected' : ''}${leadRole ? ' is-lead' : ''}${finishedWorker ? ' has-pinned-close' : ''}`}
        style={{ paddingLeft: treeRowInset(depth) }} title={node.startedBy ? `${label.tooltip}\nStarted by ${node.startedBy}` : label.tooltip}>
        {hasVisibleChildren ? <button type="button" className="tree-chevron" tabIndex={-1}
          aria-label={`${open ? 'Collapse' : 'Expand'} ${label.text}`}
          title={`${open ? 'Collapse' : 'Expand'} (Alt+click: with everything below)`}
          onClick={onChevron}>{open ? '▾' : '▸'}</button>
          : <span className="tree-chevron" aria-hidden />}
        <span className={`pane-tree-dot is-${dot}`} title={dotTitle} />
        <span className="pane-tree-text">
          <span className="pane-tree-title-line">
            <span className="pane-tree-title">
              {label.handle ? <span className="pane-tree-handle">{label.handle}</span> : title}
            </span>
            {leadRole && <span className="pane-tree-role-chip" title={`Lead · ${leadRole}. Never closed automatically.`}>{leadRole}</span>}
          </span>
          {/* Below the handle in a narrow sidebar, after it in a wide one
              (styles.css); a folded branch's counts end this line so they
              never take width from the handle. */}
          {(description || status === 'needs-you' || hasMeta) && <span className="pane-tree-sub">
            {(description || status === 'needs-you') && <span className="pane-tree-desc">
              {status === 'needs-you' && <span className="pane-tree-needs-you">needs you</span>}
              {status === 'needs-you' && description && ' · '}
              {description}
            </span>}
            {hasMeta && <span className="pane-tree-meta">
              {needsYouBelow > 0 && <span className="pane-tree-attn-count"
                title={`${needsYouBelow} pane${needsYouBelow === 1 ? '' : 's'} below need${needsYouBelow === 1 ? 's' : ''} you`}>{needsYouBelow}●</span>}
              {shortCounts && <span className="pane-tree-count" title={counts.join(' · ')}>{shortCounts}</span>}
            </span>}
          </span>}
          {/* The dot shows the state; words are for screen readers (aria-describedby). */}
          <span id={statusId} className="pane-tree-status sr-only">
            {[status === 'needs-you' ? 'needs you' : status, ...counts, node.startedBy ? `started by ${node.startedBy}` : ''].filter(Boolean).join(' · ')}
          </span>
        </span>
        <button type="button" className={`iconbtn pane-tree-close${finishedWorker ? ' is-pinned' : ''}`} tabIndex={-1} aria-label={`Close ${label.text}`} title="Close pane"
          onClick={e => {
            e.stopPropagation();
            void confirmClosePane(pane).then(ok => { if (ok) void useStore.getState().killPane(pane.id); });
          }}>×</button>
      </div>
      {open && hasVisibleChildren && <div role="group" className="pane-tree-children"
        style={{ ['--tree-guide-x' as string]: `${treeRowInset(depth) + 11}px` }}>
        <WorkspacePaneRows nodes={children} workspaceKey={workspaceKey} collapsed={collapsed} toggle={toggle} setBranches={setBranches} select={select} onMenu={onMenu} focusedKey={focusedKey} depth={depth + 1} parentKey={key} />
      </div>}
    </div>;
  })}</>;
}
