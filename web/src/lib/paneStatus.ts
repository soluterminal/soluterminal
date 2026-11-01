import type { PaneInfo } from '../types';

/** The same daemon attention signal used by the inbox and attention badges. */
export function isPromptForUser(p: PaneInfo): boolean {
  return (p.state === 'choice-prompt' || p.state === 'awaiting-input') &&
    (p.attention === undefined || p.attention === 'needs-user');
}

export type PaneRowStatus = 'busy' | 'idle' | 'finished' | 'needs-you';
export function paneRowStatus(p: PaneInfo): PaneRowStatus {
  if (p.status === 'exited') return 'finished';
  if (p.attention === 'needs-user' || isPromptForUser(p)) return 'needs-you';
  // 'recent' is only the 3s tail after any screen change, including the user's
  // own typing and composer redraws. 'busy' is fresh output, with immediate
  // keystroke echo already reported idle by the daemon.
  if (p.turn?.active || p.agentMidTurn || p.workflowStatus?.running || p.state === 'busy') return 'busy';
  const last = p.turn?.last;
  // Completion evidence, not an idle timeout: idle alone doesn't mean done.
  if (p.attention !== 'auto-pending' && !p.turn?.uncertain && last && !last.historical && last.outcome !== 'failed' && last.outcome !== 'interrupted') return 'finished';
  return 'idle';
}
