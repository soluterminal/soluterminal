import { balancedLayout, DEFAULT_LAYOUT_ASPECT, DEFAULT_LAYOUT_COLUMNS } from './autoLayout';
import type { Layout, PaneInfo, Workspace } from '../types';
import { buildNxMFromLeaves, buildPresetLayout, collectLeaves, layoutHasLeaf, leafId, type LayoutPresetId } from './layout';
import { createWorkspacePaneTreeSelector, flattenPaneTree, type PaneTreeNode } from './workspacePaneTree';
import { paneTeamKind } from './paneTeam';

export interface WorkspaceView {
  workspace: string;
  backend: 'wsl' | 'win' | null;
  kind: 'single' | 'team' | 'top-level' | 'all';
  paneId?: string;
  preset?: LayoutPresetId;
  grid?: { rows: number; cols: number };
}
export interface WorkspaceViewState {
  layout: Layout | null;
  workspaceView?: WorkspaceView | null;
  activeWorkspace?: string;
  activeBackend?: 'wsl' | 'win' | null;
  workspaces?: Workspace[];
  panes?: Record<string, PaneInfo>;
  workspaceAllPanes?: Record<string, boolean>;
  paneLayoutMode?: 'auto' | 'free';
  layoutColumns?: number;
  layoutAspect?: number;
}

/** Overview layouts remain editable; only an explicit pane/team selection is temporary. */
export const isTemporaryWorkspaceView = (view?: WorkspaceView | null): boolean => view?.kind === 'single' || view?.kind === 'team';

const viewMatches = (s: WorkspaceViewState, view?: WorkspaceView | null): view is WorkspaceView =>
  !!view && view.workspace === s.activeWorkspace && view.backend === (s.activeBackend ?? null);

/** The stored view, or the workspace overview when none applies. A stored view
 * for another workspace is stale, so every switch path lands on the overview
 * (with its saved All panes setting) in the same render, never the raw layout. */
export function effectiveWorkspaceView(s: WorkspaceViewState): WorkspaceView {
  const workspace = s.activeWorkspace ?? '';
  const backend = s.activeBackend ?? null;
  const stored = viewMatches(s, s.workspaceView) ? s.workspaceView : null;
  if (stored && isTemporaryWorkspaceView(stored)) return stored;
  const kind = s.workspaceAllPanes?.[workspaceViewKey(workspace, backend)] ? 'all' : 'top-level';
  return stored ? { ...stored, kind } : { workspace, backend, kind };
}

// A projected split retains its canonical source so resizing it updates the
// saved ratio without replacing hidden worker branches with the projection.
// Splits built only for the view map to null: resizing one first commits the
// shown arrangement (see commitWorkspaceOverview in the store).
const projectedSplits = new WeakMap<Layout, Layout | null>();
export const canonicalWorkspaceNode = (node: Layout): Layout | null => projectedSplits.has(node) ? projectedSplits.get(node)! : node;
export const isDerivedWorkspaceLayout = (node: Layout | null): boolean =>
  node?.type === 'split' && (canonicalWorkspaceNode(node) === null || isDerivedWorkspaceLayout(node.a) || isDerivedWorkspaceLayout(node.b));
function projectOverview(node: Layout | null, hidden: Set<string>): Layout | null {
  if (!node) return null;
  if (node.type !== 'split') return node.type === 'pane' && hidden.has(node.paneId) ? null : node;
  const a = projectOverview(node.a, hidden), b = projectOverview(node.b, hidden);
  if (!a) return b;
  if (!b) return a;
  if (a === node.a && b === node.b) return node;
  const projected = { ...node, a, b };
  projectedSplits.set(projected, node);
  return projected;
}
function markDerived(node: Layout | null): void {
  if (node?.type !== 'split' || projectedSplits.has(node)) return;
  projectedSplits.set(node, null);
  markDerived(node.a); markDerived(node.b);
}

const selectTrees = createWorkspacePaneTreeSelector();

function workspaceNodes(s: WorkspaceViewState): PaneTreeNode[] {
  if (!s.panes) return [];
  const workspaces = s.workspaces?.some(w => w.name === s.activeWorkspace && (!w.backend || w.backend === s.activeBackend))
    ? s.workspaces : [...(s.workspaces ?? []), { name: s.activeWorkspace!, backend: s.activeBackend ?? undefined, layout: s.layout, paneIds: [] } as unknown as Workspace];
  const tree = selectTrees(workspaces, s.panes, s.activeWorkspace!, s.layout, s.activeBackend).find(t => t.workspace.name === s.activeWorkspace && (!t.workspace.backend || t.workspace.backend === (s.activeBackend ?? null)));
  return tree?.nodes ?? [];
}

