import { describe, expect, it, vi } from 'vitest';
import type { Layout, PaneInfo } from '../types';
import { api } from '../api';
import { useStore } from '../store';
import {
  containsBrowserLeaf,
  dimensionLeafCount,
  insertLeaf,
  insertPane,
  leafId,
  rebalanceLayout,
} from './layout';

function computeLeafShares(layout: Layout | null): Map<string, number> {
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

function makePane(id: string): PaneInfo {
  return {
    id,
    cwd: '/',
    cmd: [],
    title: id,
    status: 'running',
    createdAt: new Date().toISOString(),
    cols: 80,
    rows: 24,
    pid: 1,
    lastActivityMs: Date.now(),
    state: 'idle',
    hasClaude: false,
  };
}

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

  it('preserves resized orthogonal split (0.7/0.3) elsewhere in the tree', () => {
    // Vertical split resized by the user to 0.7/0.3:
    // Top: pane p0 (70% height)
    // Bottom: horizontal split with p1 and p2 (30% height, 50/50 width)
    const initial: Layout = {
      type: 'split',
      direction: 'vertical',
      ratio: 0.7,
      a: { type: 'pane', paneId: 'p0' },
      b: {
        type: 'split',
        direction: 'horizontal',
        ratio: 0.5,
        a: { type: 'pane', paneId: 'p1' },
        b: { type: 'pane', paneId: 'p2' },
      },
    };

    // Open file horizontally next to p2 in the bottom pane
    const next = insertLeaf(
      initial,
      { type: 'file', fileId: 'f1' },
      'horizontal',
      'p2',
      { rebalance: true },
    );

    // The vertical split elsewhere MUST preserve its 0.7 ratio
    expect(next.type).toBe('split');
    if (next.type === 'split') {
      expect(next.direction).toBe('vertical');
      expect(next.ratio).toBe(0.7);
    }

    const shares = computeLeafShares(next);
    // p0 keeps its 70% share
    expect(shares.get('p0')).toBeCloseTo(0.7, 4);
    // The bottom 30% is rebalanced equally across p1, p2, f1 (10% each)
    expect(shares.get('p1')).toBeCloseTo(0.1, 4);
    expect(shares.get('p2')).toBeCloseTo(0.1, 4);
    expect(shares.get('f1')).toBeCloseTo(0.1, 4);
  });

  it('preserves resized split on an adjacent branch (0.7/0.3) when opening a file tile', () => {
    // Vertical split with two horizontal rows:
    // Row A: horizontal split resized to 0.7/0.3 (p1, p2)
    // Row B: horizontal split 0.5/0.5 (p3, p4)
    const initial: Layout = {
      type: 'split',
      direction: 'vertical',
      ratio: 0.5,
      a: {
        type: 'split',
        direction: 'horizontal',
        ratio: 0.7,
        a: { type: 'pane', paneId: 'p1' },
        b: { type: 'pane', paneId: 'p2' },
      },
      b: {
        type: 'split',
        direction: 'horizontal',
        ratio: 0.5,
        a: { type: 'pane', paneId: 'p3' },
        b: { type: 'pane', paneId: 'p4' },
      },
    };

    // Open file horizontally next to p4 in row B
    const next = insertLeaf(
      initial,
      { type: 'file', fileId: 'f1' },
      'horizontal',
      'p4',
      { rebalance: true },
    );

    // Outer vertical split stays 0.5
    expect(next.type).toBe('split');
    if (next.type === 'split') {
      expect(next.direction).toBe('vertical');
      expect(next.ratio).toBe(0.5);

      // Row A's horizontal split was NOT joined by the new file, so it MUST preserve 0.7
      expect(next.a.type).toBe('split');
      if (next.a.type === 'split') {
        expect(next.a.direction).toBe('horizontal');
        expect(next.a.ratio).toBe(0.7);
      }
    }

    const shares = computeLeafShares(next);
    // Row A (50% total): p1 gets 70% of 50% = 0.35, p2 gets 30% of 50% = 0.15
    expect(shares.get('p1')).toBeCloseTo(0.35, 4);
    expect(shares.get('p2')).toBeCloseTo(0.15, 4);
    // Row B (50% total): p3, p4, f1 each get equal share (1/3 of 50% = 1/6 ≈ 0.1667)
    expect(shares.get('p3')).toBeCloseTo(0.5 / 3, 4);
    expect(shares.get('p4')).toBeCloseTo(0.5 / 3, 4);
    expect(shares.get('f1')).toBeCloseTo(0.5 / 3, 4);
  });

  it('store openFileHere applies equal share rebalancing across tiles', () => {
    vi.spyOn(api, 'saveWorkspace').mockResolvedValue(undefined as never);
    useStore.setState({
      panes: {
        p1: makePane('p1'),
        p2: makePane('p2'),
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
