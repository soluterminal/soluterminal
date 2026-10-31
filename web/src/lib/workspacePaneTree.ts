import type { Layout, PaneInfo, Workspace } from '../types';
import { paneIdsInOrder } from './layout';
import { paneRowStatus, type PaneRowStatus } from './paneStatus';

export interface PaneTreeNode {
  pane: PaneInfo;
  children: PaneTreeNode[];
  isLead: boolean;
  status: PaneRowStatus;
  needsYou: boolean;
  descendantCount: number;
  hasLeadDescendants: boolean;
}
export interface WorkspacePaneTree {
  workspace: Workspace;
  roots: PaneTreeNode[];
  paneIds: string[];
  nodes: PaneTreeNode[];
  status: PaneRowStatus;
  hasLeads: boolean;
}

/** Immediate starters take precedence over the original lead. A closed starter
 * falls back to the surviving lead. Explicit workspace membership always wins
 * over ancestry, so moving a worker never drags it back to its old workspace. */
export function buildWorkspacePaneTrees(
  workspaces: Workspace[], panes: Record<string, PaneInfo>,
  activeWorkspace: string, layout: Layout | null, activeBackend: 'wsl' | 'win' | null = null,
): WorkspacePaneTree[] {
  panes = withoutRemovedWorkspacePanes(workspaces, panes, activeBackend);
  const list = Object.values(panes).filter(p => p.role !== 'task_owner');
  const parents = new Map<string, string>();
  for (const p of list) {
    if (p.origin?.source !== 'agent') continue;
    const id = [p.origin.byPaneId, p.origin.leadPaneId].find(id => id && id !== p.id && panes[id] && panes[id].role !== 'task_owner');
    if (id) parents.set(p.id, id);
  }
  // Bad/legacy metadata must never hide a cycle or recurse forever.
  for (const p of list) {
    const seen = new Set([p.id]);
    let id = parents.get(p.id);
    while (id) {
      if (seen.has(id)) { parents.delete(p.id); break; }
      seen.add(id);
      id = parents.get(id);
    }
  }
  const trees: WorkspacePaneTree[] = workspaces.map(workspace => ({ workspace, roots: [], paneIds: [], nodes: [], status: 'idle', hasLeads: false }));
  const owner = new Map<string, number>();
  trees.forEach(({ workspace: w }, i) => {
    const active = w.name === activeWorkspace && (!w.backend || w.backend === activeBackend);
    const ids = active && layout ? paneIdsInOrder(layout) : [...paneIdsInOrder(w.layout), ...(w.paneIds ?? [])];
    for (const id of ids) if (panes[id] && (!panes[id].backend || !w.backend || panes[id].backend === w.backend) && !owner.has(id)) owner.set(id, i);
  });
  const resolveOwner = (id: string): number | undefined => {
    if (owner.has(id)) return owner.get(id);
    const parent = parents.get(id);
    const inherited = parent ? resolveOwner(parent) : undefined;
    if (inherited !== undefined) { owner.set(id, inherited); return inherited; }
    return undefined;
  };
  // Recover workers absent from every layout (including workers of workers).
  for (const p of list) resolveOwner(p.id);
  for (const p of list) {
    if (owner.has(p.id) || p.origin?.source !== 'agent') continue;
    const i = trees.findIndex(t => t.workspace.name === p.origin?.workspace && (!t.workspace.backend || !p.backend || t.workspace.backend === p.backend));
    if (i >= 0) owner.set(p.id, i);
  }
  for (const p of list) resolveOwner(p.id);
  const nodes = new Map(list.map(pane => [pane.id, { pane, children: [], isLead: false, status: paneRowStatus(pane), needsYou: false, descendantCount: 0, hasLeadDescendants: false } as PaneTreeNode]));
  for (const p of list) {
    const i = owner.get(p.id);
    if (i === undefined) continue;
    const node = nodes.get(p.id)!;
    const parentId = parents.get(p.id);
    const parent = parentId && owner.get(parentId) === i ? nodes.get(parentId) : undefined;
    if (parent) parent.children.push(node);
    else trees[i].roots.push(node);
    trees[i].paneIds.push(p.id);
  }
  // A role label or historical origin alone does not make a lead. The pane
  // must have started a currently running child, even if that child moved.
  for (const [child, parent] of parents) if (panes[child]?.status === 'running' && nodes.has(parent)) nodes.get(parent)!.isLead = true;
  const summarize = (node: PaneTreeNode) => {
    node.children.forEach(summarize);
    node.descendantCount = node.children.reduce((count, child) => count + child.descendantCount + 1, 0);
    node.needsYou = node.status === 'needs-you' || node.children.some(child => child.needsYou);
    node.hasLeadDescendants = node.children.some(child => child.isLead || child.hasLeadDescendants);
  };
  for (const tree of trees) {
    tree.roots.forEach(summarize);
    tree.nodes = flattenPaneTree(tree.roots);
    tree.hasLeads = tree.nodes.some(node => node.isLead);
    tree.status = tree.roots.some(node => node.needsYou) ? 'needs-you'
      : tree.nodes.some(node => node.status === 'busy') ? 'busy'
      : tree.nodes.length > 0 && tree.nodes.every(node => node.status === 'finished') ? 'finished' : 'idle';
  }
  return trees;
}

export function flattenPaneTree(roots: PaneTreeNode[]): PaneTreeNode[] {
  return roots.flatMap(node => [node, ...flattenPaneTree(node.children)]);
}


/** Server tombstones win over late pane snapshots, including origin recovery.
 * Keep backend identity: another daemon may have an unrelated pane with this ID. */
export function withoutRemovedWorkspacePanes(workspaces: Workspace[], panes: Record<string, PaneInfo>, activeBackend: 'wsl' | 'win' | null = null): Record<string, PaneInfo> {
  let visible = panes;
  for (const workspace of workspaces) for (const id of workspace.removedPaneIds ?? []) {
    const pane = visible[id];
    if (!pane) continue;
    const backend = pane.backend ?? activeBackend;
    if (workspace.backend && backend && workspace.backend !== backend) continue;
    if (visible === panes) visible = { ...panes };
    delete visible[id];
  }
  return visible;
}

/** A selector per consumer: split geometry and save timestamps do not change
 * membership or statuses. Preserve the tree and all computed summaries across
 * drag frames, but invalidate on pane updates, membership, tombstones or scope. */
export function createWorkspacePaneTreeSelector() {
  let previousPanes: Record<string, PaneInfo> | undefined;
  let previousKey: string | undefined;
  let previousTrees: WorkspacePaneTree[] = [];
  return (workspaces: Workspace[], panes: Record<string, PaneInfo>, activeWorkspace: string, layout: Layout | null, activeBackend: 'wsl' | 'win' | null = null): WorkspacePaneTree[] => {
    const key = JSON.stringify([activeWorkspace, activeBackend, layout ? paneIdsInOrder(layout) : null,
      workspaces.map(w => [w.name, w.backend, w.paneIds, w.removedPaneIds, paneIdsInOrder(w.layout)])]);
    if (previousPanes === panes && previousKey === key) return previousTrees;
    previousPanes = panes;
    previousKey = key;
    previousTrees = buildWorkspacePaneTrees(workspaces, panes, activeWorkspace, layout, activeBackend);
    return previousTrees;
  };
}
