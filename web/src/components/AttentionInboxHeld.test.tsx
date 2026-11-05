import { renderHook } from '@testing-library/react';
import { afterEach, expect, it } from 'vitest';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { heldStuckSummary, useAttentionBadge, useAttentionItems } from './AttentionInbox';

const held = [
  { id: 'h1', sender: '@exe-ci · #16 Claude', text: 'secret one', at: 1 },
  { id: 'h2', sender: '@exe-ci · #16 Claude', text: 'secret two', at: 2 },
  { id: 'h3', sender: '@blob · #10 Claude', text: 'secret three', at: 3 },
];
const stuck = { id: 'p1', status: 'running', state: 'idle', cmd: [], title: '@ops', heldMessages: held, heldStuck: true } as unknown as PaneInfo;
const initial = useStore.getState();
afterEach(() => useStore.setState(initial));

function withPanes(...panes: PaneInfo[]) {
  useStore.setState({ panes: Object.fromEntries(panes.map((p) => [p.id, p])), paneTails: {}, workspaces: [] });
}

it('says how many wait, from whom, with no message text and no receipt needed', () => {
  expect(heldStuckSummary(stuck)).toBe('3 messages from @exe-ci, @blob wait behind the text in the input box — send or clear it in the pane');
  expect(heldStuckSummary({ ...stuck, heldMessages: held.slice(0, 1) })).toBe('1 message from @exe-ci waits behind the text in the input box — send or clear it in the pane');
  expect(heldStuckSummary(stuck)).not.toContain('secret');
});

it('names the unsent message it waits behind, and shortens a long sender list', () => {
  const receipt = { id: 'r', sender: '@a · #1 Claude', putBack: true };
  expect(heldStuckSummary({ ...stuck, unsentMessage: receipt })).toContain("wait behind @a · #1 Claude's unsent message — Send or Clear it on the pane");
  expect(heldStuckSummary({ ...stuck, unsentMessage: { ...receipt, edited: true } })).toContain('Clear it or keep it as your draft');
  const many = ['@a', '@b', '@c', '@d', '@e'].map((sender, i) => ({ id: `h${i}`, sender, text: '', at: i }));
  expect(heldStuckSummary({ ...stuck, heldMessages: many })).toContain('from @a, @b, @c and 2 more wait');
});

it('lists and counts a stuck pane with no receipt, and only when stuck', () => {
  withPanes(stuck, { ...stuck, id: 'p2', heldStuck: false });
  const items = renderHook(() => useAttentionItems()).result.current;
  expect(items.map((it) => [it.pane.id, it.kind])).toEqual([['p1', 'held']]);
  expect(renderHook(() => useAttentionBadge()).result.current).toMatchObject({ count: 1, firstPaneId: 'p1' });
});

it('replaces the receipt row, and sorts above a plain unsent message', () => {
  const receipt = { id: 'r', sender: '@a', putBack: true };
  withPanes({ ...stuck, unsentMessage: receipt }, { ...stuck, id: 'p2', heldMessages: [], heldStuck: false, unsentMessage: receipt });
  const items = renderHook(() => useAttentionItems()).result.current;
  expect(items.map((it) => [it.pane.id, it.kind])).toEqual([['p1', 'held'], ['p2', 'unsent']]);
  expect(renderHook(() => useAttentionBadge()).result.current.count).toBe(2);
});

it('keeps both rows when the pane also waits at a prompt, and bell and rows agree', () => {
  withPanes({ ...stuck, state: 'awaiting-input', attention: 'needs-user' });
  const items = renderHook(() => useAttentionItems()).result.current;
  expect(items.map((it) => it.kind)).toEqual(['held', 'awaiting']);
  expect(renderHook(() => useAttentionBadge()).result.current.count).toBe(items.length);
});

it('ignores a pane that is no longer running', () => {
  withPanes({ ...stuck, status: 'exited' });
  expect(renderHook(() => useAttentionItems()).result.current).toEqual([]);
  expect(renderHook(() => useAttentionBadge()).result.current.count).toBe(0);
});

it('keeps the unverified mark, so a claimed sender never reads as the real one', () => {
  const claimed = [
    { id: 'h1', sender: '@lead · #1 Claude', text: 'a', at: 1 },
    { id: 'h2', sender: '@lead · #1 Claude (unverified)', text: 'b', at: 2 },
    { id: 'h3', sender: 'an unverified agent', text: 'c', at: 3 },
  ];
  expect(heldStuckSummary({ ...stuck, heldMessages: claimed })).toContain('from @lead, @lead (unverified), an unverified agent wait');
});

it('keeps the row for a message never typed, which the held row does not cover', () => {
  withPanes({ ...stuck, unsentMessage: { id: 'r', sender: '@a', notTyped: true, text: 'x' } });
  const items = renderHook(() => useAttentionItems()).result.current;
  expect(items.map((it) => it.kind)).toEqual(['held', 'unsent']);
  expect(renderHook(() => useAttentionBadge()).result.current.count).toBe(2);
});
