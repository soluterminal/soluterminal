import { expect, it } from 'vitest';
import type { PaneInfo } from '../types';
import { unsentSummary } from './AttentionInbox';

it('names a put-back message and the messages waiting behind it in the bell', () => {
  const pane = {
    id: 'p1',
    unsentMessage: { id: 'r1', sender: '@flag-guard', putBack: true },
    heldMessages: [{ id: 'h1', sender: '@a', text: 'x', at: 1 }, { id: 'h2', sender: '@b', text: 'y', at: 2 }],
  } as PaneInfo;
  expect(unsentSummary(pane)).toBe('Message from @flag-guard is back in the input box, unsent (2 more messages wait behind it) — Send or Clear it on the pane');
  expect(unsentSummary({ id: 'p2', unsentMessage: { id: 'r2', sender: '#5 Claude' } } as PaneInfo)).toBe('Message from #5 Claude not sent — Send or Clear it on the pane');
});

it('offers Send… or Discard for a message that was never typed', () => {
  const pane = {
    id: 'p1',
    unsentMessage: { id: 'r1', sender: '@worker', notTyped: true },
    heldMessages: [{ id: 'h1', sender: '@a', text: 'x', at: 1 }],
  } as PaneInfo;
  expect(unsentSummary(pane)).toBe('Message from @worker not sent — Send… or Discard it on the pane');
});

it('says an edited put-back message can be cleared or kept as a draft', () => {
  const pane = { id: 'p1', unsentMessage: { id: 'r1', sender: '@flag-guard', putBack: true, edited: true } } as PaneInfo;
  expect(unsentSummary(pane)).toBe('Message from @flag-guard still in the input box after your edit — Clear it or keep it as your draft on the pane');
});
