import { expect, it } from 'vitest';
import type { PaneInfo } from '../types';
import { paneRowStatus } from './paneStatus';
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
  expect(paneRowStatus({ ...p, status: 'exited', asks: [ask] })).toBe('finished');
});