/** The top-level (or unattached) root whose subtree contains `paneId`. */
export function workspaceRootOf(s: WorkspaceViewState, paneId: string): string | null {
  const parent = new Map<string, string>();
  const nodes = workspaceNodes(s);
  for (const node of nodes) for (const child of node.children) parent.set(child.pane.id, node.pane.id);
  if (!nodes.some(node => node.pane.id === paneId)) return null;
  let id = paneId;
  while (parent.has(id)) id = parent.get(id)!;
  return id;
}

/** The pane whose team shows paneId: its nearest lead (itself included) or
 * else its top-level ancestor. A sub-lead's worker opens the sub-lead's team. */
export function workspaceLeadOf(s: WorkspaceViewState, paneId: string): string | null {
  const parent = new Map<string, PaneTreeNode>();
  const nodes = workspaceNodes(s);
  for (const node of nodes) for (const child of node.children) parent.set(child.pane.id, node);
  let node = nodes.find(n => n.pane.id === paneId);
  if (!node) return null;
  while (parent.has(node.pane.id) && paneTeamKind(node.pane) !== 'lead') node = parent.get(node.pane.id)!;
  return node.pane.id;
}

export const workspaceViewHasSelection = (s: WorkspaceViewState): boolean => workspaceNodes(s).some(n => n.pane.id === s.workspaceView?.paneId);
/** The overview shows something other than the saved layout (hidden workers,
 * recovered tiles, a view-only preset or an auto-balanced projection). */
export const isFilteredWorkspaceOverview = (s: WorkspaceViewState): boolean =>
  !isTemporaryWorkspaceView(effectiveWorkspaceView(s)) && workspaceViewLayout(s) !== s.layout;

function buildWorkspaceViewLayout(s: WorkspaceViewState, view: WorkspaceView): Layout | null {
  const nodes = workspaceNodes(s);
  const selected = isTemporaryWorkspaceView(view) ? nodes.find(n => n.pane.id === view.paneId) : undefined;
  // A selection that vanished falls back to the overview in the same render.
  if (isTemporaryWorkspaceView(view) && !selected) view = effectiveWorkspaceView({ ...s, workspaceView: null });
  const topLevel = nodes.filter(n => n.isTopLevel);
  const overview = view.kind === 'all' || view.kind === 'top-level';
  const visible = view.kind === 'all' || (view.kind === 'top-level' && !topLevel.length) ? nodes
    : view.kind === 'top-level' ? topLevel
    : selected ? view.kind === 'single' ? [selected] : flattenPaneTree([selected]) : [];
  if (!visible.length) return s.layout;
  if (!overview) {
    const leaves: Layout[] = visible.map(n => ({ type: 'pane', paneId: n.pane.id }));
    const cols = Math.ceil(Math.sqrt(leaves.length));
    const grid = view.grid;
    const result = buildNxMFromLeaves(leaves, grid ? Math.max(grid.rows, Math.ceil(leaves.length / grid.cols)) : Math.ceil(leaves.length / cols), grid?.cols ?? cols);
    return view.preset ? buildPresetLayout(result, view.preset) : result;
  }
  // Hide only panes known to belong elsewhere in the hierarchy. A leaf whose
  // pane is not loaded yet (or a file/browser tile) stays where it was saved.
  const shown = new Set(visible.map(n => n.pane.id));
  const hidden = new Set(nodes.filter(n => !shown.has(n.pane.id)).map(n => n.pane.id));
  let result = projectOverview(s.layout, hidden);
  const included = new Set(collectLeaves(result).map(leafId));
  const missing: Layout[] = visible.filter(n => !included.has(n.pane.id)).map(n => ({ type: 'pane', paneId: n.pane.id }));
  const columns = s.layoutColumns || DEFAULT_LAYOUT_COLUMNS, aspect = s.layoutAspect || DEFAULT_LAYOUT_ASPECT;
  if (view.grid || view.preset || (s.paneLayoutMode === 'auto' && (result !== s.layout || missing.length))) {
    // Every visible tile gets an even share: an auto layout rebalances the
    // projection instead of leaving holes where hidden workers sit.
    const leaves = [...collectLeaves(result), ...missing];
    result = view.grid ? buildNxMFromLeaves(leaves, Math.max(view.grid.rows, Math.ceil(leaves.length / view.grid.cols)), view.grid.cols)
      : balancedLayout(buildNxMFromLeaves(leaves, 1, leaves.length), columns, aspect);
    if (view.preset) result = buildPresetLayout(result, view.preset);
    markDerived(result);
  } else if (missing.length) {
    // A custom layout keeps its own geometry; recovered tiles share one
    // balanced block beside it, sized by tile count.
    const block = balancedLayout(buildNxMFromLeaves(missing, 1, missing.length), columns, aspect)!;
    markDerived(block);
    if (result) {
      const existing = collectLeaves(result).length;
      const wrapper: Layout = { type: 'split', direction: 'horizontal', ratio: existing / (existing + missing.length), a: result, b: block };
      projectedSplits.set(wrapper, null);
      result = wrapper;
    } else result = block;
  }
  return result;
}

