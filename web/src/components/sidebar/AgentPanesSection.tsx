import { useRef, useState } from 'react';
import type { PaneInfo } from '../../types';
import { useDismiss } from '../useDismiss';
import { SectionHeader } from './SectionHeader';
import { IconX } from '../ui/icons';
import { confirmDialog } from '../../lib/dialog';
import { formatAgo } from '../../lib/turnTime';
import { confirmClosePane } from '../usePaneContextMenu';
import { livePaneLabel } from '../PaneOriginButton';
import { useStore } from '../../store';
import { paneIdsInOrder } from '../../lib/layout';

// Panes that an agent, a schedule or another program opened (daemon
// pane_origin.go), grouped under the lead they work for, in one place you can
// come back to: what each one is, whether it is working or needs you, and
// which look finished. Panes the person opened themselves stay out of it.

const QUIET_FINISHED_MS = 10 * 60_000;

export function agentStartedPanes(panes: Record<string, PaneInfo>): PaneInfo[] {
  return Object.values(panes)
    .filter((p) => p.origin && p.origin.source !== 'user')
    .sort((a, b) => (b.origin?.at ?? 0) - (a.origin?.at ?? 0));
}

/** A worker that is not working, not waiting on the person, and either lost
 *  its lead or has been quiet for a while. A hint for cleanup, never acted on
 *  automatically: idle does not prove the work is done. */
export function likelyFinished(p: PaneInfo, panes: Record<string, PaneInfo>, now: number): boolean {
  if (p.status === 'exited') return true;
  if (p.state !== 'idle' && p.state !== 'awaiting-input') return false;
  if (p.attention === 'needs-user' || p.attention === 'auto-pending') return false;
  const lead = p.origin?.leadPaneId;
  const leadGone = !!lead && !panes[lead];
  return leadGone || now - (p.lastActivityMs || 0) > QUIET_FINISHED_MS;
}

function statusText(p: PaneInfo, now: number): string {
  if (p.status === 'exited') return 'exited';
  if (p.attention === 'needs-user' || p.state === 'choice-prompt') return 'needs you';
  if (p.state === 'busy' || p.state === 'recent') return 'working';
  return p.lastActivityMs ? `idle ${formatAgo(p.lastActivityMs, now).replace(' ago', '')}` : 'idle';
}

/** What a row says under its title: what it is working on now (else what it
 *  was started for), plus the starter when it is not the group's lead (a
 *  worker a worker started, a schedule, a program). */
export function originLine(p: PaneInfo, panes: Record<string, PaneInfo>, order: string[]): string {
  const o = p.origin!;
  const purpose = p.identity?.workingOn || o.purpose || '';
  let by = '';
  if (o.source === 'agent' && o.byPaneId && o.byPaneId !== o.leadPaneId) by = `started by ${livePaneLabel(o.byPaneId, o.byPaneTitle, panes, order) || 'an agent'}`;
  if (o.source === 'schedule') by = 'started by an automation';
  if (o.source === 'api') by = 'started by another program';
  return [by, purpose].filter(Boolean).join(' · ');
}

export interface AgentPaneGroup {
  /** The lead's pane id, or '' for panes no agent leads (schedules, programs). */
  leadId: string;
  label: string;
  leadOpen: boolean;
  panes: PaneInfo[];
}

/** Workers grouped under the lead they work for (a Squad), newest group first;
 *  panes no agent leads come last under "Other". */
export function groupByLead(list: PaneInfo[], panes: Record<string, PaneInfo>, order: string[]): AgentPaneGroup[] {
  const groups = new Map<string, AgentPaneGroup>();
  for (const p of list) {
    const o = p.origin!;
    const leadId = o.source === 'agent' ? (o.leadPaneId || o.byPaneId || '') : '';
    let g = groups.get(leadId);
    if (!g) {
      g = { leadId, label: leadId ? '' : 'Other', leadOpen: !!leadId && !!panes[leadId], panes: [] };
      groups.set(leadId, g);
    }
    g.panes.push(p);
  }
  // The lead's name: as it is now if open, else the first name any of its
  // workers saved for it (its lead title, or its starter's when that is the lead).
  for (const g of groups.values()) {
    if (!g.leadId) continue;
    const saved = g.panes.map((p) => p.origin?.leadPaneTitle || (p.origin?.byPaneId === g.leadId ? p.origin?.byPaneTitle : '')).find(Boolean);
    g.label = livePaneLabel(g.leadId, saved, panes, order) || 'an agent';
  }
  const newest = (g: AgentPaneGroup) => Math.max(...g.panes.map((p) => p.origin?.at ?? 0));
  return [...groups.values()].sort((a, b) => (a.leadId === '' ? 1 : b.leadId === '' ? -1 : newest(b) - newest(a)));
}

