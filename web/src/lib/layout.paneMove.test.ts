import { expect, it } from 'vitest';
import type { Layout } from '../types';
import { collectPaneIds, movePaneInLayoutImpl } from './layout';

it.each(['left', 'right', 'top', 'bottom', 'center'] as const)('never removes a pane when the %s drop target is absent from the saved layout', zone => {
  const saved: Layout = { type: 'split', direction: 'horizontal', ratio: 0.3,
    a: { type: 'pane', paneId: 'source' }, b: { type: 'pane', paneId: 'other' } };
  expect(movePaneInLayoutImpl(saved, 'source', 'appended', zone)).toBe(saved);
  expect(movePaneInLayoutImpl(saved, 'appended', 'source', zone)).toBe(saved);
  expect(collectPaneIds(saved)).toEqual(['source', 'other']);
});
