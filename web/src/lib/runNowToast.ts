import type { RunNowOutcome, Schedule } from '../types';
import type { ToastKind } from '../store/toastSlice';

// What the screen says straight after Run now, from the daemon's answer.
// Before this, Run now showed nothing, and a skipped run looked the same as
// one that started.
export function runNowToast(
  sched: Pick<Schedule, 'name' | 'paneId'>,
  runNow: RunNowOutcome,
): { message: string; opts: { kind: ToastKind; icon: string; paneId?: string } } | null {
  const why = runNow.detail?.trim();
  switch (runNow.outcome) {
    case 'started':
      return {
        message: `Started: ${sched.name}${why ? ` · ${why}` : ''}`,
        opts: { kind: 'success', icon: '⏰', paneId: sched.paneId },
      };
    case 'skipped':
      return {
        message: `Skipped: ${sched.name} · ${why || 'the run did not start'}`,
        opts: { kind: 'warn', icon: '⏰', paneId: sched.paneId },
      };
    case 'failed':
      return {
        message: `Run failed: ${sched.name} · ${why || 'the run did not start'}`,
        opts: { kind: 'error', icon: '⚠', paneId: sched.paneId },
      };
    default:
      // An outcome this build does not know: claim nothing.
      return null;
  }
}
