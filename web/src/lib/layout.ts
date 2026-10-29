import type { Layout } from '../types';
import type { DropZone } from './paneDnd';

export interface InsertLeafOptions {
  rebalance?: boolean;
}

export function leafId(n: Layout): string | null {
  if (n.type === 'pane') return n.paneId;
  if (n.type === 'browser') return n.browserId;
  if (n.type === 'file') return n.fileId;
  if (n.type === 'diff') return n.diffId;
  if (n.type === 'tasks') return n.tasksId;
  return null;
}

export function layoutHasLeaf(layout: Layout | null, id: string | null): boolean {
  if (!layout || !id) return false;
  if (leafId(layout) === id) return true;
  if (layout.type !== 'split') return false;
  return layoutHasLeaf(layout.a, id) || layoutHasLeaf(layout.b, id);
}

export function insertPane(layout: Layout | null, paneId: string, direction: 'horizontal' | 'vertical' | null, target: string | null): Layout {
  return insertLeaf(layout, { type: 'pane', paneId }, direction, target);
}

export function insertLeaf(
  layout: Layout | null,
  leaf: Layout,
  direction: 'horizontal' | 'vertical' | null,
  target: string | null,
  options?: InsertLeafOptions,
): Layout {
  if (!layout || !target || !direction) return leaf;
  const next = mapLayout(layout, (node) => {
    if (leafId(node) === target) {
      return {
        type: 'split',
        direction,
        ratio: 0.5,
        a: node,
        b: leaf,
      };
    }
    return node;
  });
  return options?.rebalance ? (rebalanceLayout(next) ?? next) : next;
}

// Check whether a node or its children contain a browser leaf.
export function containsBrowserLeaf(node: Layout | null): boolean {
  if (!node) return false;
  if (node.type === 'browser') return true;
  if (node.type !== 'split') return false;
  return containsBrowserLeaf(node.a) || containsBrowserLeaf(node.b);
}

// Count leaves along a specific split dimension. Orthogonal splits count as 1 slot.
export function dimensionLeafCount(node: Layout | null, direction: 'horizontal' | 'vertical'): number {
  if (!node) return 0;
  if (node.type !== 'split') return 1;
  if (node.direction !== direction) return 1;
  return dimensionLeafCount(node.a, direction) + dimensionLeafCount(node.b, direction);
}

// Rebalance split ratios so that leaves along the same direction receive equal shares.
export function rebalanceLayout(layout: Layout | null): Layout | null {
  if (!layout) return null;
  if (layout.type !== 'split') return layout;

  const a = rebalanceLayout(layout.a)!;
  const b = rebalanceLayout(layout.b)!;

  // Browser splits maintain deliberate user/default width (e.g. 0.6/0.4).
  if (containsBrowserLeaf(layout.a) || containsBrowserLeaf(layout.b)) {
    return { ...layout, a, b };
  }

  const countA = dimensionLeafCount(a, layout.direction);
  const countB = dimensionLeafCount(b, layout.direction);
  const total = countA + countB;
  const ratio = total > 0 ? countA / total : 0.5;

  return {
    ...layout,
    ratio,
    a,
    b,
  };
}

// Calculate the fractional share (0..1) of each leaf in the layout along the split chain.
export function computeLeafShares(layout: Layout | null): Map<string, number> {
  const shares = new Map<string, number>();
  if (!layout) return shares;

  function walk(node: Layout, weight: number) {
    if (node.type !== 'split') {
      const id = leafId(node);
      if (id) shares.set(id, weight);
      return;
    }
    walk(node.a, weight * node.ratio);
    walk(node.b, weight * (1 - node.ratio));
  }

  walk(layout, 1.0);
  return shares;
}

// movePaneInLayout handles drag-to-move semantics:
//   center -> swap source and target positions
//   top/bottom/left/right -> detach source, then insert as a new split
//   relative to target (left/top = source first, right/bottom = source second)
export function movePaneInLayoutImpl(root: Layout, source: string, target: string, zone: DropZone): Layout {
  if (source === target) return root;
  if (zone === 'center') return swapLeaves(root, source, target);

  // Capture the source leaf before removing (so we preserve its type -
  // could be a pane or a browser).
  let sourceLeaf: Layout | null = null;
  mapLayout(root, (n) => {
    if (leafId(n) === source) sourceLeaf = n;
    return n;
  });
  if (!sourceLeaf) return root;

  const withoutSource = removePane(root, source);
  if (!withoutSource) return root;
  const direction: 'horizontal' | 'vertical' =
    zone === 'left' || zone === 'right' ? 'horizontal' : 'vertical';
  const sourceFirst = zone === 'left' || zone === 'top';
  return mapLayout(withoutSource, (node) => {
    if (leafId(node) === target) {
      return {
        type: 'split',
        direction,
        ratio: 0.5,
        a: sourceFirst ? sourceLeaf! : node,
        b: sourceFirst ? node : sourceLeaf!,
      };
    }
    return node;
  });
}

