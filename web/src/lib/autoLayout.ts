import type { Layout } from '../types';
import { collectLeaves, leafId } from './layout';

// Compact desktop floor. The explicit 1600px / eight-pane acceptance case
// needs four columns; callers can supply a larger reading width.
export const MIN_AUTO_PANE_WIDTH = 320;
export const AUTO_LAYOUT_HYSTERESIS = 40;
export const DEFAULT_LAYOUT_COLUMNS = 4;
export const MAX_LAYOUT_COLUMNS = 24;
export const DEFAULT_LAYOUT_ASPECT = 1.6;

export function autoGridRows(count: number, aspect = DEFAULT_LAYOUT_ASPECT, maxColumns = DEFAULT_LAYOUT_COLUMNS): number[] {
  if (count <= 0) return [];
  const columns = Math.max(1, Math.min(count, Math.max(1, Math.min(MAX_LAYOUT_COLUMNS, Math.floor(maxColumns))), Math.ceil(Math.sqrt(count * Math.max(0.25, aspect)))));
  const rows = Math.ceil(count / columns);
  const base = Math.floor(count / rows), extra = count % rows;
  return Array.from({ length: rows }, (_, row) => base + (row < extra ? 1 : 0));
}

export function viewportGridRows(count: number, width: number, height: number, minWidth = MIN_AUTO_PANE_WIDTH): number[] {
  return autoGridRows(count, width / Math.max(1, height), Math.max(1, Math.min(MAX_LAYOUT_COLUMNS, Math.floor(width / minWidth))));
}

export function resizedGridConfig(count: number, width: number, height: number, previous: { columns: number; aspect: number }, minWidth = MIN_AUTO_PANE_WIDTH): { columns: number; aspect: number } {
  const next = { columns: Math.max(1, Math.min(MAX_LAYOUT_COLUMNS, Math.floor(width / minWidth))), aspect: width / Math.max(1, height) };
  const oldRows = autoGridRows(count, previous.aspect, previous.columns);
  const nextRows = autoGridRows(count, next.aspect, next.columns);
  if (JSON.stringify(oldRows) === JSON.stringify(nextRows)) return previous;
  // Only accept a crossing if a 40px band around the new size agrees on the
  // new shape. Count changes use the stored configuration without hysteresis.
  const edge = width + ((nextRows[0] ?? 0) > (oldRows[0] ?? 0) ? -AUTO_LAYOUT_HYSTERESIS : AUTO_LAYOUT_HYSTERESIS);
  const edgeRows = viewportGridRows(count, edge, height, minWidth);
  if (JSON.stringify(edgeRows) === JSON.stringify(oldRows)) return previous;
  if (JSON.stringify(edgeRows) !== JSON.stringify(nextRows)) {
    return { columns: Math.max(1, Math.min(MAX_LAYOUT_COLUMNS, Math.floor(edge / minWidth))), aspect: edge / Math.max(1, height) };
  }
  return next;
}

function stack(nodes: Layout[], direction: 'horizontal' | 'vertical', weights = nodes.map(() => 1)): Layout {
  if (nodes.length === 1) return nodes[0];
  const total = weights.reduce((a, b) => a + b, 0);
  const middle = Math.floor(nodes.length / 2);
  return { type: 'split', direction, ratio: weights.slice(0, middle).reduce((a, b) => a + b, 0) / total,
    a: stack(nodes.slice(0, middle), direction, weights.slice(0, middle)),
    b: stack(nodes.slice(middle), direction, weights.slice(middle)) };
}

export function balancedLayout(layout: Layout | null, columns = DEFAULT_LAYOUT_COLUMNS, aspect = DEFAULT_LAYOUT_ASPECT): Layout | null {
  const leaves = collectLeaves(layout);
  let offset = 0;
  const rows = autoGridRows(leaves.length, aspect, columns).map(count => {
    const row = stack(leaves.slice(offset, offset + count), 'horizontal'); offset += count; return row;
  });
  return rows.length ? stack(rows, 'vertical') : null;
}

export function appendAutoPane(layout: Layout | null, paneId: string, columns: number, aspect: number): Layout {
  const leaf: Layout = { type: 'pane', paneId };
  return balancedLayout(layout ? { type: 'split', direction: 'horizontal', ratio: .5, a: layout, b: leaf } : leaf, columns, aspect)!;
}

export function splitLargestPane(layout: Layout | null, paneId: string, aspect = DEFAULT_LAYOUT_ASPECT): Layout {
  if (!layout) return { type: 'pane', paneId };
  let largest: Layout = layout, area = -1, horizontal = true;
  const visit = (node: Layout, width: number, height: number) => {
    if (node.type !== 'split') {
      if (width * height > area) { largest = node; area = width * height; horizontal = width >= height; }
      return;
    }
    if (node.direction === 'horizontal') { visit(node.a, width * node.ratio, height); visit(node.b, width * (1 - node.ratio), height); }
    else { visit(node.a, width, height * node.ratio); visit(node.b, width, height * (1 - node.ratio)); }
  };
  visit(layout, aspect, 1);
  const replace = (node: Layout): Layout => node === largest
    ? { type: 'split', direction: horizontal ? 'horizontal' : 'vertical', ratio: .5, a: node, b: { type: 'pane', paneId } }
    : node.type === 'split' ? { ...node, a: replace(node.a), b: replace(node.b) } : node;
  return replace(layout);
}

// Flatten just the contiguous row/column containing the removed tile. Its
// released area is divided equally among the other siblings; other axes keep
// their geometry. This preserves a deliberately unequal custom arrangement.
export function removeCustomPane(layout: Layout | null, id: string): Layout | null {
  if (!layout) return null;
  if (layout.type !== 'split') return leafId(layout) === id ? null : layout;
  const siblings: { node: Layout; weight: number }[] = [];
  const flatten = (node: Layout, weight: number) => {
    if (node.type === 'split' && node.direction === layout.direction) {
      flatten(node.a, weight * node.ratio); flatten(node.b, weight * (1 - node.ratio));
    } else siblings.push({ node, weight });
  };
  flatten(layout, 1);
  let released = 0, changed = false;
  const kept: { node: Layout; weight: number }[] = [];
  for (const sibling of siblings) {
    const next = removeCustomPane(sibling.node, id);
    if (next !== sibling.node) changed = true;
    if (next) kept.push({ node: next, weight: sibling.weight }); else released += sibling.weight;
  }
  if (!changed) return layout;
  if (!kept.length) return null;
  return stack(kept.map(s => s.node), layout.direction, kept.map(s => s.weight + released / kept.length));
}

// A saved workspace governs a passive viewer. Only a real size change in
// this view (or a one-time legacy initialization) may publish viewport data.
export function createViewportLayoutTracker() {
  const measured = new Map<string, { width: number; height: number }>();
  return (key: string, width: number, height: number, configured: boolean): boolean => {
    const previous = measured.get(key);
    measured.set(key, { width, height });
    return previous ? Math.abs(previous.width - width) > 1 || Math.abs(previous.height - height) > 1 : !configured;
  };
}
