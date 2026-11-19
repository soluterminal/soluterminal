import type { Schedule, ScheduleRun } from '../types';

// Automations stage 2 (ST-299): what a run came to, in words, and the one
// thing the person can do about a failure. The daemon sends stable codes
// (ScheduleRun.outcome, .cause; daemon/schedules_history.go); this file is
// the only place they turn into words.

export type RunOutcome = 'worked' | 'nothing' | 'failed' | 'skipped';

/** The run's outcome, from the daemon's field or (older daemons) its status. */
export function runOutcome(run: ScheduleRun): RunOutcome {
  if (run.outcome) return run.outcome;
  if (run.status === 'error') return 'failed';
  if (run.status === 'skipped') return 'skipped';
  return 'worked';
}

/** The outcome as the run's chip reads it. A prompt that worked was sent. */
export function outcomeLabel(run: ScheduleRun, schedule: Pick<Schedule, 'kind'>): string {
  switch (runOutcome(run)) {
    case 'nothing': return 'Nothing to do';
    case 'failed': return 'Failed';
    case 'skipped': return 'Skipped';
  }
  return !schedule.kind || schedule.kind === 'prompt' ? 'Sent' : 'Worked';
}

/** The one action a failure or skip offers. */
export type RunAction = 'retry' | 'pick-pane' | 'pick-folder' | 'install-claude' | 'edit' | 'open-log';

export const RUN_ACTION_LABEL: Record<RunAction, string> = {
  retry: 'Retry now',
  'pick-pane': 'Pick another pane',
  'pick-folder': 'Pick a folder',
  'install-claude': 'Install Claude',
  edit: 'Edit automation',
  'open-log': 'Open log',
};

interface CauseInfo {
  /** Plain words: why it failed or was skipped. */
  words: string;
  /** The one button, or none when nothing the person does helps now. */
  action?: RunAction;
}

const CAUSES: Record<string, CauseInfo> = {
  'pane-gone': { words: 'Its pane was closed, so the automation is paused.', action: 'pick-pane' },
  'pane-busy': { words: 'The pane was busy.', action: 'retry' },
  'pane-menu': { words: 'The pane was waiting on an answer.', action: 'retry' },
  'pane-typing': { words: 'You were typing in the pane, or its input box had text.', action: 'retry' },
  'pane-not-ready': { words: 'The pane was not ready for it.', action: 'retry' },
  'claude-missing': { words: 'Claude Code is not installed here.', action: 'install-claude' },
  'folder-gone': { words: 'Its folder no longer exists.', action: 'pick-folder' },
  'bad-setting': { words: 'One of its settings can no longer be used.', action: 'edit' },
  'usage-limit': { words: 'Your Claude usage limit was reached; task runs resume after it resets.' },
  timeout: { words: 'It ran past its time limit and was stopped.', action: 'retry' },
  'temporary-error': { words: 'Claude had a temporary error.', action: 'retry' },
  'run-failed': { words: 'The run stopped with an error.', action: 'retry' },
  'reported-failed': { words: 'Its report says it could not finish.', action: 'open-log' },
  'start-failed': { words: 'The run could not start.', action: 'retry' },
  'already-running': { words: 'Its previous run was still going.' },
  'too-many-runs': { words: 'Too many automations were running at once.', action: 'retry' },
  'agent-missing': { words: 'The agent is not installed on this computer.' },
  'kill-switch': { words: 'It is paused by its kill switch.' },
  snoozed: { words: 'You snoozed this run.' },
  ended: { words: 'Its end date had passed, so it stopped.' },
  'missed-app-off': { words: 'SoluTerminal was not running at that time.' },
  'missed-asleep': { words: 'The computer was asleep at that time.' },
  stopped: { words: 'You stopped it.', action: 'retry' },
  interrupted: { words: 'SoluTerminal stopped while it was running.', action: 'retry' },
  internal: { words: 'SoluTerminal could not run it.', action: 'retry' },
};

/**
 * Why a run failed or was skipped, and its one button. A run with a log
 * whose cause offers nothing better offers "Open log". Undefined for a run
 * that worked or an unknown cause without a log.
 */
export function runCause(run: ScheduleRun): CauseInfo | undefined {
  const outcome = runOutcome(run);
  if (outcome === 'worked' || outcome === 'nothing') return undefined;
  const info = run.cause ? CAUSES[run.cause] : undefined;
  if (info) {
    if (!info.action && run.logFile && outcome === 'failed') return { ...info, action: 'open-log' };
    return info;
  }
  if (outcome === 'failed') return { words: 'The run failed.', action: run.logFile ? 'open-log' : 'retry' };
  return undefined;
}

/** "42 s", "3 min 5 s", "1 h 4 min"; "" under a second. */
export function fmtDuration(ms: number | undefined): string {
  if (!ms || ms < 0) return '';
  const s = Math.round(ms / 1000);
  // Under a second (a prompt typed into its pane) says nothing useful.
  if (s < 1) return '';
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 ? `${m} min ${s % 60} s` : `${m} min`;
  const h = Math.floor(m / 60);
  return m % 60 ? `${h} h ${m % 60} min` : `${h} h`;
}

/** "12.3k tokens · $0.04". Empty when the run reported nothing. */
export function fmtUsage(run: ScheduleRun): string {
  const u = run.usage;
  if (!u) return '';
  const tokens = (u.inputTokens || 0) + (u.outputTokens || 0) + (u.cacheReadTokens || 0) + (u.cacheWriteTokens || 0);
  const parts: string[] = [];
  if (tokens > 0) {
    parts.push(tokens >= 1000 ? `${(tokens / 1000).toFixed(tokens >= 100000 ? 0 : 1)}k tokens` : `${tokens} tokens`);
  }
  if (typeof u.costUsd === 'number') {
    parts.push(u.costUsd > 0 && u.costUsd < 0.01 ? '<$0.01' : `$${u.costUsd.toFixed(2)}`);
  }
  return parts.join(' · ');
}

/** What started the run, when it was not the timetable. */
export function triggerLabel(run: ScheduleRun): string {
  switch (run.trigger) {
    case 'run-now': return 'Run now';
    case 'retry': return 'after waiting';
    case 'catch-up': return 'catch-up';
  }
  return '';
}
