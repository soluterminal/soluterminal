import type { Schedule } from '../types';

// A spent automation will not start another run: a one-time one that ran or
// whose moment passed, a repeat that reached its run limit, or one past its
// end date. The daemon switches these off itself and refuses to switch a spent
// one-time back on, so the list never offers to resume one.
export function scheduleSpent(s: Schedule, now = Date.now()): boolean {
  if (s.enabled) return false;
  if (s.runAt) return s.fireCount > 0 || new Date(s.runAt).getTime() <= now;
  if (s.maxFires && s.maxFires > 0 && s.fireCount >= s.maxFires) return true;
  return !!s.until && new Date(s.until).getTime() <= now;
}

// Finished is spent with nothing still going. One whose last run is still
// going stays with the live ones: clearing it would remove the run's files
// under it.
export function scheduleFinished(s: Schedule, now = Date.now()): boolean {
  return !s.agentRunning && scheduleSpent(s, now);
}

// One word for how it ended. One that never ran (skipped, or paused until its
// time passed) was missed, whether or not a run was recorded.
export function finishedLabel(s: Schedule): 'done' | 'failed' | 'missed' {
  if (s.runs?.[0]?.status === 'error') return 'failed';
  return s.fireCount === 0 ? 'missed' : 'done';
}
