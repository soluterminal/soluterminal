import { Fragment, useEffect, useRef, useState } from 'react';
import { useStore } from '../../store';
import type { Schedule } from '../../types';
import { ScheduleDialog } from '../ScheduleDialog';
import { SectionHeader } from './SectionHeader';
import { IconClock, IconPlus, IconDiamond, IconSparkle, IconAlertTriangle, IconPause } from '../ui/icons';
import { api } from '../../api';
import type { AutomationsPause } from '../../types';
import { relativeTime } from './time';
import { confirmDialog } from '../../lib/dialog';
import { describeCron, formatNextFire } from '../../lib/cron';
import { ScheduleHistoryDialog } from '../ScheduleHistoryDialog';
import { useClampedMenuPosition } from '../useClampedMenuPosition';
import { scheduleUnread } from '../../lib/scheduleUnread';
import { finishedLabel, scheduleFinished, scheduleSpent } from '../../lib/scheduleFinished';
import { schedulePermissionChoice } from '../../lib/schedulePermission';
import { RunStatusDot, RunStatusGlyph, scheduleRunVisual } from '../shared/runStatus';
import { describeChange, pausedSince } from '../../lib/scheduleChanges';

interface SchedulesSectionProps {
  collapsed: boolean;
  onToggle: () => void;
}

// Human label for the row's cadence: describeCron covers every builder shape
// plus evenly-spaced minute lists; anything odder shows "custom" with the
// exact expression already in the row tooltip.
function cadenceLabel(s: Schedule): string {
  if (s.runAt) return 'once';
  return describeCron(s.cron || '') ?? 'custom';
}

function endConditionHint(s: Schedule): string | null {
  if (s.runAt) return null;
  if (s.maxFires && s.maxFires > 0) return `${s.fireCount}/${s.maxFires}`;
  if (s.until) {
    const d = new Date(s.until);
    if (isNaN(d.getTime())) return null;
    const now = new Date();
    if (d.toDateString() === now.toDateString()) return 'ends today';
    const diffDays = Math.round((d.getTime() - now.getTime()) / 86400000);
    if (diffDays >= 0 && diffDays < 7) {
      return `ends ${d.toLocaleDateString(undefined, { weekday: 'short' })}`;
    }
    return `ends ${d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })}`;
  }
  return null;
}


// A task schedule a removed managed agent had imported, kept because its
// prompt is not in agents-export.md. It never runs; the person can only read
// its history or delete it (the daemon saves its prompt first).
function keptFromRemovedAgent(s: Schedule): boolean {
  return s.kind === 'task' && !!s.ownedByAgentId;
}

