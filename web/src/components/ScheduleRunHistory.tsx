import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../api';
import { useStore } from '../store';
import type { Schedule, ScheduleHistoryEntry, ScheduleRun } from '../types';
import { useClaudeInstall, ClaudeInstallConfirmCard } from '../claudeBootstrap';
import { describeChange } from '../lib/scheduleChanges';
import {
  RUN_ACTION_LABEL,
  fmtDuration,
  fmtUsage,
  outcomeLabel,
  runCause,
  runOutcome,
  triggerLabel,
  type RunAction,
  type RunOutcome,
} from '../lib/runCauses';
import { RunStatusGlyph, type RunVisual } from './shared/runStatus';

// An automation's history (Automations stage 2, ST-299 items 10 and 11): every
// run and every change, newest first, grouped by day, a page at a time, with a
// failed-only filter. A failed or skipped run says why in plain words and
// offers one button.
//
// The newest runs the row already holds (schedule.runs) show at once, so the
// list is never empty while the first page loads, or when it cannot load.

const PAGE = 50;

const OUTCOME_VISUAL: Record<RunOutcome, RunVisual> = {
  worked: 'ok',
  nothing: 'neutral',
  failed: 'error',
  skipped: 'paused',
};

type RunLog = { state: 'loading' } | { state: 'ready'; text: string } | { state: 'error'; message: string };

interface Props {
  schedule: Schedule;
  /** Task automations: open this run's report in the reader. Without it, a
   *  run's log opens on its row. */
  onOpenReport?: (run: ScheduleRun) => void;
  /** Close the surrounding dialog (before going to a pane or editing). */
  onClose?: () => void;
  /** Open the newest run's log once, on open (the run list for agents and
   *  prompts: their newest log is usually what the person came for). */
  autoExpandNewestLog?: boolean;
}

function dayLabel(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return 'Unknown day';
  const today = new Date();
  const start = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const days = Math.round((start(today) - start(d)) / 86400000);
  if (days === 0) return 'Today';
  if (days === 1) return 'Yesterday';
  return d.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short', year: d.getFullYear() === today.getFullYear() ? undefined : 'numeric' });
}

function timeOf(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime()) ? '' : d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
}

/** The row's newest runs and on/off changes, as history entries, newest
 *  first: what shows before (or without) the first page. */
function seedEntries(schedule: Schedule): ScheduleHistoryEntry[] {
  const runs: ScheduleHistoryEntry[] = (schedule.runs || []).map((run) => ({ type: 'run', at: run.endedAt ?? run.at, run }));
  const changes: ScheduleHistoryEntry[] = (schedule.changes || []).map((change) => ({ type: 'change', at: change.at, change }));
  return [...runs, ...changes].sort((a, b) => new Date(b.at).getTime() - new Date(a.at).getTime());
}

/** Identifies an entry across pages, so a refresh adds only what is new. */
function entryKey(e: ScheduleHistoryEntry): string {
  if (e.type === 'run' && e.run) return `r:${e.run.id || e.run.at}`;
  return `c:${e.at}:${e.change?.action ?? ''}`;
}

