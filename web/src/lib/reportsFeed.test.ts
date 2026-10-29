import { describe, expect, it } from 'vitest';
import { reportsFeed, runNeedsYou, runPreview } from './reportsFeed';
import type { Schedule, ScheduleRun } from '../types';

function sched(over: Partial<Schedule> = {}): Schedule {
  return {
    id: 's1',
    name: 'Morning digest',
    kind: 'task',
    enabled: true,
    createdAt: '2026-07-01T00:00:00Z',
    fireCount: 0,
    skipCount: 0,
    errorCount: 0,
    ...over,
  } as Schedule;
}

function run(over: Partial<ScheduleRun> = {}): ScheduleRun {
  return { at: '2026-07-10T09:00:00Z', status: 'fired', ...over };
}

describe('reportsFeed', () => {
  it('files ONE row per task schedule (its newest non-skipped run), newest first; skips and non-task kinds are excluded', () => {
    const items = reportsFeed([
      sched({
        id: 'a',
        runs: [
          run({ at: '2026-07-10T09:00:00Z', status: 'skipped' }),
          run({ at: '2026-07-09T09:00:00Z' }),
          run({ at: '2026-07-08T09:00:00Z' }),
        ],
      }),
      sched({ id: 'b', runs: [run({ at: '2026-07-09T21:00:00Z' })] }),
      sched({ id: 'c', kind: 'agent', runs: [run({ at: '2026-07-10T23:00:00Z' })] }),
      sched({ id: 'd', kind: 'prompt', runs: [run({ at: '2026-07-10T22:00:00Z' })] }),
      sched({ id: 'e', runs: [run({ at: '2026-07-10T22:00:00Z', status: 'skipped' })] }),
    ]);
    expect(items.map((i) => `${i.schedule.id}:${i.run.at}`)).toEqual([
      'b:2026-07-09T21:00:00Z',
      'a:2026-07-09T09:00:00Z',
    ]);
  });

  it('counts "needs you" once per schedule, from the newest report only', () => {
    const items = reportsFeed([
      sched({
        id: 'daily',
        runs: Array.from({ length: 7 }, (_, i) =>
          run({ at: `2026-07-${String(20 - i).padStart(2, '0')}T09:00:00Z`, summary: '## Needs you\n- merge PR #7' })),
      }),
      sched({
        id: 'quiet-today',
        runs: [
          run({ at: '2026-07-20T06:00:00Z', summary: 'Nothing new needs you.' }),
          run({ at: '2026-07-19T06:00:00Z', summary: '## Needs you\n- rotate the key' }),
        ],
      }),
    ]);
    expect(items).toHaveLength(2);
    // quiet-today's older ask is unread, so it still counts — once.
    expect(items.filter((i) => i.needsYou).map((i) => i.schedule.id)).toEqual(['daily', 'quiet-today']);
  });

  it('an ask the user already read does not resurface under a quiet newer report', () => {
    const [item] = reportsFeed([
      sched({
        lastReadAt: '2026-07-19T06:00:00Z',
        runs: [
          run({ at: '2026-07-20T06:00:00Z', summary: 'Nothing new needs you.' }),
          run({ at: '2026-07-19T06:00:00Z', summary: '## Needs you\n- rotate the key' }),
        ],
      }),
    ]);
    expect(item.unread).toBe(true);
    expect(item.needsYou).toBe(false);
  });

  it('marks the row unread from the newest run and folds older unread runs into earlierUnread', () => {
    const [item] = reportsFeed([
      sched({
        lastReadAt: '2026-07-08T09:00:00Z',
        runs: [
          run({ at: '2026-07-10T09:00:00Z' }),
          run({ at: '2026-07-09T09:00:00Z', status: 'error' }),
          run({ at: '2026-07-08T09:00:00Z' }),
          run({ at: '2026-07-07T09:00:00Z' }),
        ],
      }),
    ]);
    expect(item.unread).toBe(true);
    expect(item.run.at).toBe('2026-07-10T09:00:00Z');
    expect(item.earlierUnread).toBe(1);
    // No marker at all → unread.
    expect(reportsFeed([sched({ runs: [run()] })])[0].unread).toBe(true);
    // Read up to the newest → read, nothing folded.
    const [read] = reportsFeed([sched({ lastReadAt: '2026-07-10T09:00:00Z', runs: [run({ at: '2026-07-10T09:00:00Z' }), run({ at: '2026-07-09T09:00:00Z' })] })]);
    expect(read.unread).toBe(false);
    expect(read.earlierUnread).toBe(0);
  });
});

describe('runNeedsYou', () => {
  it('detects the contract heading, case-insensitively, and error runs', () => {
    expect(runNeedsYou(run({ summary: '## Needs you\n- approve PR #7' }))).toBe(true);
    expect(runNeedsYou(run({ summary: '  ## needs YOU — two things' }))).toBe(true);
    expect(runNeedsYou(run({ summary: 'Nothing needs you today.\n\n## CI' }))).toBe(false);
    expect(runNeedsYou(run({ summary: '## Needs your attention later' }))).toBe(false);
    expect(runNeedsYou(run({ status: 'error', summary: undefined }))).toBe(true);
    expect(runNeedsYou(run({ summary: undefined }))).toBe(false);
  });
});

describe('runPreview', () => {
  it('drops a leading heading, strips markers, joins lines', () => {
    expect(runPreview(run({ summary: '## Needs you\n- approve **PR #7**\n- rotate `token`' })))
      .toBe('approve PR #7 · rotate token');
    expect(runPreview(run({ summary: 'Nothing needs you today.' })))
      .toBe('Nothing needs you today.');
  });

  it('uses the detail line for error runs', () => {
    expect(runPreview(run({ status: 'error', detail: 'plan · exit 1 in 9s' })))
      .toBe('plan · exit 1 in 9s');
  });
});