interface AgentPanesSectionProps {
  panes: Record<string, PaneInfo>;
  collapsed: boolean;
  onToggle: () => void;
  onRevealPane: (paneId: string) => void;
  onKillPane: (paneId: string) => Promise<unknown> | void;
  now?: number;
}

export function AgentPanesSection({ panes, collapsed, onToggle, onRevealPane, onKillPane, now = Date.now() }: AgentPanesSectionProps) {
  const [menuOpen, setMenuOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement | null>(null);
  useDismiss(() => setMenuOpen(false), { enabled: menuOpen, inside: [menuRef] });

  const layout = useStore((s) => s.layout);
  const order = paneIdsInOrder(layout);
  const list = agentStartedPanes(panes);
  if (list.length === 0) return null;
  const finished = list.filter((p) => likelyFinished(p, panes, now));

  const closeFinished = async () => {
    setMenuOpen(false);
    const names = finished.map((p) => p.title || p.cmd.join(' ')).join(', ');
    const ok = await confirmDialog({
      title: `Close ${finished.length} finished worker${finished.length === 1 ? '' : 's'}?`,
      message: `${names}\n\nClosing ends their sessions. Files they wrote stay where they are, and Claude and Codex conversations can be resumed.`,
      confirmLabel: 'Close',
      danger: true,
    });
    if (!ok) return;
    for (const p of finished) await onKillPane(p.id);
  };

  return (
    <div className="sidebar-section agent-panes">
      <SectionHeader
        icon={<span aria-hidden="true">↳</span>}
        title="Started by agents"
        count={list.length}
        collapsed={collapsed}
        onToggle={onToggle}
        extra={finished.length > 0 ? (
          <div ref={menuRef} style={{ position: 'relative' }}>
            <button
              type="button"
              className="iconbtn"
              onClick={(e) => { e.stopPropagation(); setMenuOpen((v) => !v); }}
              title="More actions"
              aria-label="Started by agents actions"
              aria-haspopup="menu"
              aria-expanded={menuOpen}
            >⋯</button>
            {menuOpen && (
              <div className="pane-ctx-menu" role="menu" style={{ position: 'absolute', right: 0, top: '100%', zIndex: 'var(--z-menu)' }}>
                <button role="menuitem" className="danger" onClick={() => { void closeFinished(); }}>
                  Close finished workers ({finished.length})…
                </button>
              </div>
            )}
          </div>
        ) : undefined}
      />
      {!collapsed && groupByLead(list, panes, order).map((g) => (
        <div key={g.leadId || 'other'} className="agent-pane-group">
          {g.leadId ? (
            <button type="button" className="agent-pane-lead" disabled={!g.leadOpen} onClick={() => onRevealPane(g.leadId)} title={g.leadOpen ? `Go to ${g.label}` : 'The lead pane is closed'}>
              Lead {g.label}{g.leadOpen ? '' : ' (closed)'} · {g.panes.length} worker{g.panes.length === 1 ? '' : 's'}
            </button>
          ) : (
            <div className="agent-pane-lead">Other</div>
          )}
        <ul className="agent-panes-list">
          {g.panes.map((p) => {
            const done = likelyFinished(p, panes, now);
            const title = p.title || p.cmd.join(' ');
            const line = originLine(p, panes, order);
            return (
              <li key={p.id} className={`agent-pane-row${done ? ' is-finished' : ''}`}>
                <button type="button" className="agent-pane-main" onClick={() => onRevealPane(p.id)} title={`Go to ${title}`}>
                  <span className="agent-pane-title">
                    {title}
                    {p.identity?.handle && <span className="pane-identity-handle"> @{p.identity.handle}</span>}
                  </span>
                  <span className="agent-pane-status">{statusText(p, now)}{done ? ' · done?' : ''}</span>
                  {line && <span className="agent-pane-origin">{line}</span>}
                </button>
                <button
                  type="button"
                  className="iconbtn agent-pane-close"
                  aria-label={`Close ${title}`}
                  title="Close this pane"
                  onClick={() => { void confirmClosePane(p).then((ok) => { if (ok) void onKillPane(p.id); }); }}
                >
                  <IconX size={12} />
                </button>
              </li>
            );
          })}
        </ul>
        </div>
      ))}
    </div>
  );
}
