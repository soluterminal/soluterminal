import { flattenPaneTree, type PaneTreeNode } from '../../lib/workspacePaneTree';
import { confirmClosePane } from '../usePaneContextMenu';
import { useStore } from '../../store';
import { paneLeadRole, paneTeamKind } from '../../lib/paneTeam';
import { paneRowLabel } from '../../lib/paneRowLabel';

interface Props {
  nodes: PaneTreeNode[];
  workspaceKey: string;
  collapsed: Record<string, boolean>;
  toggle: (key: string) => void;
  select: (node: PaneTreeNode) => void;
  focusedKey: string;
  depth?: number;
  parentKey?: string;
}

export function WorkspacePaneRows({ nodes, workspaceKey, collapsed, toggle, select, focusedKey, depth = 2, parentKey = workspaceKey }: Props) {
  const view = useStore(s => s.workspaceView);
  return <>{nodes.map(node => {
    const { pane, children } = node;
    const key = `${workspaceKey}:pane:${pane.id}`;
    const open = !collapsed[key];
    const hasVisibleChildren = children.length > 0;
    const selected = (view?.kind === 'team' || view?.kind === 'single') && view.paneId === pane.id;
    const { status, needsYou, descendantCount } = node;
    const title = pane.title || pane.cmd.join(' ');
    const label = paneRowLabel(pane, title);
    const finishedWorker = paneTeamKind(pane) === 'worker' && status === 'finished';
    const leadRole = paneLeadRole(pane);
    const below = descendantCount > 0 ? flattenPaneTree(children) : [];
    const subLeads = below.filter(n => paneTeamKind(n.pane) === 'lead').length;
    const workers = below.length - subLeads;
    return <div key={pane.id} role="treeitem" aria-level={depth}
      aria-label={label.text} aria-expanded={hasVisibleChildren ? open : undefined}
      aria-selected={selected} aria-current={selected ? 'true' : undefined}
      data-tree-key={key} data-tree-parent={parentKey} tabIndex={focusedKey === key ? 0 : -1}
      onClick={e => { e.stopPropagation(); select(node); }}>
      <div className={`workspace-pane-row${selected ? ' is-selected' : ''}`}
        style={{ paddingLeft: 8 + Math.min(depth - 1, 3) * 12 }} title={label.tooltip}>
        {hasVisibleChildren ? <button type="button" className="tree-chevron" tabIndex={-1}
          aria-label={`${open ? 'Collapse' : 'Expand'} ${label.text}`}
          onClick={e => { e.stopPropagation(); toggle(key); }}>{open ? '▾' : '▸'}</button>
          : <span className="tree-chevron" aria-hidden>·</span>}
        <span className={`pane-tree-dot is-${needsYou ? 'needs-you' : status}`} title={needsYou ? 'needs you' : status} />
        <span className="pane-tree-text">
          <span className="pane-tree-title-line">
            {label.handle ? <span className="pane-tree-title">
              <span className="pane-tree-handle">{label.handle}</span>
              {label.detail && <span className="pane-tree-title-detail"> {label.detail}</span>}
            </span> : <span className="pane-tree-title">{title}</span>}
            {leadRole && <span className="pane-tree-lead-pill" title={`Lead · ${leadRole}. Never closed automatically.`}>lead · {leadRole}</span>}
          </span>
          <span className={`pane-tree-status${status === 'needs-you' ? ' needs-you' : ''}`}>
            {status === 'needs-you' ? 'needs you' : status}
            {workers > 0 ? ` · ${workers} worker${workers === 1 ? '' : 's'}` : ''}
            {subLeads > 0 ? ` · ${subLeads} lead${subLeads === 1 ? '' : 's'}` : ''}
          </span>
        </span>
        <button type="button" className={`iconbtn pane-tree-close${finishedWorker ? ' is-pinned' : ''}`} tabIndex={-1} aria-label={`Close ${label.text}`} title="Close pane"
          onClick={e => {
            e.stopPropagation();
            void confirmClosePane(pane).then(ok => { if (ok) void useStore.getState().killPane(pane.id); });
          }}>×</button>
      </div>
      {open && hasVisibleChildren && <div role="group">
        <WorkspacePaneRows nodes={children} workspaceKey={workspaceKey} collapsed={collapsed} toggle={toggle} select={select} focusedKey={focusedKey} depth={depth + 1} parentKey={key} />
      </div>}
    </div>;
  })}</>;
}
