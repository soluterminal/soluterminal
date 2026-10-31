import { describe, expect, it } from 'vitest';
import type { Layout } from '../types';
import { collectLeaves, collectPaneIds } from './layout';
import { autoGridRows, balancedLayout, createViewportLayoutTracker, removeCustomPane, resizedGridConfig, splitLargestPane, viewportGridRows } from './autoLayout';

const leaf = (paneId: string): Layout => ({ type: 'pane', paneId });
const row: Layout = { type: 'split', direction: 'horizontal', ratio: .2, a: leaf('a'), b: { type: 'split', direction: 'horizontal', ratio: .375, a: leaf('b'), b: leaf('c') } };

describe('balanced workspace tiling', () => {
  const expected = [[1], [2], [3], [2, 2], [3, 2], [3, 3], [4, 3], [4, 4], [3, 3, 3], [4, 3, 3], [4, 4, 3], [4, 4, 4]];
  it.each(expected.map((rows, i) => [i + 1, rows] as const))('balances %i panes in leaf order', (count, rows) => {
    expect(autoGridRows(count, 1.6, 4)).toEqual(rows);
    expect(rows.reduce((a, b) => a + b)).toBe(count);
    expect(Math.max(...rows) - Math.min(...rows)).toBeLessThanOrEqual(1);
  });
  it('chooses four columns for eight panes at 1600 by 1000 and respects a larger minimum width', () => {
    expect(viewportGridRows(8, 1600, 1000, 320)).toEqual([4, 4]);
    expect(viewportGridRows(8, 1600, 1000, 640)).toEqual([2, 2, 2, 2]);
    expect(viewportGridRows(3, 1600, 1000)).toEqual([3]);
  });
  it('does not flap near a column threshold', () => {
    const wide = { columns: 4, aspect: 1.6 };
    expect(resizedGridConfig(8, 1260, 1000, wide)).toBe(wide);
    expect(resizedGridConfig(8, 940, 1000, wide).columns).toBe(3); // a jump across two thresholds still advances
    const narrow = resizedGridConfig(8, 1230, 1000, wide);
    expect(narrow.columns).toBe(3);
    expect(resizedGridConfig(8, 1300, 1000, narrow)).toBe(narrow);
    expect(resizedGridConfig(8, 1330, 1000, narrow).columns).toBe(4);
  });
  it('preserves every leaf and its payload when rebuilding the grid', () => {
    const layout: Layout = { type: 'split', direction: 'horizontal', ratio: .9, a: row, b: { type: 'file', fileId: 'notes' } };
    const balanced = balancedLayout(layout);
    expect(collectLeaves(balanced)).toEqual(collectLeaves(layout));
    expect(collectPaneIds(balanced)).toEqual(['a', 'b', 'c']);
  });
});

describe('custom workspace geometry', () => {
  it('splits the largest tile along its longer side and preserves the other area', () => {
    const layout: Layout = { type: 'split', direction: 'horizontal', ratio: .2, a: leaf('small'), b: leaf('large') };
    expect(splitLargestPane(layout, 'new', 1.6)).toEqual({ ...layout, b: { type: 'split', direction: 'horizontal', ratio: .5, a: leaf('large'), b: leaf('new') } });
  });
  it('shares the released width among all remaining row siblings', () => {
    const next = removeCustomPane(row, 'b');
    expect(next).toMatchObject({ type: 'split', direction: 'horizontal', a: leaf('a'), b: leaf('c') });
    expect(next?.type === 'split' ? next.ratio : 0).toBeCloseTo(.35);
  });
  it('keeps a perpendicular group and unrelated proportions intact', () => {
    const layout: Layout = { type: 'split', direction: 'vertical', ratio: .3, a: row, b: leaf('below') };
    expect(removeCustomPane(layout, 'b')).toEqual({ ...layout, a: removeCustomPane(row, 'b') });
  });
});

it('publishes viewport geometry only for real size changes or legacy initialization', () => {
  const first = createViewportLayoutTracker(), second = createViewportLayoutTracker();
  expect(first('saved', 1400, 900, true)).toBe(false);
  expect(second('saved', 900, 700, true)).toBe(false);
  expect(first('saved', 1400, 900, true)).toBe(false); // peer update / count change
  expect(first('saved', 1200, 900, true)).toBe(true);
  expect(second('saved', 900, 700, true)).toBe(false); // passive peer must not echo its size
  expect(first('legacy', 1400, 900, false)).toBe(true);
});
