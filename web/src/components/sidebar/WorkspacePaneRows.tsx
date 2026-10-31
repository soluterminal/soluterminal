import type { PaneTreeNode } from '../../lib/workspacePaneTree';
import { confirmClosePane } from '../usePaneContextMenu';
import { useStore } from '../../store';

interface Props {
  nodes: PaneTreeNode[];
  workspaceKey: string;
  collapsed: Record<string, boolean>;
  toggle: (key: string) => void;
  select: (node: PaneTreeNode) => void;
  focusedKey: string;
  leadsOnly?: boolean;
  depth?: number;
  parentKey?: string;
}

export function WorkspacePaneRows({ nodes, workspaceKey, collapsed, toggle, select, focusedKey, leadsOnly, depth = 2, parentKey = workspaceKey }: Props) {
  const focusedPaneId = useStore(s => s.focusedPaneId);
  const view = useStore(s => s.workspaceView);
  return <>{nodes.map(node => {
    if (leadsOnly && !node.isLead) return <WorkspacePaneRows key={node.pane.id} nodes={node.children}
      workspaceKey={workspaceKey} collapsed={collapsed} toggle={toggle} select={select}
      focusedKey={focusedKey} leadsOnly depth={depth} parentKey={parentKey} />;
    const { pane, children } = node;
    const key = `${workspaceKey}:pane:${pane.id}`;
    const open = !collapsed[key];
    const hasVisibleChildren = leadsOnly ? node.hasLeadDescendants : children.length > 0;
    const { status, needsYou, descendantCount } = node;
    const title = pane.title || pane.cmd.join(' ');
    const finishedWorker = pane.origin?.source === 'agent' && status === 'finished';
    return <div key={pane.id} role="treeitem" aria-level={depth}
      aria-label={title} aria-expanded={hasVisibleChildren ? open : undefined}
      aria-selected={!!view && focusedPaneId === pane.id}
      data-tree-key={key} data-tree-parent={parentKey} tabIndex={focusedKey === key ? 0 : -1}
      onClick={e => { e.stopPropagation(); select(node); }}>
      <div className={`workspace-pane-row${focusedPaneId === pane.id && view ? ' is-selected' : ''}`}
        style={{ paddingLeft: 8 + Math.min(depth - 1, 3) * 12 }} title={title}>
        {hasVisibleChildren ? <button type="button" className="tree-chevron" tabIndex={-1}
          aria-label={`${open ? 'Collapse' : 'Expand'} ${title}`}
          onClick={e => { e.stopPropagation(); toggle(key); }}>{open ? '▾' : '▸'}</button>
          : <span className="tree-chevron" aria-hidden>·</span>}
        <span className={`pane-tree-dot is-${needsYou ? 'needs-you' : status}`} title={needsYou ? 'needs you' : status} />
        <span className="pane-tree-text">
          <span className="pane-tree-title">{title}</span>
          <span className={`pane-tree-status${status === 'needs-you' ? ' needs-you' : ''}`}>
            {status === 'needs-you' ? 'needs you' : status}
            {!open && descendantCount > 0 ? ` · ${descendantCount} worker${descendantCount === 1 ? '' : 's'}` : ''}
          </span>
        </span>
        {node.isLead && <span className="pane-tree-role">lead</span>}
        {finishedWorker && <button type="button" className="iconbtn pane-tree-close" aria-label={`Close ${title}`} title="Close pane"
          onClick={e => {
            e.stopPropagation();
            void confirmClosePane(pane).then(ok => { if (ok) void useStore.getState().killPane(pane.id); });
          }}>×</button>}
      </div>
      {open && hasVisibleChildren && <div role="group">
        <WorkspacePaneRows nodes={children} workspaceKey={workspaceKey} collapsed={collapsed} toggle={toggle} select={select} focusedKey={focusedKey} leadsOnly={leadsOnly} depth={depth + 1} parentKey={key} />
      </div>}
    </div>;
  })}</>;
}
