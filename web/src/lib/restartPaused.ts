// "Don't resume" (Settings › Terminal & panes › Sessions › After a restart): a pane the daemon
// suspended across a restart carries `restartPaused`. Its body says what
// happened and where the setting lives, and offers nothing; the pane's ⋯ menu
// still has Resume session.
export const RESTART_PAUSED_TITLE = 'Paused after a restart';
export const RESTART_PAUSED_BODY =
  "Not resumed, as set in Settings › Terminal & panes › Sessions › After a restart. To continue it, choose Resume session in the pane's ⋯ menu.";

export function isRestartPaused(pane: { status: string; resumable?: boolean; restartPaused?: boolean }): boolean {
  return pane.status === 'exited' && !!pane.resumable && !!pane.restartPaused;
}
