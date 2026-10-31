import type { PaneInfo } from '../types';

export function titleShowsHandle(pane: Pick<PaneInfo, 'title' | 'identity'>): boolean {
  const handle = pane.identity?.handle;
  if (!handle) return false;
  // Require a whole @handle token; @api must not match @api-worker.
  return pane.title.split(/[^\p{L}\p{N}_@-]+/u).some(token => token.toLowerCase() === `@${handle}`.toLowerCase());
}

export function messageSharesOrigin(pane: PaneInfo, panes: Record<string, PaneInfo>): boolean {
  const sender = pane.lastAgentMessage?.fromPaneId;
  if (!sender) return false;
  const origin = pane.origin;
  return (!!origin && origin.source !== 'user' && (sender === origin.leadPaneId || sender === origin.byPaneId))
    || panes[sender]?.origin?.leadPaneId === pane.id;
}
