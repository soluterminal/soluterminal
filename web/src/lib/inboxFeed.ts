import type { Schedule } from '../types';
import type { AgentReport, FeedbackEvent, FeedbackReport } from '../api';
import { reportStatusWord } from './myReports';
import { reportsFeed, type ReportFeedItem } from './reportsFeed';

// The inbox: ONE chronological surface for everything addressed to the human.
// Sources today: scheduled-task reports (reportsFeed, one row per schedule),
// feedback-pipeline events (needs-info / fixed, from the reporter loop) and
// managed-agent decisions. Hard inclusion rule: only things a person is meant
// to READ — routine agent telemetry stays out, while an agent's explicit
// Needs-you report is a decision and belongs here; the ◇ Agents cockpit owns
// health. Retired caretaker briefs are not an inbox source.
//
// Each source keeps its OWN read state (schedules: daemon-synced lastReadAt;
// feedback: cloud-side read_at; agent decisions: daemon acknowledgement) —
// the inbox is a view, not a store.

/** The window renders the newest INBOX_CAP rows; counts cover every item. */
export const INBOX_CAP = 50;

/** A count as the inbox shows it: anything past the cap reads "50+". */
export function inboxCountLabel(n: number): string {
  return n > INBOX_CAP ? `${INBOX_CAP}+` : String(n);
}

export interface InboxItem {
  key: string;
  source: 'report' | 'feedback' | 'agentreport';
  /** Who this is from — the schedule's name, "Feedback", or the agent's name. */
  sender: string;
  at: string;
  title: string;
  preview: string;
  unread: boolean;
  needsYou: boolean;
  /** source=report: the underlying feed item (open the reader). */
  report?: ReportFeedItem;
  /** source=feedback: the underlying event (open My reports + mark read). */
  feedback?: FeedbackEvent;
  /** source=feedback: the report's number, and its live status word from My
   *  reports (absent when the status can't be read). */
  number?: number;
  status?: string;
  /** source=agentreport: the durable operator-decision report. */
  agentReport?: AgentReport;
}

export function agentReportToInbox(report: AgentReport): InboxItem {
  return {
    key: `agentreport:${report.agentId}:${report.runId}`,
    source: 'agentreport',
    sender: report.agentName,
    at: report.at,
    title: report.title,
    preview: report.preview ?? '',
    // GET /api/agents/reports is a pending-only feed. Treat every row as unread;
    // this also keeps older daemons' serialized zero time from making a pending
    // decision look acknowledged in JavaScript.
    unread: true,
    needsYou: true,
    agentReport: report,
  };
}

export function feedbackToInbox(ev: FeedbackEvent, live?: FeedbackReport): InboxItem {
  const status = live ? reportStatusWord(live) : null;
  return {
    key: `fb:${ev.number}:${ev.event}`,
    source: 'feedback',
    sender: 'Feedback',
    at: ev.at,
    title: ev.title,
    preview: ev.message,
    unread: !ev.readAt,
    // "fixed" is good news, not a demand; only an open ask needs the user.
    needsYou: ev.event === 'needs-info',
    feedback: ev,
    number: ev.number,
    ...(status ? { status } : {}),
  };
}

/** A report of the person's own with no events yet (filed before the reporter
 *  loop, or closed quietly): a quiet row, never unread. */
export function myReportToInbox(r: FeedbackReport): InboxItem {
  const status = reportStatusWord(r);
  return {
    key: `fbmine:${r.number}`,
    source: 'feedback',
    sender: 'Feedback',
    at: r.filedAt,
    title: r.title,
    // "Closed" over "Closed" says nothing; the row then names the report.
    preview: r.message && r.message !== status ? r.message : `Report #${r.number}`,
    unread: false,
    needsYou: false,
    number: r.number,
    ...(status ? { status } : {}),
  };
}

// Every item, newest first. Not capped: counts and "mark all read" must cover
// the whole retained set (the window slices to INBOX_CAP for rendering only),
// otherwise a busy inbox silently tops out at 50.
export function inboxFeed(
  schedules: Schedule[],
  feedback: FeedbackEvent[],
  agentReports: AgentReport[] = [],
  myReports: FeedbackReport[] = [],
): InboxItem[] {
  const items: InboxItem[] = reportsFeed(schedules).map((r) => ({
    key: `${r.schedule.id}:${r.run.at}`,
    source: 'report' as const,
    sender: r.schedule.name,
    at: r.run.at,
    title: r.schedule.name,
    preview: '', // rows render runPreview(r.run) lazily; kept empty to avoid double work
    unread: r.unread,
    needsYou: r.needsYou,
    report: r,
  }));
  // My reports: every own report once, with its live status; a report with
  // no events yet still shows.
  const live = new Map(myReports.map((r) => [r.number, r]));
  const withEvents = new Set<number>();
  for (const ev of feedback) {
    withEvents.add(ev.number);
    items.push(feedbackToInbox(ev, live.get(ev.number)));
  }
  for (const r of myReports) if (!withEvents.has(r.number)) items.push(myReportToInbox(r));
  for (const report of agentReports) items.push(agentReportToInbox(report));
  items.sort((a, b) => new Date(b.at).getTime() - new Date(a.at).getTime());
  return items;
}
