import { describe, expect, it, vi } from 'vitest';
import type { Layout } from '../types';
import { api } from '../api';
import { useStore } from '../store';
import {
  computeLeafShares,
  containsBrowserLeaf,
  dimensionLeafCount,
  insertLeaf,
  insertPane,
  rebalanceLayout,
} from './layout';

describe('layout leaf rebalancing', () => {
  it('rebalanceLayout handles null and single leaf gracefully', () => {
    expect(rebalanceLayout(null)).toBeNull();
    const leaf: Layout = { type: 'pane', paneId: 'p1' };
    expect(rebalanceLayout(leaf)).toEqual(leaf);
  });

  it('insertPane delegates to insertLeaf for pane tiles', () => {
    const pane1: Layout = { type: 'pane', paneId: 'p1' };
    const result = insertPane(pane1, 'p2', 'horizontal', 'p1');
    expect(result).toEqual({
      type: 'split',
      direction: 'horizontal',
      ratio: 0.5,
      a: { type: 'pane', paneId: 'p1' },
      b: { type: 'pane', paneId: 'p2' },
    });
  });

  it('dimensionLeafCount counts leaves along the matching split direction', () => {
    const pane1: Layout = { type: 'pane', paneId: 'p1' };
    const pane2: Layout = { type: 'pane', paneId: 'p2' };
    const horizontalSplit: Layout = {
      type: 'split',
      direction: 'horizontal',
      ratio: 0.5,
      a: pane1,
      b: pane2,
    };
    expect(dimensionLeafCount(horizontalSplit, 'horizontal')).toBe(2);
    // An orthogonal split counts as 1 slot along the perpendicular dimension
    expect(dimensionLeafCount(horizontalSplit, 'vertical')).toBe(1);
  });

  it('detects browser leaves correctly with containsBrowserLeaf', () => {
    const pane: Layout = { type: 'pane', paneId: 'p1' };
    const browser: Layout = { type: 'browser', browserId: 'b1' };
    const split: Layout = {
      type: 'split',
      direction: 'horizontal',
      ratio: 0.6,
      a: pane,
      b: browser,
    };
    expect(containsBrowserLeaf(pane)).toBe(false);
    expect(containsBrowserLeaf(browser)).toBe(true);
    expect(containsBrowserLeaf(split)).toBe(true);
  });

  it('inserts without rebalance by default (legacy 50/50 split on targeted pane)', () => {
    const initial: Layout = {
      type: 'split',
      direction: 'horizontal',
      ratio: 0.5,
      a: { type: 'pane', paneId: 'p1' },
      b: { type: 'pane', paneId: 'p2' },
    };

    const next = insertLeaf(
      initial,
      { type: 'file', fileId: 'f1' },
      'horizontal',
      'p2',
    );

    const shares = computeLeafShares(next);
    // Without rebalancing, p1 keeps 50%, while p2 and f1 take 25% each
    expect(shares.get('p1')).toBeCloseTo(0.5, 4);
    expect(shares.get('p2')).toBeCloseTo(0.25, 4);
    expect(shares.get('f1')).toBeCloseTo(0.25, 4);
  });

  it('rebalances file tile to equal share (1/3 each) when inserted next to p2', () => {
    const initial: Layout = {
      type: 'split',
      direction: 'horizontal',
      ratio: 0.5,
      a: { type: 'pane', paneId: 'p1' },
      b: { type: 'pane', paneId: 'p2' },
    };

    const next = insertLeaf(
      initial,
      { type: 'file', fileId: 'f1' },
      'horizontal',
      'p2',
      { rebalance: true },
    );

    const shares = computeLeafShares(next);
    // With rebalance, all 3 tiles get equal share (1/3 each)
    expect(shares.get('p1')).toBeCloseTo(1 / 3, 4);
    expect(shares.get('p2')).toBeCloseTo(1 / 3, 4);
    expect(shares.get('f1')).toBeCloseTo(1 / 3, 4);
  });

  it('rebalances file tile to equal share (1/3 each) when inserted next to p1', () => {
    const initial: Layout = {
      type: 'split',
      direction: 'horizontal',
      ratio: 0.5,
      a: { type: 'pane', paneId: 'p1' },
      b: { type: 'pane', paneId: 'p2' },
    };

    const next = insertLeaf(
      initial,
      { type: 'file', fileId: 'f1' },
      'horizontal',
      'p1',
      { rebalance: true },
    );

    const shares = computeLeafShares(next);
    expect(shares.get('p1')).toBeCloseTo(1 / 3, 4);
    expect(shares.get('f1')).toBeCloseTo(1 / 3, 4);
    expect(shares.get('p2')).toBeCloseTo(1 / 3, 4);
  });

  it('distributes equal shares across 4 tiles when adding another file tile', () => {
    let layout: Layout = {
      type: 'split',
      direction: 'horizontal',
      ratio: 0.5,
      a: { type: 'pane', paneId: 'p1' },
      b: { type: 'pane', paneId: 'p2' },
    };

    // First file added -> 3 tiles, 1/3 each
    layout = insertLeaf(layout, { type: 'file', fileId: 'f1' }, 'horizontal', 'p2', { rebalance: true });
    // Second file added -> 4 tiles, 1/4 (0.25) each
    layout = insertLeaf(layout, { type: 'file', fileId: 'f2' }, 'horizontal', 'f1', { rebalance: true });

    const shares = computeLeafShares(layout);
    expect(shares.get('p1')).toBeCloseTo(0.25, 4);
    expect(shares.get('p2')).toBeCloseTo(0.25, 4);
    expect(shares.get('f1')).toBeCloseTo(0.25, 4);
    expect(shares.get('f2')).toBeCloseTo(0.25, 4);
  });

  it('rebalances vertical splits equally when inserting vertically', () => {
    const initial: Layout = {
      type: 'split',
      direction: 'vertical',
      ratio: 0.5,
      a: { type: 'pane', paneId: 'p1' },
      b: { type: 'pane', paneId: 'p2' },
    };

    const next = insertLeaf(
      initial,
      { type: 'file', fileId: 'f1' },
      'vertical',
      'p2',
      { rebalance: true },
    );

    const shares = computeLeafShares(next);
    expect(shares.get('p1')).toBeCloseTo(1 / 3, 4);
    expect(shares.get('p2')).toBeCloseTo(1 / 3, 4);
    expect(shares.get('f1')).toBeCloseTo(1 / 3, 4);
  });

  it('preserves orthogonal dimension splits without distorting outer layout', () => {
    // 2 columns (horizontal 0.5 / 0.5).
    const initial: Layout = {
      type: 'split',
      direction: 'horizontal',
      ratio: 0.5,
      a: { type: 'pane', paneId: 'p1' },
      b: { type: 'pane', paneId: 'p2' },
    };

    // Insert file vertically inside column p2
    const next = insertLeaf(
      initial,
      { type: 'file', fileId: 'f1' },
      'vertical',
      'p2',
      { rebalance: true },
    );

    // Outer split should remain 0.5 because column 2 is 1 orthogonal unit
    expect(next.type).toBe('split');
    if (next.type === 'split') {
      expect(next.ratio).toBe(0.5);
      expect(next.direction).toBe('horizontal');
      // Subtree inside b should be vertical split with 0.5 ratio
      expect(next.b.type).toBe('split');
      if (next.b.type === 'split') {
        expect(next.b.direction).toBe('vertical');
        expect(next.b.ratio).toBe(0.5);
      }
    }
  });

  it('preserves browser split ratio while rebalancing non-browser pane tiles', () => {
    const initial: Layout = {
      type: 'split',
      direction: 'horizontal',
      ratio: 0.6,
      a: {
        type: 'split',
        direction: 'horizontal',
        ratio: 0.5,
        a: { type: 'pane', paneId: 'p1' },
        b: { type: 'pane', paneId: 'p2' },
      },
      b: { type: 'browser', browserId: 'b1' },
    };

    const next = insertLeaf(
      initial,
      { type: 'file', fileId: 'f1' },
      'horizontal',
      'p2',
      { rebalance: true },
    );

    // Browser split ratio must stay 0.6
    expect(next.type).toBe('split');
    if (next.type === 'split') {
      expect(next.ratio).toBe(0.6);
      const shares = computeLeafShares(next);
      // Browser retains 40% (0.4)
      expect(shares.get('b1')).toBeCloseTo(0.4, 4);
      // The 60% (0.6) on the left is split equally among p1, p2, f1 (0.2 each)
      expect(shares.get('p1')).toBeCloseTo(0.2, 4);
      expect(shares.get('p2')).toBeCloseTo(0.2, 4);
      expect(shares.get('f1')).toBeCloseTo(0.2, 4);
    }
  });

  it('store openFileHere applies equal share rebalancing across tiles', () => {
    vi.spyOn(api, 'saveWorkspace').mockResolvedValue(undefined as never);
    useStore.setState({
      panes: {
        p1: { id: 'p1', cwd: '/', cmd: [], title: 'p1' } as any,
        p2: { id: 'p2', cwd: '/', cmd: [], title: 'p2' } as any,
      },
      layout: {
        type: 'split',
        direction: 'horizontal',
        ratio: 0.5,
        a: { type: 'pane', paneId: 'p1' },
        b: { type: 'pane', paneId: 'p2' },
      },
      focusedPaneId: 'p2',
    });

    useStore.getState().openFileHere('/test/file.txt', 'p2', 'horizontal');

    const layout = useStore.getState().layout;
    expect(layout).not.toBeNull();
    const shares = computeLeafShares(layout);
    expect(shares.get('p1')).toBeCloseTo(1 / 3, 4);
    expect(shares.get('p2')).toBeCloseTo(1 / 3, 4);
    const fileId = Array.from(shares.keys()).find((k) => k.startsWith('file_'));
    expect(fileId).toBeDefined();
    expect(shares.get(fileId!)).toBeCloseTo(1 / 3, 4);
  });
});
