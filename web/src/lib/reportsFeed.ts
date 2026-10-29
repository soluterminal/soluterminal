import type { Schedule, ScheduleRun } from '../types';

// The cross-schedule reports feed: ONE row per task schedule, carrying its
// newest retained run. The sidebar's Schedules section answers "what will run
// and when"; this answers the daily question — "what came in since I last
// looked" — without making the user remember which schedule to tap. A daily
// schedule used to file a row per run, so one digest showed up seven times and
// its "needs you" counted seven times even though only today's report matters
// (yesterday's asks were replaced by today's). Older runs stay one click away
// in the reader's run cursor. Skips are excluded (nothing was produced); errors
// are included (a failure you haven't seen matters MORE than a digest).

export interface ReportFeedItem {
  schedule: Schedule;
  run: ScheduleRun;
  /** The newest run landed after the schedule's daemon-synced lastReadAt marker. */
  unread: boolean;
  /** An unread run asked for a human or failed — or, once all are read, the newest did. */
  needsYou: boolean;
  /** Older unread runs folded behind this row ("+N earlier"). */
  earlierUnread: number;
}

// Triage straight from run.summary — the daemon's taskReportHint asks every
// task report to open with "## Needs you" only when something NEW needs the
// user (otherwise "Nothing new needs you."), and
// the summary is the report's opening lines, so the contract is machine-
// checkable here without fetching a single log. Errors always need you.
export function runNeedsYou(run: ScheduleRun): boolean {
  if (run.status === 'error') return true;
  return /^##\s*needs you\b/i.test((run.summary || '').trimStart());
}

// One-line preview for feed rows. Drops a leading heading line (the badge
// already says "needs you" — repeating it as text wastes the row), strips
// list markers and emphasis, joins the rest with dots. CSS truncates.
export function runPreview(run: ScheduleRun): string {
  if (run.status === 'error') return run.detail || 'run failed';
  let s = (run.summary || '').trimStart();
  if (s.startsWith('#')) {
    const nl = s.indexOf('\n');
    s = nl === -1 ? '' : s.slice(nl + 1);
  }
  return s
    .split('\n')
    .map((l) => l.replace(/^\s*(?:[-*]|\d+\.|#{1,6}|>)\s+/, '').trim())
    .filter(Boolean)
    .join(' · ')
    .replace(/(\*\*|`|~~)/g, '');
}

export function reportsFeed(schedules: Schedule[]): ReportFeedItem[] {
  const items: ReportFeedItem[] = [];
  for (const s of schedules) {
    if (s.kind !== 'task') continue;
    const readAt = s.lastReadAt ? new Date(s.lastReadAt).getTime() : 0;
    // Runs are stored newest first; sort defensively so a reordered payload
    // can never promote an old report to the row's face.
    const runs = (s.runs || [])
      .filter((r) => r.status !== 'skipped')
      .sort((a, b) => new Date(b.at).getTime() - new Date(a.at).getTime());
    const newest = runs[0];
    if (!newest) continue;
    const isUnread = (r: ScheduleRun) => new Date(r.at).getTime() > readAt;
    const unread = runs.filter(isUnread);
    items.push({
      schedule: s,
      run: newest,
      unread: unread.length > 0,
      // Counted once per schedule. An ask in an older run the user never
      // opened still counts: folding it behind today's row must not hide it.
      needsYou: unread.length > 0 ? unread.some(runNeedsYou) : runNeedsYou(newest),
      earlierUnread: unread.length > 0 ? unread.length - 1 : 0,
    });
  }
  items.sort((a, b) => new Date(b.run.at).getTime() - new Date(a.run.at).getTime());
  return items;
}
