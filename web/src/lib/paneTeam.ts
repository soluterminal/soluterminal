import type { PaneInfo } from '../types';

/** A pane's place in agent work (daemon pane_team.go). A lead is a long-lived
 *  role pane and is never closed automatically; a worker is a task for its
 *  lead; '' is the user's own pane. */
export type PaneTeamKind = 'lead' | 'worker' | '';

/** The kind the daemon acts on (pane_team.go paneTeamKind), or '' when it
 *  has none: #850 auto workers, with a worker record, are workers. */
export function declaredPaneTeamKind(p: PaneInfo | undefined): PaneTeamKind {
  const kind = p?.origin?.teamKind;
  if (kind === 'lead' || kind === 'worker') return kind;
  // Any started pane with a worker record (an auto worker whose caller had
  // closed is an 'api' pane), never your own.
  return p?.origin?.worker && p.origin.source && p.origin.source !== 'user' ? 'worker' : '';
}

/** The kind the sidebar and headers show. Agent-started panes without a
 *  recorded kind (auto workers, and panes from before kinds) stay workers,
 *  nested as before. */
export function paneTeamKind(p: PaneInfo | undefined): PaneTeamKind {
  const declared = declaredPaneTeamKind(p);
  if (declared) return declared;
  // 'api' and 'schedule' panes without a worker record stay top level.
  return p?.origin?.source === 'agent' ? 'worker' : '';
}

export function paneLeadRole(p: PaneInfo | undefined): string {
  return declaredPaneTeamKind(p) === 'lead' ? p?.origin?.leadRole ?? '' : '';
}

/** The lead a pane works for: the first running pane up its chain of
 *  starters that is not a worker (a sub-lead, a lead or your own pane), else the top
 *  of its chain (origin.leadPaneId, which caps and authority key on). Matches
 *  the daemon's workerLeadFor, which decides who reads a worker's result. */
export function nearestLeadId(p: PaneInfo, panes: Record<string, PaneInfo>): string | undefined {
  const o = p.origin;
  if (o?.source !== 'agent') return undefined;
  let id = o.byPaneId;
  for (let hops = 0; id && id !== p.id && panes[id]?.status === 'running' && hops < 32; hops++) {
    if (paneTeamKind(panes[id]) !== 'worker') return id;
    id = panes[id].origin?.byPaneId;
  }
  return o.leadPaneId && o.leadPaneId !== p.id ? o.leadPaneId : undefined;
}