function swapLeaves(layout: Layout, aId: string, bId: string): Layout {
  // Two-pass swap: first pass finds both leaves, second applies.
  let leafA: Layout | null = null;
  let leafB: Layout | null = null;
  mapLayout(layout, (n) => {
    if (leafId(n) === aId) leafA = n;
    if (leafId(n) === bId) leafB = n;
    return n;
  });
  if (!leafA || !leafB) return layout;
  return mapLayout(layout, (n) => {
    if (leafId(n) === aId) return leafB!;
    if (leafId(n) === bId) return leafA!;
    return n;
  });
}

export function remapLayoutIds(layout: Layout, idRemap: Map<string, string>): Layout {
  if (idRemap.size === 0) return layout;
  return mapLayout(layout, (n) => {
    if (n.type === 'pane') {
      const mapped = idRemap.get(n.paneId);
      if (mapped) return { type: 'pane', paneId: mapped };
    }
    return n;
  });
}

export function mapLayout(layout: Layout, fn: (n: Layout) => Layout): Layout {
  const mapped = fn(layout);
  if (mapped !== layout) return mapped;
  if (layout.type === 'split') {
    const a = mapLayout(layout.a, fn);
    const b = mapLayout(layout.b, fn);
    if (a !== layout.a || b !== layout.b) return { ...layout, a, b };
  }
  return layout;
}

// Removes a leaf (pane OR browser/file) by id. Named removePane for
// historical continuity; the actual search matches any leaf via leafId().
export function removePane(layout: Layout | null, id: string): Layout | null {
  if (!layout) return null;
  if (layout.type !== 'split') {
    return leafId(layout) === id ? null : layout;
  }
  const a = removePane(layout.a, id);
  const b = removePane(layout.b, id);
  if (!a && !b) return null;
  if (!a) return b;
  if (!b) return a;
  return { ...layout, a, b };
}

export function collectPaneIds(layout: Layout | null): string[] {
  if (!layout) return [];
  if (layout.type === 'pane') return [layout.paneId];
  if (layout.type !== 'split') return [];
  return [...collectPaneIds(layout.a), ...collectPaneIds(layout.b)];
}

export function collectBrowserIds(layout: Layout | null): string[] {
  if (!layout) return [];
  if (layout.type === 'browser') return [layout.browserId];
  if (layout.type !== 'split') return [];
  return [...collectBrowserIds(layout.a), ...collectBrowserIds(layout.b)];
}

export function collectFileIds(layout: Layout | null): string[] {
  if (!layout) return [];
  if (layout.type === 'file') return [layout.fileId];
  if (layout.type !== 'split') return [];
  return [...collectFileIds(layout.a), ...collectFileIds(layout.b)];
}

export function collectDiffIds(layout: Layout | null): string[] {
  if (!layout) return [];
  if (layout.type === 'diff') return [layout.diffId];
  if (layout.type !== 'split') return [];
  return [...collectDiffIds(layout.a), ...collectDiffIds(layout.b)];
}

export function collectTasksIds(layout: Layout | null): string[] {
  if (!layout) return [];
  if (layout.type === 'tasks') return [layout.tasksId];
  if (layout.type !== 'split') return [];
  return [...collectTasksIds(layout.a), ...collectTasksIds(layout.b)];
}

// Patch a diff leaf in place (e.g. flip the staged toggle). Returns the same
// layout reference when nothing changed so callers can skip a re-render.
export function updateDiffLeaf(
  layout: Layout | null,
  diffId: string,
  patch: Partial<{ staged: boolean; path: string; turn: { paneId: string; n: number } | undefined }>,
): Layout | null {
  if (!layout) return layout;
  if (layout.type === 'diff') {
    return layout.diffId === diffId ? { ...layout, ...patch } : layout;
  }
  if (layout.type !== 'split') return layout;
  const a = updateDiffLeaf(layout.a, diffId, patch);
  const b = updateDiffLeaf(layout.b, diffId, patch);
  if (a === layout.a && b === layout.b) return layout;
  return { ...layout, a: a as Layout, b: b as Layout };
}

export function updateTasksLeaf(
  layout: Layout | null,
  tasksId: string,
  patch: Partial<{ selectedTaskId: string | null; composing: boolean }>,
): Layout | null {
  if (!layout) return layout;
  if (layout.type === 'tasks') {
    return layout.tasksId === tasksId ? { ...layout, ...patch } : layout;
  }
  if (layout.type !== 'split') return layout;
  const a = updateTasksLeaf(layout.a, tasksId, patch);
  const b = updateTasksLeaf(layout.b, tasksId, patch);
  if (a === layout.a && b === layout.b) return layout;
  return { ...layout, a: a as Layout, b: b as Layout };
}

// Same as collectPaneIds but exposed for external use (keyboard shortcuts).
export function paneIdsInOrder(layout: Layout | null): string[] {
  return collectPaneIds(layout);
}

