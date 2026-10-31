import { useEffect, useId, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import type { PaneInfo, PaneOrigin } from '../types';
import { useStore } from '../store';
import { formatAgo } from '../lib/turnTime';
import { useAnchoredCard } from './useAnchoredCard';
import { paneIdsInOrder } from '../lib/layout';

// A pane's place in agent work, said in words on its header: "Worker · lead
// #2" on a pane an agent started, "Lead · 2 workers" on a pane whose agent
// started others (yours included), or who else started it (a schedule,
// another program). The card has the details and the lead's workers. A pane
// you opened that leads no one shows nothing: no label means yours.

/** One-line description of the starter, e.g. "Started by #5 Claude". */
export function originSummary(o: PaneOrigin, starter = o.byPaneTitle): string {
  if (o.source === 'agent') return `Started by ${starter || 'another agent'}`;
  if (o.source === 'schedule') return 'Started by a schedule';
  return 'Started by another program';
}

/** A starter or lead as it is now: its number and name while it is open in
 *  this workspace (a rename shows), else the name saved when the pane started. */
export function livePaneLabel(id: string | undefined, saved: string | undefined, panes: Record<string, PaneInfo>, order: string[]): string | undefined {
  const live = id ? panes[id] : undefined;
  if (!live) return saved;
  const name = live.identity?.handle ? `@${live.identity.handle}` : live.title || live.cmd.join(' ');
  const n = order.indexOf(live.id) + 1;
  return n > 0 ? `#${n} ${name}` : name;
}

/** The open panes a lead's agent started (directly or through its workers),
 *  oldest first: the Squad it leads. */
export function workersOf(leadId: string, panes: Record<string, PaneInfo>): PaneInfo[] {
  return Object.values(panes)
    .filter((p) => p.id !== leadId && p.origin?.source === 'agent' && p.origin.leadPaneId === leadId)
    .sort((a, b) => (a.origin?.at ?? 0) - (b.origin?.at ?? 0));
}

const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? '' : 's'}`;

/** The header label, or '' for a pane you opened that leads no one. */
export function roleLabel(pane: PaneInfo, panes: Record<string, PaneInfo>, order: string[]): string {
  const o = pane.origin;
  let label = '';
  if (o?.source === 'agent') {
    if (o.leadPaneId && o.leadPaneId !== pane.id) {
      const lead = livePaneLabel(o.leadPaneId, o.leadPaneTitle, panes, order);
      label = panes[o.leadPaneId] ? `Worker · lead ${lead}` : 'Worker · lead closed';
    } else {
      label = `Worker · started by ${livePaneLabel(o.byPaneId, o.byPaneTitle, panes, order) || 'an agent'}`;
    }
  } else if (o?.source === 'schedule') {
    label = 'Started by a schedule';
  } else if (o?.source === 'api') {
    label = 'Started by a program';
  }
  const n = workersOf(pane.id, panes).length;
  if (n > 0) label = label ? `${label} · ${plural(n, 'worker')}` : `Lead · ${plural(n, 'worker')}`;
  return label;
}

/** The same role in a word or two, for a narrow pane: "Worker", "Lead · 3". */
export function roleShortLabel(pane: PaneInfo, panes: Record<string, PaneInfo>): string {
  const o = pane.origin;
  const n = workersOf(pane.id, panes).length;
  const base = o?.source === 'agent' ? 'Worker' : o?.source === 'schedule' ? 'Schedule' : o?.source === 'api' ? 'Program' : '';
  if (n > 0) return base ? `${base} · ${n}` : `Lead · ${n}`;
  return base;
}

function workerStatus(p: PaneInfo): string {
  if (p.status === 'exited') return 'exited';
  if (p.attention === 'needs-user' || p.state === 'choice-prompt') return 'needs you';
  if (p.state === 'busy' || p.state === 'recent') return 'working';
  return 'idle';
}

export function PaneOriginButton({ pane }: { pane: PaneInfo }) {
  const o = pane.origin;
  const [open, setOpen] = useState(false);
  const titleId = useId();
  const buttonRef = useRef<HTMLButtonElement>(null);
  const cardRef = useRef<HTMLDivElement>(null);
  const panes = useStore((s) => s.panes);
  const layout = useStore((s) => s.layout);
  const revealPane = useStore((s) => s.revealPane);

  const pos = useAnchoredCard(open, buttonRef);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!cardRef.current?.contains(e.target as Node) && !buttonRef.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('click', onDown);
    return () => document.removeEventListener('click', onDown);
  }, [open]);

  const order = paneIdsInOrder(layout);
  const label = roleLabel(pane, panes, order);
  if (!label) return null;
  const workers = workersOf(pane.id, panes);
  const started = !!o && o.source !== 'user';
  const starter = started ? livePaneLabel(o.byPaneId, o.byPaneTitle, panes, order) : undefined;
  const leadLabel = started ? livePaneLabel(o.leadPaneId, o.leadPaneTitle, panes, order) : undefined;
  const summary = started ? originSummary(o, starter) : `Leads ${plural(workers.length, 'worker')} its agent started`;
  const starterAlive = started && !!o.byPaneId && !!panes[o.byPaneId];
  const leadDiffers = started && !!o.leadPaneId && o.leadPaneId !== o.byPaneId;
  const leadAlive = leadDiffers && !!panes[o!.leadPaneId!];
  const close = () => {
    setOpen(false);
    buttonRef.current?.focus();
  };
  const go = (id: string) => {
    setOpen(false);
    void revealPane(id);
  };

  return (
    <span className="pane-origin">
      <button
        ref={buttonRef}
        type="button"
        className="pane-origin-btn"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-label={`${label}. Show details`}
        title={summary}
        onClick={(e) => {
          e.stopPropagation();
          setOpen((v) => !v);
        }}
        onMouseDown={(e) => e.stopPropagation()}
        onDoubleClick={(e) => e.stopPropagation()}
      >
        <span className="pane-role-long">{label}</span>
        <span className="pane-role-short" aria-hidden="true">{roleShortLabel(pane, panes)}</span>
      </button>
      {open && pos && createPortal(
        <div
          ref={cardRef}
          className="pane-origin-card"
          style={{ top: pos.top, left: pos.left }}
          role="dialog"
          aria-labelledby={titleId}
          onClick={(e) => e.stopPropagation()}
          onMouseDown={(e) => e.stopPropagation()}
          onKeyDown={(e) => {
            e.stopPropagation();
            if (e.key === 'Escape') {
              e.preventDefault();
              close();
            }
          }}
        >
          <div id={titleId} className="pane-origin-card-title">{summary}</div>
          {started && o && (
            <dl className="pane-origin-card-rows">
              {o.purpose && (<><dt>Started for</dt><dd>{o.purpose}</dd></>)}
              {leadDiffers && (<><dt>Lead</dt><dd>{leadLabel}{leadAlive ? '' : ' (closed)'}</dd></>)}
              {o.byPaneId && !starterAlive && (<><dt>Starter</dt><dd>closed</dd></>)}
              {o.workspace && (<><dt>Workspace</dt><dd>{o.workspace}</dd></>)}
              {o.at > 0 && (<><dt>When</dt><dd>{formatAgo(o.at)}</dd></>)}
            </dl>
          )}
          {workers.length > 0 && (
            <>
              <div className="pane-origin-card-heading">Its workers</div>
              <ul className="pane-origin-workers">
                {workers.map((w) => (
                  <li key={w.id}>
                    <span className="pane-origin-worker-title" title={w.identity?.workingOn}>
                      {livePaneLabel(w.id, w.title, panes, order)}
                      {w.identity?.handle && <span className="pane-identity-handle"> @{w.identity.handle}</span>}
                      {w.identity?.workingOn && <span className="pane-origin-worker-doing">{w.identity.workingOn}</span>}
                    </span>
                    <span className="pane-origin-worker-status">{workerStatus(w)}</span>
                    <button type="button" onClick={() => go(w.id)}>Go to</button>
                  </li>
                ))}
              </ul>
            </>
          )}
          <div className="pane-origin-card-actions">
            {starterAlive && o?.byPaneId && (
              <button type="button" autoFocus onClick={() => go(o.byPaneId!)}>Go to {starter || 'starter'}</button>
            )}
            {leadAlive && o?.leadPaneId && (
              <button type="button" onClick={() => go(o.leadPaneId!)}>Go to lead</button>
            )}
            <button type="button" onClick={close} autoFocus={!starterAlive}>Close</button>
          </div>
        </div>,
        document.body,
      )}
    </span>
  );
}