export function SchedulesSection({ collapsed, onToggle }: SchedulesSectionProps) {
  const schedules = useStore((s) => s.schedules);
  const loaded = useStore((s) => s.schedulesLoaded);
  const loadError = useStore((s) => s.schedulesError);
  const refreshSchedules = useStore((s) => s.refreshSchedules);
  const [retrying, setRetrying] = useState(false);
  async function retryLoad() {
    setRetrying(true);
    try {
      await refreshSchedules();
    } finally {
      setRetrying(false);
    }
  }
  const panes = useStore((s) => s.panes);
  const setAgentsPanelOpen = useStore((s) => s.setAgentsPanelOpen);
  const setEnabled = useStore((s) => s.setScheduleEnabled);
  const fireSchedule = useStore((s) => s.fireSchedule);
  const snoozeSchedule = useStore((s) => s.snoozeSchedule);
  const deleteSchedule = useStore((s) => s.deleteSchedule);
  const toast = useStore((s) => s.toast);
  const [dialog, setDialog] = useState<{ open: boolean; editing: Schedule | null; paneId?: string; fromPaneMenu: boolean }>({
    open: false,
    editing: null,
    fromPaneMenu: false,
  });
  // A new automation is seeded from the pane focused when the form opens, read
  // once here: following focus while the form is open would re-seed it under
  // what the person is typing.
  const openNewDialog = () => setDialog({
    open: true,
    editing: null,
    paneId: useStore.getState().focusedPaneId ?? undefined,
    fromPaneMenu: false,
  });
  // Run-history dialog target. Stored as an ID (not the object) so the
  // dialog live-updates as new runs land while it's open.
  const [historyId, setHistoryId] = useState<string | null>(null);
  // Right-click menu on a schedule row — carries the full action set so the
  // hover rail can stay minimal (fire/edit/delete only).
  const [ctxMenu, setCtxMenu] = useState<{ x: number; y: number; id: string } | null>(null);
  const ctxMenuRef = useRef<HTMLDivElement | null>(null);
  const ctxMenuPos = useClampedMenuPosition(ctxMenuRef, ctxMenu);

  useEffect(() => {
    if (!ctxMenu) return;
    const onAway = (e: MouseEvent | KeyboardEvent) => {
      if (e instanceof KeyboardEvent) {
        if (e.key === 'Escape') setCtxMenu(null);
        return;
      }
      const target = e.target as Node | null;
      if (target && ctxMenuRef.current?.contains(target)) return;
      setCtxMenu(null);
    };
    window.addEventListener('mousedown', onAway, { capture: true });
    window.addEventListener('keydown', onAway as EventListener);
    return () => {
      window.removeEventListener('mousedown', onAway, { capture: true });
      window.removeEventListener('keydown', onAway as EventListener);
    };
  }, [ctxMenu]);

  // NOTE: the `solu:open-schedule-dialog` event is handled by the
  // always-mounted ScheduleDialogHost in App — NOT here. This section only
  // mounts when its sidebar slot is visible, so a listener here silently
  // dropped the event for anyone who hid the section (same lesson as
  // ScheduleHistoryDeepLink). The local dialog below serves the section's
  // own New/Edit buttons only.

  const [, setTick] = useState(0);
  useEffect(() => {
    const h = setInterval(() => setTick((t) => (t + 1) % 10000), 30_000);
    return () => clearInterval(h);
  }, []);

  // "Pause all automations" (ST-299): read on open and whenever the list
  // changes (a pause holds rows, a resume re-arms them).
  const [pause, setPause] = useState<AutomationsPause | null>(null);
  useEffect(() => {
    // Nothing to pause or hold without automations (and an older daemon
    // without the route is not asked).
    if (schedules.length === 0) return;
    let gone = false;
    api.getPauseAll().then((p) => { if (!gone) setPause(p); }).catch(() => {});
    return () => { gone = true; };
  }, [schedules]);
  async function setAllPaused(paused: boolean) {
    try {
      setPause(await api.setPauseAll(paused));
      toast(paused ? 'All automations paused' : 'Automations resumed', { kind: 'success' });
    } catch (e) {
      toast(`Couldn't ${paused ? 'pause' : 'resume'} automations: ${(e as Error).message}`, { kind: 'warn', icon: '⚠' });
    }
  }

  async function handleFire(id: string) {
    const s = schedules.find((x) => x.id === id);
    try {
      await fireSchedule(id);
      // A task runs for a while: open its reader, which shows the run live
      // with Stop (ST-299 item 12).
      if (s?.kind === 'task') setHistoryId(id);
    } catch (e) {
      toast(`Run failed: ${(e as Error).message}`, {
        kind: 'error',
        icon: '⚠',
        paneId: s?.paneId,
      });
    }
  }

  async function handleDelete(s: Schedule) {
    if (keptFromRemovedAgent(s)) {
      if (!(await confirmDialog({
        message: `Delete "${s.name}"?\n\nIt was part of a managed agent that SoluTerminal no longer has, and it doesn't run. Its prompt is saved to agents-export-kept.md in SoluTerminal's data folder first; its run history stays on disk.`,
        confirmLabel: 'Delete',
        danger: true,
      }))) return;
      try {
        await deleteSchedule(s.id);
      } catch (e) {
        toast(`Delete failed: ${(e as Error).message}`, { kind: 'error', icon: '⚠' });
      }
      return;
    }
    if (!(await confirmDialog({
      // Deleting removes the automation and its run files; it does not stop a
      // run that has already started.
      message: `Delete automation "${s.name}"?\n\nIt will not run again, and its run history and reports are removed.${s.agentRunning ? ' The run going now is not stopped.' : ''}`,
      confirmLabel: 'Delete',
      danger: true,
    }))) return;
    try {
      await deleteSchedule(s.id);
    } catch (e) {
      toast(`Delete failed: ${(e as Error).message}`, { kind: 'error', icon: '⚠' });
    }
  }

  // Finished automations pile up (every one-time reminder leaves one), so one
  // action removes them all. Deleting removes run history too; say so.
  async function handleClearFinished(finished: Schedule[]) {
    const n = finished.length;
    if (!(await confirmDialog({
      message: `Clear ${n} finished automation${n === 1 ? '' : 's'}?\n\nTheir run history and reports are removed too.`,
      confirmLabel: 'Clear finished',
      danger: true,
    }))) return;
    for (const s of finished) {
      try {
        await deleteSchedule(s.id);
      } catch (e) {
        toast(`Could not clear "${s.name}": ${(e as Error).message}`, { kind: 'error', icon: '⚠' });
      }
    }
  }

  async function handleToggle(s: Schedule) {
    try {
      await setEnabled(s.id, !s.enabled);
    } catch (e) {
      toast(`Toggle failed: ${(e as Error).message}`, { kind: 'error', icon: '⚠' });
    }
  }

  // §7.2's nudge, as an action. Offered only on task schedules with `edit`
  // permission — the §0 inventory rule for "this already acts on your board" —
  // because those are the rows that are autonomous in the sense the Agents
  // surface exists to make legible.
  //
  // The confirm copy leads with what does NOT change, because that is the
  // honest headline: the import gives the schedule a card, a pause button and a
  // history, and alters nothing about how it runs (§8.3 Phase 1).
  async function handleSnooze(s: Schedule) {
    try {
      await snoozeSchedule(s.id);
      toast(`Next run of "${s.name}" will be skipped`, { kind: 'success', icon: '💤', ttlMs: 2000 });
    } catch (e) {
      toast(`Snooze failed: ${(e as Error).message}`, { kind: 'error', icon: '⚠' });
    }
  }

  // Ambient health: red dot on the header when any ENABLED schedule's most
  // recent run errored — silent when healthy, mirroring the agents-cockpit
  // chip doctrine. Paused schedules don't count (their error is stale news).
  const hasFailingSchedule = schedules.some(
    (s) => s.enabled && !keptFromRemovedAgent(s) && s.runs?.[0]?.status === 'error',
  );
  // Enabled schedules only: a paused schedule's last report can never be
  // replaced by a newer one, so counting it kept the chip from reaching 0.
  // Its row still shows its own unread dot.
  const unreadCount = schedules.filter((s) => s.enabled && !keptFromRemovedAgent(s) && scheduleUnread(s)).length;
  // Finished automations sit below the live ones and stay out of the header
  // count: the count answers "how many things run here on their own".
  const finished = schedules.filter((s) => scheduleFinished(s));
  const live = schedules.filter((s) => !scheduleFinished(s));
  // Kept schedules from removed agents are listed but never run.
  const liveCount = live.filter((s) => !keptFromRemovedAgent(s)).length;

  return (
    <div className="sidebar-section">
      <SectionHeader
        icon={<IconClock size={14} />}
        title="Automations"
        count={liveCount || undefined}
        collapsed={collapsed}
        onToggle={onToggle}
        extra={
          <>
            {unreadCount > 0 && (
              <button
                type="button"
                className="sched-new-chip"
                title={`${unreadCount} report${unreadCount === 1 ? '' : 's'} you haven't read — click for the reports feed`}
                onClick={(e) => {
                  e.stopPropagation();
                  window.dispatchEvent(new CustomEvent('solu:open-reports-feed'));
                }}
              >{unreadCount} new</button>
            )}
            {hasFailingSchedule && (
              <RunStatusDot
                visual="error"
                title="An automation's last run failed — open its history for details"
              />
            )}
            {schedules.length > 0 && !pause?.paused && (
              <button
                className="iconbtn"
                onClick={(e) => { e.stopPropagation(); void setAllPaused(true); }}
                title="Pause all automations: none runs until you resume (runs that come due are held, and run once each on resume)"
                aria-label="Pause all automations"
              ><IconPause size={14} /></button>
            )}
            <button
              className="iconbtn"
              onClick={(e) => { e.stopPropagation(); openNewDialog(); }}
              title="New automation"
              aria-label="New automation"
            ><IconPlus size={14} /></button>
          </>
        }
      />
      {!collapsed && pause?.paused && (
        <div role="status" className="sched-paused-banner" style={{ fontSize: 11, padding: '4px 0', display: 'flex', alignItems: 'baseline', gap: 6 }}>
          <span style={{ flex: 1 }}>
            All automations are paused{pause.source === 'autonomy' ? ' (Pause all autonomy)' : ''}{pause.since ? ` since ${new Date(pause.since).toLocaleString(undefined, { weekday: 'short', hour: '2-digit', minute: '2-digit' })}` : ''}. Runs that come due are held and run once each when you resume.
          </span>
          <button
            className="linklike"
            onClick={() => void setAllPaused(false)}
            style={{ background: 'none', border: 'none', padding: 0, font: 'inherit', color: 'var(--accent)', cursor: 'pointer', textDecoration: 'underline' }}
          >Resume</button>
        </div>
      )}
      {/* An empty list means "none" only once a load has succeeded. Before
          that it is still loading, or the load failed; say which. */}
      {!collapsed && loadError && (
        <div role="alert" style={{ fontSize: 11, color: 'var(--fg-dim)' }}>
          {loaded
            ? 'Couldn’t refresh automations. This list may be out of date.'
            : 'Couldn’t load automations.'}{' '}
          <button
            className="linklike"
            disabled={retrying}
            title={loadError}
            onClick={() => void retryLoad()}
            style={{ background: 'none', border: 'none', padding: 0, font: 'inherit', color: 'var(--accent)', cursor: 'pointer', textDecoration: 'underline' }}
          >{retrying ? 'Retrying…' : 'Retry'}</button>
        </div>
      )}
      {!collapsed && !loaded && !loadError && schedules.length === 0 && (
        <div role="status" style={{ fontSize: 11, color: 'var(--fg-dim)' }}>Loading automations…</div>
      )}
      {!collapsed && loaded && schedules.length === 0 && (
        <div style={{ fontSize: 11, color: 'var(--fg-dim)' }}>
          No automations yet.{' '}
          <button
            className="linklike"
            onClick={() => openNewDialog()}
            style={{ background: 'none', border: 'none', padding: 0, font: 'inherit', color: 'var(--accent)', cursor: 'pointer', textDecoration: 'underline' }}
          >Pick a recipe</button>
          {' '}such as a morning digest or a nightly test run, and Claude runs it on time.
        </div>
      )}
      {!collapsed && [...live, ...finished].map((s) => {
        const done = scheduleFinished(s);
        const doneLabel = done ? finishedLabel(s) : null;
        // Spent but its last run is still going: not finished yet, and nothing
        // to resume either.
        const lastRunGoing = !done && scheduleSpent(s);
        const isAgent = s.kind === 'agent';
        const isTask = s.kind === 'task';
        const kept = keptFromRemovedAgent(s);
        const isPrompt = !isAgent && !isTask;
        const target = isPrompt && s.paneId ? panes[s.paneId] : undefined;
        const shortCwd = (s.cwd || '').replace(/^\/home\/[^/]+/, '~');
        const targetLabel = isAgent
          ? `${s.agentId || 'agent'}${s.apply ? '' : ' (dry-run)'}`
          : isTask
            ? `${shortCwd.split('/').pop() || shortCwd}${schedulePermissionChoice(s.permission).rowTag}`
            : target?.title || target?.cmd?.join(' ') || '(closed pane)';
        const targetIcon = isAgent
          ? <IconDiamond size={11} style={{ verticalAlign: '-1px', marginRight: 3 }} />
          : isTask
            ? <IconSparkle size={11} style={{ verticalAlign: '-1px', marginRight: 3 }} />
            : null;
        const targetMissing = isPrompt && !target;
        const lastRun = s.runs?.[0];
        const snoozeActive = !!(s.snoozeUntil && new Date(s.snoozeUntil).getTime() > Date.now());
        const nextLabel = kept
          ? "doesn't run"
          : doneLabel
          ?? (lastRunGoing
            ? 'running…'
            : !s.enabled
              ? 'paused'
              : s.agentRunning
                ? 'running…'
                : s.waitingForPane
                  ? (s.waitingReason ? `waiting: ${s.waitingReason}` : 'waiting for pane')
                : snoozeActive
                  ? 'snoozed'
                  : formatNextFire(s.nextFire));
        const createdRel = relativeTime(new Date(s.createdAt).getTime());
        const lastFiredRel = s.lastFiredAt ? relativeTime(new Date(s.lastFiredAt).getTime()) : null;
        const fireBadge = s.fireCount > 0 ? `${s.fireCount}×` : 'not yet';
        const endHint = endConditionHint(s);
        const endHintTitle = s.runAt
          ? 'One-time automation — runs once then turns itself off'
          : s.maxFires && s.maxFires > 0
            ? `Turns itself off after ${s.maxFires} successful runs (${s.fireCount}/${s.maxFires})`
            : s.until
              ? `Turns itself off at ${new Date(s.until).toLocaleString()}`
              : '';
        const triggerLine = s.runAt
          ? `One-time: ${new Date(s.runAt).toLocaleString()}`
          : `Cron: ${s.cron}`;
        const endLine = s.runAt
          ? null
          : s.maxFires && s.maxFires > 0
            ? `Ends after ${s.maxFires} runs (${s.fireCount}/${s.maxFires})`
            : s.until
              ? `Ends at ${new Date(s.until).toLocaleString()}`
              : null;
        const promptText = s.prompt || '';
        // A paused automation says since when and by whom, when the daemon
        // recorded it. A paused built-in agent is a standing warning: the
        // agent does nothing until someone turns it back on.
        const since = pausedSince(s);
        const paused = !s.enabled && !done && !lastRunGoing;
        const agentPaused = isAgent && paused;
        const recentChanges = (s.changes ?? []).slice(0, 3).map(describeChange);
        const tip = [
          kept ? `Kept from a managed agent (${s.ownedByAgentId}) that SoluTerminal no longer has. It doesn't run; you can read its history or delete it, and its prompt is saved first.` : null,
          triggerLine,
          endLine,
          isAgent
            ? `Agent: ${s.agentId} · mode: ${s.apply ? 'apply (writes to GitHub)' : 'dry-run'}${s.agentRunning ? ' · running now' : ''}`
            : isTask
              ? `Claude task in ${shortCwd} · ${schedulePermissionChoice(s.permission).summary}${s.catchUp ? ' · catches up after downtime' : ''}${s.agentRunning ? ' · running now' : ''}`
              : `Target: ${targetLabel}`,
          s.waitingForPane ? `Waiting for the pane${s.waitingReason ? `: ${s.waitingReason}` : ''}. It goes in when the pane is free, or is skipped after an hour.` : null,
          s.enabled ? `Next: ${s.nextFire ? new Date(s.nextFire).toLocaleString() : '—'}` : done ? 'Finished' : lastRunGoing ? 'Last run still going' : since ? `Paused ${since}` : 'Paused',
          recentChanges.length > 0 ? `Changes: ${recentChanges.join(' · ')}` : null,
          `Created: ${createdRel} (${new Date(s.createdAt).toLocaleString()})`,
          `Ran: ${s.fireCount}${s.skipCount ? ` · Skipped: ${s.skipCount}` : ''}${s.errorCount ? ` · Errors: ${s.errorCount}` : ''}`,
          lastFiredRel ? `Last ran: ${lastFiredRel}` : 'Never ran',
          lastRun ? `Last run: ${lastRun.status}${lastRun.detail ? ' — ' + lastRun.detail : ''}` : '',
          '',
          promptText.length > 200 ? promptText.slice(0, 200) + '…' : promptText,
          '',
          'Click: runs & reports · right-click: more actions',
        ].filter(Boolean).join('\n');
        return (
          <Fragment key={s.id}>
            {done && s.id === finished[0].id && (
              <div className="sched-finished-head">
                <span>Finished</span>
                <button
                  type="button"
                  className="linklike"
                  title="Remove every finished automation and its run history"
                  onClick={() => void handleClearFinished(finished)}
                >Clear finished</button>
              </div>
            )}
            <div
              className={`sched-row ${s.enabled && !kept ? '' : 'disabled'} ${done ? 'finished' : ''} ${targetMissing ? 'target-missing' : ''}`}
              title={tip}
              role="button"
              tabIndex={0}
              onClick={(e) => {
                // The checkbox and hover-rail buttons own their clicks; a tap
                // anywhere else opens runs & reports. This is the ONLY path on
                // iOS, where long-press never fires contextmenu — the report a
                // schedule produced must never be right-click-gated.
                if ((e.target as HTMLElement).closest('button, input, a')) return;
                setHistoryId(s.id);
              }}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && e.target === e.currentTarget) setHistoryId(s.id);
              }}
              onContextMenu={(e) => {
                e.preventDefault();
                e.stopPropagation();
                setCtxMenu({ x: e.clientX, y: e.clientY, id: s.id });
              }}
            >
              {done || lastRunGoing || kept ? (
                // Nothing to switch back on: a spent one-time is refused, and
                // the others would stop again at once. The box keeps its space.
                <span className="sched-toggle sched-toggle-none" aria-hidden="true" />
              ) : (
                <input
                  type="checkbox"
                  checked={s.enabled}
                  onChange={() => handleToggle(s)}
                  title={s.enabled
                    ? 'Pause this automation (keep its settings and counters)'
                    : 'Resume this automation'}
                  className="sched-toggle"
                />
              )}
              <div className="sched-main">
                <div className="sched-name">
                  {scheduleUnread(s) && (
                    <span className="sched-unread-dot" title="New report — click to read" aria-label="New report" />
                  )}
                  {s.name}
                  {kept ? (
                    <span
                      className="sched-paused-tag"
                      title="Part of a managed agent that SoluTerminal no longer has. It doesn't run; delete it from the ⋯ menu (its prompt is saved first)."
                    > · kept from a removed agent</span>
                  ) : doneLabel ? (
                    <span
                      className="sched-paused-tag"
                      title="Finished — it has nothing left to run. Edit it to set a new time, or clear it."
                    > · {doneLabel}</span>
                  ) : !s.enabled && !lastRunGoing && (s.pausedReason?.startsWith('all automations are paused') ? (
                    <span
                      className="sched-paused-tag"
                      title="Held: all automations are paused. It runs once when they resume."
                    > · held</span>
                  ) : s.pausedReason ? (
                    <span
                      className="sched-paused-tag"
                      title={`Paused automatically: ${s.pausedReason}. Edit it to pick another pane, then turn it back on.`}
                    > · paused: {s.pausedReason}</span>
                  ) : agentPaused ? (
                    <span
                      className="sched-paused-tag sched-warn"
                      title={`This built-in agent is paused${since ? ` ${since}` : ''}. It does nothing until you turn it back on.`}
                    > · <IconAlertTriangle size={11} style={{ verticalAlign: '-1px' }} /> paused{since ? ` ${since}` : ''}</span>
                  ) : (
                    <span
                      className="sched-paused-tag"
                      title={`Paused${since ? ` ${since}` : ''} — it will not run again until re-enabled`}
                    > · paused</span>
                  ))}
                  {targetMissing && <span className="sched-warn" title="The pane this automation types into is closed. Edit it to pick another pane."> <IconAlertTriangle size={11} style={{ verticalAlign: '-1px' }} /></span>}
                  {s.tooFrequent && (
                    <span
                      className="sched-paused-tag"
                      title="This automation runs more often than one can be set now (every 5 minutes at most). It keeps running as it is; a new timing must respect the limit."
                    > · runs more often than allowed</span>
                  )}
                </div>
                <div className="sched-sub">
                  <span className="sched-cadence">{cadenceLabel(s)}</span>
                  {endHint && (
                    <>
                      <span className="sched-sep">·</span>
                      <span className="sched-cadence dim" title={endHintTitle}>{endHint}</span>
                    </>
                  )}
                  <span className="sched-sep">·</span>
                  {isAgent ? (
                    <button
                      type="button"
                      className="sched-target linklike"
                      title="Open the Agents cockpit"
                      onClick={() => setAgentsPanelOpen(true)}
                    >{targetIcon}{targetLabel.length > 18 ? targetLabel.slice(0, 16) + '…' : targetLabel}</button>
                  ) : (
                    <span className="sched-target">{targetIcon}{targetLabel.length > 18 ? targetLabel.slice(0, 16) + '…' : targetLabel}</span>
                  )}
                  <span className="sched-sep">·</span>
                  <span className={`sched-next ${s.enabled && !kept ? '' : 'dim'}`}>{nextLabel}</span>
                  <span className="sched-sep">·</span>
                  <span
                    className={`sched-count ${s.fireCount > 0 ? '' : 'dim'}`}
                    title={[
                      `Ran ${s.fireCount} time${s.fireCount === 1 ? '' : 's'} since creation`,
                      s.skipCount ? `${s.skipCount} skipped (pane busy/exited)` : null,
                      s.errorCount ? `${s.errorCount} errored` : null,
                    ].filter(Boolean).join(' · ')}
                  >{fireBadge}</span>
                  {lastRun && (
                    <>
                      <span className="sched-sep">·</span>
                      <span className="sched-last" title={lastRun.detail || lastRun.status}>
                        <RunStatusGlyph
                          visual={scheduleRunVisual(lastRun.status)}
                          size={11}
                          style={{ verticalAlign: '-1px' }}
                        />
                      </span>
                    </>
                  )}
                </div>
              </div>
              {/* One ⋯ overlay button opening the same menu as right-click —
                  NOT a fire/edit/delete rail. The row itself is the click
                  target for runs & reports now, and at the 160px default
                  sidebar width a three-button rail covered ~70% of the row on
                  hover, silently eating those clicks (a stray "Run now" is a
                  bad price for trying to read a report). Also the only menu
                  path on iOS, where long-press never fires contextmenu. */}
              <div className="sched-actions">
                <button
                  className="iconbtn"
                  onClick={(e) => {
                    const r = (e.currentTarget as HTMLElement).getBoundingClientRect();
                    setCtxMenu({ x: r.left, y: r.bottom + 2, id: s.id });
                  }}
                  title="All actions (also on right-click)"
                  aria-label="Automation actions"
                >⋯</button>
              </div>
            </div>
          </Fragment>
        );
      })}

      {ctxMenu && (() => {
        const s = schedules.find((x) => x.id === ctxMenu.id);
        if (!s) return null;
        const snoozeActive = !!(s.snoozeUntil && new Date(s.snoozeUntil).getTime() > Date.now());
        const canSnooze = !s.runAt && s.enabled && !snoozeActive;
        if (keptFromRemovedAgent(s)) {
          return (
            <div
              ref={ctxMenuRef}
              className="pane-ctx-menu"
              role="menu"
              style={{ position: 'fixed', left: ctxMenuPos.left, top: ctxMenuPos.top, zIndex: 'var(--z-menu)' }}
            >
              <button
                role="menuitem"
                onClick={() => { setCtxMenu(null); setHistoryId(s.id); }}
              >History…</button>
              <div className="pane-ctx-menu-sep" />
              <button
                role="menuitem"
                className="danger"
                title="Saves its prompt to agents-export-kept.md, then deletes it"
                onClick={() => { setCtxMenu(null); void handleDelete(s); }}
              >Delete</button>
            </div>
          );
        }
        return (
          <div
            ref={ctxMenuRef}
            className="pane-ctx-menu"
            role="menu"
            style={{ position: 'fixed', left: ctxMenuPos.left, top: ctxMenuPos.top, zIndex: 'var(--z-menu)' }}
          >
            <button
              role="menuitem"
              onClick={() => { setCtxMenu(null); void handleFire(s.id); }}
            >Run now</button>
            {canSnooze && (
              <button
                role="menuitem"
                title="Skip the next run (one occurrence)"
                onClick={() => { setCtxMenu(null); void handleSnooze(s); }}
              >Snooze next run</button>
            )}
            {!scheduleSpent(s) && (
              <button
                role="menuitem"
                onClick={() => { setCtxMenu(null); void handleToggle(s); }}
              >{s.enabled ? 'Pause' : 'Resume'}</button>
            )}
            <button
              role="menuitem"
              onClick={() => { setCtxMenu(null); setDialog({ open: true, editing: s, fromPaneMenu: false }); }}
            >Edit…</button>
            {s.kind === 'agent' && (
              <button
                role="menuitem"
                title="Fleet health and PR queue — the ◇ popover"
                onClick={() => { setCtxMenu(null); setAgentsPanelOpen(true); }}
              >Agents cockpit…</button>
            )}
            {/* The popover mounts from StatusBar's AgentsChip. A hidden chip
                safely clears a stranded open flag; agent schedules are only
                available where the corresponding local agent is present. */}
            <button
              role="menuitem"
              title="Run history — every retained run, with logs for agent runs"
              onClick={() => { setCtxMenu(null); setHistoryId(s.id); }}
            >History…</button>
            <div className="pane-ctx-menu-sep" />
            <button
              role="menuitem"
              className="danger"
              onClick={() => { setCtxMenu(null); void handleDelete(s); }}
            >Delete</button>
          </div>
        );
      })()}

      <ScheduleDialog
        open={dialog.open}
        editing={dialog.editing}
        initialPaneId={dialog.paneId}
        fromPaneMenu={dialog.fromPaneMenu}
        onClose={() => setDialog({ open: false, editing: null, fromPaneMenu: false })}
      />
      {historyId && (
        <ScheduleHistoryDialog
          schedule={schedules.find((s) => s.id === historyId) ?? null}
          onClose={() => setHistoryId(null)}
        />
      )}
    </div>
  );
}
