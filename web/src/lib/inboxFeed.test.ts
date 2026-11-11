import { describe, expect, it } from 'vitest';
import { agentReportToInbox, INBOX_CAP, inboxCountLabel, inboxFeed } from './inboxFeed';
import type { AgentReport, FeedbackEvent, FeedbackReport } from '../api';
import { reportStatusWord } from './myReports';
import type { Schedule } from '../types';

function sched(id: string, at: string, over: Partial<Schedule> = {}): Schedule {
  return {
    id,
    name: `Task ${id}`,
    kind: 'task',
    enabled: true,
    createdAt: '2026-07-01T00:00:00Z',
    fireCount: 1,
    skipCount: 0,
    errorCount: 0,
    runs: [{ at, status: 'fired', summary: 'Nothing new needs you.' }],
    ...over,
  } as Schedule;
}

function fb(number: number, at: string, over: Partial<FeedbackEvent> = {}): FeedbackEvent {
  return { number, title: `Report ${number}`, event: 'needs-info', message: 'which browser?', at, ...over };
}

describe('agent Needs-you reports', () => {
  const report: AgentReport = {
    agentId: 'agt_steward', agentName: 'Board Steward', runId: 'run_1',
    at: '2026-08-21T10:00:00Z', title: 'Needs you — I did not resume it',
    preview: 'wft_6ed95586 needs a decision', items: [{ taskId: 'wft_6ed95586', decision: 'decide-next-step' }],
  };

  it('maps the durable server report into the decision inbox source', () => {
    const item = agentReportToInbox(report);
    expect(item.source).toBe('agentreport');
    expect(item.sender).toBe('Board Steward');
    expect(item.needsYou).toBe(true);
    expect(item.unread).toBe(true);
    expect(item.agentReport).toBe(report);
  });

  it('treats a pending report from an older daemon zero timestamp as unread', () => {
    const legacyPayload = { ...report, ackedAt: '0001-01-01T00:00:00Z' } as AgentReport;
    expect(agentReportToInbox(legacyPayload).unread).toBe(true);
  });

  it('merges agent reports newest-first with the other inbox sources', () => {
    const items = inboxFeed([sched('a', '2026-08-20T09:00:00Z')], [], [report]);
    expect(items.map((i) => i.source)).toEqual(['agentreport', 'report']);
  });

  it('keeps a volume of agent decisions uniquely keyed and interleaved newest-first', () => {
    const reports = Array.from({ length: 10 }, (_, index): AgentReport => ({
      ...report,
      agentId: `agt_steward_${index}`,
      runId: `run_${index}`,
      at: `2026-08-21T10:${String(index).padStart(2, '0')}:00Z`,
    }));
    const items = inboxFeed(
      [sched('a', '2026-08-21T10:07:30Z')],
      [fb(42, '2026-08-21T10:05:30Z')],
      reports,
    );

    expect(items.map((item) => item.at)).toEqual([...items.map((item) => item.at)].sort().reverse());
    expect(new Set(items.map((item) => item.key)).size).toBe(items.length);
    expect(items.filter((item) => item.source === 'agentreport').map((item) => item.key)).toEqual(
      Array.from({ length: 10 }, (_, i) => `agentreport:agt_steward_${9 - i}:run_${9 - i}`),
    );
  });
});

describe('inboxFeed', () => {
  it('is empty with no sources', () => {
    expect(inboxFeed([], [])).toEqual([]);
  });

  it('does not cap: counts and mark-all-read see every retained item', () => {
    const schedules = Array.from({ length: INBOX_CAP + 7 }, (_, i) =>
      sched(`s${i}`, new Date(Date.UTC(2026, 6, 1, 0, i)).toISOString()));
    const items = inboxFeed(schedules, []);
    expect(items).toHaveLength(INBOX_CAP + 7);
    expect(items.filter((i) => i.unread)).toHaveLength(INBOX_CAP + 7);
  });
});

describe('inboxCountLabel', () => {
  it('shows the number up to the cap and "50+" past it', () => {
    expect(inboxCountLabel(0)).toBe('0');
    expect(inboxCountLabel(INBOX_CAP)).toBe('50');
    expect(inboxCountLabel(INBOX_CAP + 1)).toBe('50+');
    expect(inboxCountLabel(195)).toBe('50+');
  });
});

// ST-162: one "My reports" list, the union of event rows and /mine.
describe('My reports', () => {
  const mine = (number: number, status: FeedbackReport['status'], over: Partial<FeedbackReport> = {}): FeedbackReport => ({
    number, title: `Report ${number}`, filedAt: '2026-11-01T10:00:00Z', status, message: `status of ${number}`, ...over,
  });

  it('gives each event row its live status word, and adds reports with no events, quietly', () => {
    const items = inboxFeed([], [fb(42, '2026-11-10T10:00:00Z')], [], [mine(42, 'needs-info'), mine(7, 'closed')]);
    const row42 = items.find((i) => i.number === 42)!;
    expect(row42).toMatchObject({ source: 'feedback', status: 'Needs info', unread: true });
    const row7 = items.find((i) => i.number === 7)!;
    expect(row7).toMatchObject({ source: 'feedback', status: 'Closed', unread: false, needsYou: false, at: '2026-11-01T10:00:00Z' });
    expect(items.filter((i) => i.number === 42)).toHaveLength(1);
    // Newest change first.
    expect(items.map((i) => i.number)).toEqual([42, 7]);
  });

  it('shows no word when the status can\'t be read, and no row is lost without /mine', () => {
    expect(inboxFeed([], [fb(42, '2026-11-10T10:00:00Z')], [], [mine(42, 'unknown')])[0].status).toBeUndefined();
    expect(inboxFeed([], [fb(42, '2026-11-10T10:00:00Z')])[0]).toMatchObject({ number: 42 });
  });

  it.each([
    ['received', undefined, 'Received'],
    ['triaged', undefined, 'Looking into it'],
    ['fix-merged', undefined, 'Looking into it'],
    ['needs-info', undefined, 'Needs info'],
    ['known', undefined, 'Known problem'],
    ['answered', undefined, 'Answered'],
    ['fixed', '0.0.1242', 'Fixed in 0.0.1242'],
    // No public version yet: never "Fixed in".
    ['fixed', undefined, 'Looking into it'],
    ['not-planned', undefined, 'Closed'],
    ['cant-reproduce', undefined, 'Closed'],
    ['closed', undefined, 'Closed'],
    ['unknown', undefined, null],
  ] as const)('status %s (fixedIn %s) reads %s', (status, fixedIn, word) => {
    expect(reportStatusWord({ status, fixedIn })).toBe(word);
  });
});