export function ScheduleRunHistory({ schedule, onOpenReport, onClose, autoExpandNewestLog }: Props) {
  const panes = useStore((s) => s.panes);
  const revealPane = useStore((s) => s.revealPane);
  const fireSchedule = useStore((s) => s.fireSchedule);
  const toast = useStore((s) => s.toast);
  const claude = useClaudeInstall({ onSpawn: () => onClose?.() });

  const [failedOnly, setFailedOnly] = useState(false);
  const [entries, setEntries] = useState<ScheduleHistoryEntry[] | null>(null); // null: first page not in yet
  const [next, setNext] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [logs, setLogs] = useState<Record<string, RunLog>>({});
  const [retrying, setRetrying] = useState(false);
  // The request that may still write: a filter change or a newer load makes
  // older answers stale.
  const seq = useRef(0);

  const load = useCallback(async (more: boolean) => {
    const mine = ++seq.current;
    setLoading(true);
    setError(null);
    try {
      const page = await api.scheduleHistory(schedule.id, { before: more ? next : undefined, limit: PAGE, failedOnly });
      if (mine !== seq.current) return;
      setEntries((cur) => (more && cur ? [...cur, ...page.entries] : page.entries));
      setNext(page.next);
    } catch (e) {
      if (mine !== seq.current) return;
      setError((e as Error).message || 'unknown error');
    } finally {
      if (mine === seq.current) setLoading(false);
    }
  }, [schedule.id, failedOnly, next]);

  // First page on open, and again on a filter change. A refresh of the newest
  // entries started before the change is dropped when it lands (filterGen).
  const filterGen = useRef(0);
  useEffect(() => {
    filterGen.current++;
    setEntries(null);
    setNext(undefined);
    void load(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [schedule.id, failedOnly]);

  // A new run or an on/off change (on any device: the row is polled): add
  // what is new at the top and keep the older pages already loaded, and their
  // cursor. A page that is loading meanwhile is not cut off.
  const newest = `${schedule.runs?.[0]?.id ?? schedule.runs?.[0]?.at ?? ''}|${schedule.changes?.[0]?.at ?? ''}`;
  const firstNewest = useRef(newest);
  useEffect(() => {
    if (newest === firstNewest.current) return;
    firstNewest.current = newest;
    let gone = false;
    const gen = filterGen.current;
    void api.scheduleHistory(schedule.id, { limit: PAGE, failedOnly }).then((page) => {
      if (gone || gen !== filterGen.current) return;
      setEntries((cur) => {
        if (!cur) return page.entries;
        const have = new Set(cur.map(entryKey));
        const fresh = page.entries.filter((e) => !have.has(entryKey(e)));
        return fresh.length ? [...fresh, ...cur] : cur;
      });
    }).catch(() => { /* the loaded pages stay; the next open shows it */ });
    return () => { gone = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [newest]);

  const shown = useMemo(() => {
    if (entries) return entries;
    const seed = seedEntries(schedule);
    return failedOnly ? seed.filter((e) => e.run && runOutcome(e.run) === 'failed') : seed;
  }, [entries, schedule, failedOnly]);

  const groups = useMemo(() => {
    const out: { day: string; entries: ScheduleHistoryEntry[] }[] = [];
    for (const e of shown) {
      // By the line's own time, the order the history comes in: a run that
      // started before midnight and ended after it is under the later day.
      const day = dayLabel(e.at);
      if (out.length === 0 || out[out.length - 1].day !== day) out.push({ day, entries: [] });
      out[out.length - 1].entries.push(e);
    }
    return out;
  }, [shown]);

  async function loadLog(run: ScheduleRun) {
    if (!run.logFile) return;
    const key = run.id || run.at;
    setLogs((m) => ({ ...m, [key]: { state: 'loading' } }));
    try {
      const text = await api.scheduleRunLog(schedule.id, run.logFile);
      // Only if the row is still open: the person may have hidden it meanwhile.
      setLogs((m) => (key in m ? { ...m, [key]: { state: 'ready', text } } : m));
    } catch (e) {
      setLogs((m) => (key in m ? { ...m, [key]: { state: 'error', message: (e as Error).message || 'unknown error' } } : m));
    }
  }

  async function toggleLog(run: ScheduleRun) {
    if (!run.logFile) return;
    const key = run.id || run.at;
    if (logs[key] !== undefined) {
      setLogs((m) => {
        const n = { ...m };
        delete n[key];
        return n;
      });
      return;
    }
    await loadLog(run);
  }

  // One auto-expand per automation per open: never re-fight a person who
  // collapsed the log.
  const autoExpandedFor = useRef<string | null>(null);
  useEffect(() => {
    if (!autoExpandNewestLog || autoExpandedFor.current === schedule.id) return;
    autoExpandedFor.current = schedule.id;
    const newest = (schedule.runs || []).find((r) => r.logFile);
    if (newest) void loadLog(newest);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [schedule.id, autoExpandNewestLog]);

  async function act(action: RunAction, run: ScheduleRun) {
    switch (action) {
      case 'retry':
        if (retrying) return;
        setRetrying(true);
        try {
          await fireSchedule(schedule.id);
        } catch (e) {
          toast(`Couldn't run it: ${(e as Error).message || 'unknown error'}`, { kind: 'warn', icon: '⚠' });
        } finally {
          setRetrying(false);
        }
        return;
      case 'pick-pane':
      case 'pick-folder':
      case 'edit':
        onClose?.();
        window.dispatchEvent(new CustomEvent('solu:open-schedule-dialog', { detail: { editId: schedule.id } }));
        return;
      case 'install-claude':
        claude.startInstall();
        return;
      case 'open-log':
        if (onOpenReport) onOpenReport(run);
        else void toggleLog(run);
        return;
    }
  }

  return (
    <div className="sched-history" style={{ display: 'flex', flexDirection: 'column', minHeight: 0, flex: '1 1 auto' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 11, color: 'var(--fg-dim)', marginTop: 6 }}>
        <label style={{ display: 'inline-flex', alignItems: 'center', gap: 4, cursor: 'pointer' }}>
          <input
            type="checkbox"
            checked={failedOnly}
            onChange={(e) => setFailedOnly(e.target.checked)}
          />
          Failed only
        </label>
        {loading && entries === null && <span role="status">Loading the history…</span>}
      </div>

      {claude.pendingInstall && (
        <ClaudeInstallConfirmCard
          spec={claude.pendingInstall}
          onConfirm={claude.confirmInstall}
          onCancel={claude.cancelInstall}
        />
      )}

      {error && (
        <div role="alert" style={{ fontSize: 11, color: 'var(--fg-dim)', marginTop: 6 }}>
          Couldn&apos;t load the history: {error}
          {entries === null && shown.length > 0 && ' (showing the newest runs)'}{' '}
          <button style={{ fontSize: 10, padding: '1px 8px' }} onClick={() => void load(entries !== null && !!next)}>Retry</button>
        </div>
      )}

      {!shown.some((e) => e.type === 'run') && (
        <div style={{ fontSize: 12, color: 'var(--fg-dim)', marginTop: 10 }}>
          {failedOnly ? 'No failed runs.' : "No runs yet: this automation hasn't run."}
        </div>
      )}

      <div style={{ display: 'flex', flexDirection: 'column', marginTop: 6, flex: '1 1 auto', minHeight: 0, overflow: 'auto' }}>
        {groups.map((g) => (
          <section key={g.day} aria-label={g.day}>
            <div style={{ fontSize: 10, fontWeight: 600, textTransform: 'uppercase', letterSpacing: 0.4, color: 'var(--fg-dim)', padding: '6px 0 2px' }}>
              {g.day}
            </div>
            {g.entries.map((e, i) => {
              if (e.type === 'change' && e.change) {
                return (
                  <div key={`c-${e.at}-${i}`} style={{ fontSize: 11, color: 'var(--fg-dim)', padding: '3px 0 3px 22px' }}>
                    {describeChange(e.change)}
                  </div>
                );
              }
              const run = e.run;
              if (!run) return null;
              return (
                <RunRow
                  key={run.id || `${run.at}-${i}`}
                  run={run}
                  schedule={schedule}
                  log={logs[run.id || run.at]}
                  paneAlive={!!(run.paneId && panes[run.paneId])}
                  retrying={retrying}
                  hasReader={!!onOpenReport}
                  onAct={(a) => void act(a, run)}
                  onToggleLog={() => void toggleLog(run)}
                  onRetryLog={() => void loadLog(run)}
                  onOpenReport={onOpenReport ? () => onOpenReport(run) : undefined}
                  onGoToPane={() => { void revealPane(run.paneId!); onClose?.(); }}
                />
              );
            })}
          </section>
        ))}
        {entries !== null && next && (
          <button
            style={{ alignSelf: 'center', fontSize: 11, margin: '8px 0', padding: '2px 12px' }}
            disabled={loading}
            onClick={() => void load(true)}
          >{loading ? 'Loading…' : 'Load older'}</button>
        )}
      </div>
    </div>
  );
}

function RunRow({ run, schedule, log, paneAlive, retrying, hasReader, onAct, onToggleLog, onRetryLog, onOpenReport, onGoToPane }: {
  run: ScheduleRun;
  schedule: Schedule;
  log: RunLog | undefined;
  paneAlive: boolean;
  retrying: boolean;
  hasReader: boolean;
  onAct: (a: RunAction) => void;
  onToggleLog: () => void;
  onRetryLog: () => void;
  onOpenReport?: () => void;
  onGoToPane: () => void;
}) {
  const outcome = runOutcome(run);
  // An automation kept from a removed managed agent cannot run or be edited:
  // its history keeps its reports and logs, and offers nothing else.
  const readOnly = !!schedule.ownedByAgentId;
  const cause = runCause(run);
  const meta = [
    fmtDuration(run.durationMs),
    triggerLabel(run),
    run.account,
    run.model,
    fmtUsage(run),
  ].filter(Boolean).join(' · ');
  return (
    <div style={{ borderBottom: '1px solid var(--border, #333)', padding: '5px 0' }} data-outcome={outcome}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 8, fontSize: 12 }}>
        <span style={{ width: 14, textAlign: 'center' }}>
          <RunStatusGlyph visual={OUTCOME_VISUAL[outcome]} size={13} />
        </span>
        <span style={{ whiteSpace: 'nowrap' }} title={new Date(run.at).toLocaleString()}>{timeOf(run.at)}</span>
        <span style={{ fontWeight: 600, whiteSpace: 'nowrap' }}>{outcomeLabel(run, schedule)}</span>
        <span
          style={{ color: 'var(--fg-dim)', flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
          title={run.detail}
        >{run.detail}</span>
        {hasReader && run.logFile && onOpenReport && (
          <button style={{ fontSize: 10, padding: '1px 8px' }} onClick={onOpenReport}>report</button>
        )}
        {log?.state === 'ready' && !!log.text && <CopyLog text={log.text} />}
        {!hasReader && run.logFile && (
          <button style={{ fontSize: 10, padding: '1px 8px' }} onClick={onToggleLog}>
            {log !== undefined ? 'hide' : 'view log'}
          </button>
        )}
        {run.paneId && (
          <button
            style={{ fontSize: 10, padding: '1px 8px' }}
            disabled={!paneAlive}
            title={paneAlive ? 'Focus the pane this run delivered into' : 'Pane no longer exists'}
            onClick={onGoToPane}
          >go to pane</button>
        )}
      </div>
      {meta && <div style={{ fontSize: 10, color: 'var(--fg-dim)', paddingLeft: 22 }}>{meta}</div>}
      {cause && (
        <div style={{ display: 'flex', alignItems: 'baseline', gap: 8, fontSize: 11, paddingLeft: 22, marginTop: 2 }}>
          <span style={{ flex: 1 }}>{cause.words}</span>
          {/* "Open log" is the row's report button where there is a reader. */}
          {cause.action && !(cause.action === 'open-log' && (!run.logFile || hasReader)) && !(readOnly && cause.action !== 'open-log') && (
            <button
              style={{ fontSize: 10, padding: '1px 8px' }}
              disabled={cause.action === 'retry' && retrying}
              onClick={() => onAct(cause.action!)}
            >{RUN_ACTION_LABEL[cause.action]}</button>
          )}
        </div>
      )}
      {log?.state === 'error' && (
        <div role="alert" style={{ fontSize: 11, color: 'var(--fg-dim)', marginTop: 4 }}>
          Couldn&apos;t load this log: {log.message}{' '}
          <button style={{ fontSize: 10, padding: '1px 8px' }} onClick={onRetryLog}>Retry</button>
        </div>
      )}
      {log !== undefined && log.state !== 'error' && (
        <pre style={{
          fontSize: 10,
          fontFamily: "'SF Mono','Menlo',monospace",
          background: 'var(--bg-dim, rgba(127,127,127,0.08))',
          padding: 8,
          marginTop: 4,
          maxHeight: 260,
          overflow: 'auto',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-word',
        }}>{log.state === 'loading' ? 'Loading the log…' : log.text}</pre>
      )}
    </div>
  );
}

function CopyLog({ text }: { text: string }) {
  const toast = useStore((s) => s.toast);
  return (
    <button
      style={{ fontSize: 10, padding: '1px 8px' }}
      title="Copy the full log to the clipboard"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
          toast('Log copied', { kind: 'success' });
        } catch {
          toast('Copy failed — select the text instead', { kind: 'warn', icon: '⚠' });
        }
      }}
    >copy</button>
  );
}
