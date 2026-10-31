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
