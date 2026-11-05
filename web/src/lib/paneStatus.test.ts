import { expect, it } from 'vitest';
import type { PaneInfo } from '../types';
import { paneNeedsYouLabel, paneRowStatus } from './paneStatus';

it('does not mark quiet lead housekeeping as needs-you', () => {
  expect(paneRowStatus({ id: 'lead', status: 'running', state: 'idle', attention: 'none', workerNotices: [{ id: 'n1', text: 'Worker @x kept open', source: '@lead', createdAt: 1 }] } as PaneInfo)).toBe('idle');
});
const p = { id: 'worker', status: 'running', state: 'idle', turn: { active: false, count: 1, last: { n: 1, endedMs: 10, startedMs: 1, files: 0, added: 0, removed: 0, outcome: 'completed' } } } as PaneInfo;
it('requires completion evidence instead of an idle timeout', () => {
  expect(paneRowStatus({ ...p, turn: undefined })).toBe('idle');
  expect(paneRowStatus(p)).toBe('finished');
  expect(paneRowStatus({ ...p, turn: { ...p.turn!, active: true } })).toBe('busy');
  expect(paneRowStatus({ ...p, workflowStatus: { running: true, agentsTotal: 2, agentsDone: 1, lastActivityMs: 0 } })).toBe('busy');
});
it('honours daemon attention and suppresses auto-approval alerts', () => {
  expect(paneRowStatus({ ...p, state: 'choice-prompt', attention: 'needs-user' })).toBe('needs-you');
  expect(paneRowStatus({ ...p, state: 'choice-prompt', attention: 'auto-pending' })).toBe('idle');
  expect(paneRowStatus({ ...p, state: 'choice-prompt', attention: 'none', turn: undefined })).toBe('idle');
});
it('counts agent output as busy but not the recent tail after typing', () => {
  expect(paneRowStatus({ ...p, state: 'busy' })).toBe('busy');
  expect(paneRowStatus({ ...p, state: 'recent' })).toBe('finished');
  expect(paneRowStatus({ ...p, state: 'recent', turn: undefined })).toBe('idle');
  expect(paneRowStatus({ ...p, state: 'recent', agentMidTurn: true })).toBe('busy');
});
it("counts an agent's open ask as needing you, until the pane exits", () => {
  const ask = { id: 'a1', text: 'OK to deploy?', source: '@lead', createdAt: 1 };
  expect(paneRowStatus({ ...p, state: 'busy', asks: [ask] })).toBe('needs-you');
  expect(paneRowStatus({ ...p, asks: [] })).toBe('finished');
  expect(paneRowStatus({ ...p, status: 'exited', asks: [ask] })).toBe('needs-you');
});
it('says why a row needs you', () => {
  const ask = { id: 'a1', text: 'OK to deploy?', source: '@lead', createdAt: 1 };
  const asks = [ask, { ...ask, id: 'a2' }];
  expect(paneNeedsYouLabel({ ...p, state: 'choice-prompt', attention: 'needs-user' })).toBe('needs answer');
  expect(paneNeedsYouLabel({ ...p, asks: [ask] })).toBe('1 ask');
  expect(paneNeedsYouLabel({ ...p, asks })).toBe('2 asks');
  expect(paneNeedsYouLabel({ ...p, state: 'choice-prompt', attention: 'needs-user', asks })).toBe('needs answer · 2 asks');
  // A menu auto-approval is handling, or a dead pane's stale state, isn't waiting on anyone.
  expect(paneNeedsYouLabel({ ...p, state: 'choice-prompt', attention: 'auto-pending', asks })).toBe('2 asks');
  expect(paneNeedsYouLabel({ ...p, status: 'exited', state: 'choice-prompt', attention: 'needs-user', asks: [ask] })).toBe('1 ask');
  // Anything else stays generic rather than guessing.
  expect(paneNeedsYouLabel({ ...p, state: 'awaiting-input', attention: 'needs-user' })).toBe('needs you');
});
it('marks a pane with stuck held messages as needing you, only while running', () => {
  const held = [{ id: 'h', sender: '@a', text: 'x', at: 1 }];
  expect(paneRowStatus({ ...p, heldMessages: held, heldStuck: true })).toBe('needs-you');
  expect(paneRowStatus({ ...p, heldMessages: held })).toBe('finished');
  expect(paneRowStatus({ ...p, status: 'exited', heldMessages: held, heldStuck: true })).toBe('finished');
});