// Many selectors run per store update. Memoise on input identity, and keep
// the previous tree when only unrelated pane fields changed, so splits and
// terminals never re-render because a pane's activity timestamp moved.
let memo: { inputs: unknown[]; shape: string; layout: Layout | null; result: Layout | null } | null = null;
/** A presentation only: canonical layout (and therefore autosave) never changes. */
export function workspaceViewLayout(s: WorkspaceViewState): Layout | null {
  if (!s.panes || s.activeWorkspace === undefined) return s.layout;
  const inputs = [s.layout, s.panes, s.workspaces, s.activeWorkspace, s.activeBackend ?? null, s.workspaceView, s.workspaceAllPanes, s.paneLayoutMode, s.layoutColumns, s.layoutAspect];
  if (memo && memo.inputs.every((value, i) => value === inputs[i])) return memo.result;
  const view = effectiveWorkspaceView(s);
  const built = buildWorkspaceViewLayout(s, view);
  const shape = built === s.layout ? '' : JSON.stringify(built);
  const result = memo && memo.layout === s.layout && shape && memo.shape === shape ? memo.result : built;
  memo = { inputs, shape, layout: s.layout, result };
  return result;
}

/** The view that shows `paneId`: undefined when the current view already does
 * (or nothing can), null for the overview, else its root's team view. */
export function workspaceViewForPane(s: WorkspaceViewState, paneId: string): WorkspaceView | null | undefined {
  if (!s.panes || s.activeWorkspace === undefined) return undefined;
  if (layoutHasLeaf(workspaceViewLayout(s), paneId)) return undefined;
  const overview = { ...s, workspaceView: null };
  if (layoutHasLeaf(buildWorkspaceViewLayout(overview, effectiveWorkspaceView(overview)), paneId)) return null;
  const lead = workspaceLeadOf(s, paneId);
  if (!lead) return undefined;
  return { workspace: s.activeWorkspace, backend: s.activeBackend ?? null, kind: 'team', paneId: lead };
}

/** Turn the shown overview into a saved layout. Each hidden leaf sits beside
 * its top-level pane (so All panes keeps workers next to their lead); the
 * rest share one block at the end. Projecting the result back gives exactly
 * the shown geometry, so committing never moves a visible tile. */
export function graftHiddenWorkspaceLeaves(s: WorkspaceViewState, shown: Layout): Layout {
  const copy = (node: Layout): Layout => node.type === 'split' ? { ...node, a: copy(node.a), b: copy(node.b) } : node;
  const shownIds = new Set(collectLeaves(shown).map(leafId));
  const groups = new Map<string, Layout[]>();
  const rest: Layout[] = [];
  for (const leaf of collectLeaves(s.layout)) {
    const id = leafId(leaf);
    if (!id || shownIds.has(id)) continue;
    const root = leaf.type === 'pane' ? workspaceRootOf(s, leaf.paneId) : null;
    if (root && root !== id && shownIds.has(root)) groups.set(root, [...groups.get(root) ?? [], leaf]);
    else rest.push(leaf);
  }
  const graft = (node: Layout): Layout => {
    if (node.type === 'split') return { ...node, a: graft(node.a), b: graft(node.b) };
    const group = groups.get(leafId(node) ?? '');
    return group ? { type: 'split', direction: 'vertical', ratio: 0.5, a: node, b: buildNxMFromLeaves(group, 1, group.length)! } : node;
  };
  const result = graft(copy(shown));
  if (!rest.length) return result;
  const count = shownIds.size;
  return { type: 'split', direction: 'horizontal', ratio: count / (count + rest.length), a: result, b: buildNxMFromLeaves(rest, 1, rest.length)! };
}

const ALL_PANES_KEY = 'solu.workspaceAllPanes.v1';
export const workspaceViewKey = (workspace: string, backend: 'wsl' | 'win' | null) => JSON.stringify([backend, workspace]);
export function readWorkspaceAllPanes(): Record<string, boolean> {
  try {
    const saved: unknown = JSON.parse(localStorage.getItem(ALL_PANES_KEY) || '{}');
    return saved && typeof saved === 'object' && !Array.isArray(saved)
      ? Object.fromEntries(Object.entries(saved).filter(([, value]) => value === true)) : {};
  } catch { return {}; }
}
export function saveWorkspaceAllPanes(value: Record<string, boolean>): void {
  try { localStorage.setItem(ALL_PANES_KEY, JSON.stringify(value)); } catch { /* storage may be unavailable */ }
}