// Total number of leaves (panes + browsers + files) in the subtree.
export function leafCount(layout: Layout | null): number {
  if (!layout) return 0;
  if (layout.type !== 'split') return 1;
  return leafCount(layout.a) + leafCount(layout.b);
}

// Flat ordered list of leaves in the layout. Preserves leaf variant
// (pane / browser / file) so preset rebuilds don't lose non-pane tiles.
export function collectLeaves(layout: Layout | null): Layout[] {
  if (!layout) return [];
  if (layout.type !== 'split') return [layout];
  return [...collectLeaves(layout.a), ...collectLeaves(layout.b)];
}

export type LayoutPresetId =
  | 'cols'
  | 'rows'
  | 'grid'
  | 'left-big';

// Stack of leaves into a single chain of splits in one direction.
function stackTree(leaves: Layout[], direction: 'horizontal' | 'vertical'): Layout {
  if (leaves.length === 1) return leaves[0];
  const a = leaves[0];
  const rest = stackTree(leaves.slice(1), direction);
  return {
    type: 'split',
    direction,
    ratio: 1 / leaves.length,
    a,
    b: rest,
  };
}

export function buildPresetLayout(
  layout: Layout | null,
  preset: LayoutPresetId,
): Layout | null {
  const leaves = collectLeaves(layout);
  if (leaves.length === 0) return null;
  if (leaves.length === 1) return leaves[0];

  switch (preset) {
    case 'cols':
      return stackTree(leaves, 'horizontal');
    case 'rows':
      return stackTree(leaves, 'vertical');
    case 'left-big': {
      if (leaves.length === 2) {
        return { type: 'split', direction: 'horizontal', ratio: 0.5, a: leaves[0], b: leaves[1] };
      }
      const right = stackTree(leaves.slice(1), 'vertical');
      return { type: 'split', direction: 'horizontal', ratio: 0.5, a: leaves[0], b: right };
    }
    case 'grid':
    default: {
      const n = leaves.length;
      const cols = Math.ceil(Math.sqrt(n));
      const rows = Math.ceil(n / cols);
      return buildNxMFromLeaves(leaves, rows, cols);
    }
  }
}

// Build an explicit rows x cols layout from leaves, column-major.
export function buildNxMFromLeaves(leaves: Layout[], rows: number, cols: number): Layout | null {
  if (leaves.length === 0) return null;
  if (leaves.length === 1) return leaves[0];
  const columns: Layout[] = [];
  for (let c = 0; c < cols; c++) {
    const start = c * rows;
    const end = Math.min(start + rows, leaves.length);
    if (start >= end) break;
    columns.push(stackTree(leaves.slice(start, end), 'vertical'));
  }
  if (columns.length === 0) return null;
  if (columns.length === 1) return columns[0];
  return stackTree(columns, 'horizontal');
}

export function pruneLayout(layout: Layout, alive: (id: string) => boolean): Layout | null {
  if (layout.type === 'pane') return alive(layout.paneId) ? layout : null;
  if (layout.type === 'browser') return alive(layout.browserId) ? layout : null;
  if (layout.type === 'file') return alive(layout.fileId) ? layout : null;
  // Diff leaves are self-contained (path + staged live in the leaf) and
  // re-fetch on mount, so they always survive a reload.
  if (layout.type === 'diff') return layout;
  if (layout.type === 'tasks') return null;
  const a = pruneLayout(layout.a, alive);
  const b = pruneLayout(layout.b, alive);
  if (!a && !b) return null;
  if (!a) return b;
  if (!b) return a;
  return { ...layout, a, b };
}

// Convert the earlier task-dashboard owner tile in place when its real terminal
// is revealed. Keeping both with the same leaf ID makes focus/layout ambiguous.
export function migrateOwnerTerminal(layout: Layout | null, paneId: string): Layout | null {
  if (!layout) return layout;
  if (layout.type === 'tasks' && layout.tasksId === paneId && layout.ownerTaskId && paneId === `owner_${layout.ownerTaskId}`) {
    return { type: 'pane', paneId };
  }
  if (layout.type !== 'split') return layout;
  return { ...layout, a: migrateOwnerTerminal(layout.a, paneId)!, b: migrateOwnerTerminal(layout.b, paneId)! };
}

// Exact workflow bindings only. A generated-looking ID/title is not authority.
export function detachManagedOwnerPresentation(layout: Layout | null, bindings: ReadonlyMap<string,string>): Layout | null {
 if(!layout)return null;
 if(layout.type === 'pane')return bindings.has(layout.paneId) ? null : layout;
 if(layout.type === 'tasks')return layout.ownerTaskId && bindings.get(layout.tasksId) === layout.ownerTaskId ? null : layout;
 if(layout.type !== 'split')return layout;
 const a=detachManagedOwnerPresentation(layout.a,bindings),b=detachManagedOwnerPresentation(layout.b,bindings);
 if(!a)return b;if(!b)return a;
 return a === layout.a && b === layout.b ? layout : {...layout,a,b};
}
