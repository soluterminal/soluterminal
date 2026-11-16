import { useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { api } from '../api';
import { useStore } from '../store';
import type { Schedule, ScheduleRun } from '../types';
import { relativeTime } from './sidebar/time';
import { renderReportMarkdown } from '../lib/reportMarkdown';
import { RunStatusGlyph, runVisualColor, scheduleRunVisual } from './shared/runStatus';
import { describeChange } from '../lib/scheduleChanges';

// Runs & reports for one schedule — opened by clicking the schedule row
// (and from the row's right-click menu, and by tapping a notification).
//
// Two shapes, because two audiences:
//   • task schedules — the run's output is a REPORT a human reads, so it
//     gets a reader: human header (when · how long · mode), the report in
//     document type, ‹ › paging through past runs, technical meta behind ⓘ.
//     Full-screen on phones — reading in a 92vw card on a 390px screen is
//     exactly the "still not very good UI" this replaced.
//   • agent/prompt schedules — telemetry and pane deliveries. A run LIST:
//     raw mono logs for agent runs, "go to pane" for prompt runs.

interface Props {
  schedule: Schedule | null;
  onClose: () => void;
  /** Open the reader at this run (its `at` timestamp) instead of the newest —
   *  the reports feed's rows target specific runs. Unknown timestamps fall
   *  back to the newest run. Agent/prompt kinds ignore it. */
  initialRunAt?: string;
}

// App-level host for notification deep-links. A tapped schedule push lands
// on ?pane=sched:<id> (cold start) or a SW postMessage (app already open) —
// both re-dispatch as this event. The sidebar's own history dialog can't
// serve this: on phones the drawer — and SchedulesSection with it — may not
// be mounted when the tap arrives. This host is always mounted, subscribes
// to the store, and simply waits: the dialog renders as soon as the
// schedule shows up in a poll, so a tap during bootstrap still lands.
export function ScheduleHistoryDeepLink() {
  const [target, setTarget] = useState<{ id: string; at?: string } | null>(null);
  const schedules = useStore((s) => s.schedules);

  useEffect(() => {
    const handler = (e: Event) => {
      const detail = (e as CustomEvent).detail as { id?: string; at?: string } | undefined;
      if (detail?.id) setTarget({ id: detail.id, at: detail.at });
    };
    window.addEventListener('solu:open-schedule-history', handler);
    return () => window.removeEventListener('solu:open-schedule-history', handler);
  }, []);

  if (!target) return null;
  return (
    <ScheduleHistoryDialog
      // Keyed so a second event retargeting an already-open dialog remounts
      // the reader — its run cursor is initial-state, not a live prop.
      key={`${target.id}|${target.at ?? ''}`}
      schedule={schedules.find((s) => s.id === target.id) ?? null}
      initialRunAt={target.at}
      onClose={() => setTarget(null)}
    />
  );
}

// Task logs open with a `# scheduled task · …` / `# prompt: …` header the
// daemon prepends (schedules_task.go). Peel it off for display — but ONLY
// that exact header: the report body may legitimately start with markdown
// `#` headings of its own.
function splitTaskLog(text: string): { meta: string; body: string } {
  if (text.startsWith('# scheduled task')) {
    const gap = text.indexOf('\n\n');
    if (gap > 0) {
      return { meta: text.slice(0, gap), body: text.slice(gap + 2) };
    }
  }
  return { meta: '', body: text };
}

// "Thu 10 Jul, 09:00" — the report's dateline. Weekday first because the
// reader's question is "is this TODAY's digest?", not the calendar date.
function fmtRunDate(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  const day = d.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' });
  const time = d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false });
  return `${day}, ${time}`;
}

export function ScheduleHistoryDialog({ schedule, onClose, initialRunAt }: Props) {
  // Both shapes' close buttons advertise "Close (Esc)" — honor it here so
  // neither dialog can drift out of sync with the promise.
  useEffect(() => {
    if (!schedule) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { e.preventDefault(); onClose(); }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [schedule, onClose]);
  if (!schedule) return null;
  return schedule.kind === 'task'
    ? <TaskReportReader schedule={schedule} onClose={onClose} initialRunAt={initialRunAt} />
    : <RunListDialog schedule={schedule} onClose={onClose} />;
}

// ---------------------------------------------------------------- reader --

// Dialog shell around TaskReportView — the standalone reader for schedule
// rows and notification deep-links. The inbox embeds TaskReportView directly
// in its reading pane instead (embedded: the host window owns Esc and ×).
function TaskReportReader({ schedule, onClose, initialRunAt }: {
  schedule: Schedule;
  onClose: () => void;
  initialRunAt?: string;
}) {
  // Portaled to <body> (as is RunListDialog below): opened from the sidebar
  // section, which on mobile sits inside the transformed drawer — a transform
  // makes the drawer the containing block for position:fixed, so without the
  // portal the reader rendered squeezed inside the drawer box on phones.
  return createPortal(
    <div className="dialog-backdrop" onMouseDown={onClose}>
      <div
        className="dialog report-reader"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <TaskReportView schedule={schedule} onClose={onClose} initialRunAt={initialRunAt} />
      </div>
    </div>,
    document.body,
  );
}

// The reader itself — header (dateline · status), collapsible run details,
// the report body in reading type, and the act/page footer. Shell-agnostic:
// TaskReportReader wraps it in a dialog; the inbox mounts it as the right
// pane of the two-pane window (embedded hides the × — the window has one).
export function TaskReportView({ schedule, onClose, initialRunAt, embedded }: {
  schedule: Schedule;
  /** Called by × (dialog mode) and after "Act on this" lands in its pane. */
  onClose: () => void;
  initialRunAt?: string;
  embedded?: boolean;
}) {
  const toast = useStore((s) => s.toast);
  const createPane = useStore((s) => s.createPane);
  const [spawning, setSpawning] = useState(false);
  const runs = schedule.runs || [];
  // Cursor by run timestamp, not index: a new run landing mid-read shifts
  // every index but leaves `at` stable — the page the user is on must not
  // change under them.
  const [cursor, setCursor] = useState<string | null>(initialRunAt ?? null);
  const [logs, setLogs] = useState<Record<string, string>>({});
  // run.at → why its report could not be loaded. Shown in place of the
  // report, with Retry; cleared when a retry starts.
  const [logErrors, setLogErrors] = useState<Record<string, string>>({});
  const [showMeta, setShowMeta] = useState(false);
  const fetching = useRef<Set<string>>(new Set());

  const idx = cursor ? runs.findIndex((r) => r.at === cursor) : 0;
  const run: ScheduleRun | undefined = runs[idx === -1 ? 0 : idx];
  const pos = idx === -1 ? 0 : idx;

  function loadReport(target: ScheduleRun) {
    if (!target.logFile || fetching.current.has(target.at)) return;
    fetching.current.add(target.at);
    setLogErrors((m) => {
      if (!(target.at in m)) return m;
      const next = { ...m };
      delete next[target.at];
      return next;
    });
    api.scheduleRunLog(schedule.id, target.logFile)
      .then((text) => setLogs((m) => ({ ...m, [target.at]: text })))
      // The failure stays on the page with Retry. A toast alone left
      // "Loading…" up for good.
      .catch((e) => setLogErrors((m) => ({ ...m, [target.at]: (e as Error).message || 'unknown error' })))
      .finally(() => fetching.current.delete(target.at));
  }

  useEffect(() => {
    if (!run?.logFile || logs[run.at] !== undefined || logErrors[run.at] !== undefined) return;
    loadReport(run);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [schedule.id, run?.at, run?.logFile]);

  // Viewing a run reads it: advance the daemon-synced marker so the sidebar
  // dot and "N new" chip clear — on every device, not just this one. The
  // marker is monotonic server-side; markedRef just avoids duplicate POSTs
  // before the next poll reflects it. Errors ignored: old daemons don't
  // have the route, and an unread dot that lingers is a cosmetic bug.
  const markedRef = useRef<string | null>(null);
  useEffect(() => {
    if (!run) return;
    const readAt = schedule.lastReadAt ? new Date(schedule.lastReadAt).getTime() : 0;
    if (new Date(run.at).getTime() <= readAt || markedRef.current === run.at) return;
    markedRef.current = run.at;
    void api.markScheduleRead(schedule.id, run.at).catch(() => {});
  }, [run, schedule.id, schedule.lastReadAt]);

  async function copyReport(text: string) {
    try {
      await navigator.clipboard.writeText(text);
      toast('Report copied', { kind: 'success' });
    } catch {
      toast('Copy failed — select the text instead', { kind: 'warn', icon: '⚠' });
    }
  }

  const raw = run ? logs[run.at] : undefined;
  const loadFailure = run && raw === undefined ? logErrors[run.at] : undefined;
  const { meta, body } = raw ? splitTaskLog(raw) : { meta: '', body: '' };
  const newer = pos > 0 ? runs[pos - 1] : null;
  const older = pos < runs.length - 1 ? runs[pos + 1] : null;

  // The report is the START of work, not the end of it. One click spawns a
  // Claude session in the schedule's own directory, seeded with the report
  // — "PR #111 needs your click" becomes a session already looking at it.
  // The seed rides argv (interactive claude takes a positional prompt);
  // it always begins with our template text, so the `--`-prefix argv trap
  // that bit `claude -p` can't apply.
  async function actOnReport() {
    if (!run || !body || !schedule.cwd || spawning) return;
    setSpawning(true);
    try {
      const capped = body.length > 12_000 ? body.slice(0, 12_000) + '\n…(report truncated)' : body;
      const seed = `Here's the report my automation "${schedule.name}" produced (${fmtRunDate(run.at)}). `
        + 'Read it, then help me act on it — start with whatever it says needs my decision.\n\n'
        + '--- report ---\n' + capped;
      await createPane({
        cwd: schedule.cwd,
        cmd: ['claude', seed],
        title: `${schedule.name} — follow-up`,
      });
      onClose();
    } catch (e) {
      toast(`Could not open a pane: ${(e as Error).message}`, { kind: 'error', icon: '⚠' });
    } finally {
      setSpawning(false);
    }
  }

  return (
    <>
        <div className="report-reader-top">
          <div style={{ minWidth: 0 }}>
            <div className="report-reader-eyebrow">Report · {schedule.name}</div>
            {run && (
              <h2 className="report-reader-title">
                {run.status !== 'fired' && (
                  <span style={{ marginRight: 6 }}>
                    <RunStatusGlyph visual={scheduleRunVisual(run.status)} size={13} />
                  </span>
                )}
                {fmtRunDate(run.at)}
                <span className="report-reader-when"> · {relativeTime(new Date(run.at).getTime())}</span>
              </h2>
            )}
          </div>
          <div style={{ display: 'flex', gap: 6, alignItems: 'center', flexShrink: 0 }}>
            {raw && (
              <button
                className="reader-btn"
                title="Copy the raw report text"
                onClick={() => void copyReport(raw)}
              >Copy</button>
            )}
            <button
              className="reader-btn"
              title="Run details — mode, directory, prompt, lifetime counts"
              aria-label="Run details"
              onClick={() => setShowMeta((v) => !v)}
            >Details</button>
            {!embedded && (
              <button
                className="reader-btn"
                onClick={onClose}
                title="Close (Esc)"
                aria-label="Close"
                style={{ padding: '5px 9px' }}
              >×</button>
            )}
          </div>
        </div>

        {showMeta && run && (
          <div className="report-reader-meta">
            <div>{run.detail || run.status}</div>
            {meta && <pre>{meta}</pre>}
            <div>
              Lifetime: ran {schedule.fireCount}× · skipped {schedule.skipCount} · errors {schedule.errorCount}
              {runs.length > 0 && ` · last ${runs.length} kept`}
            </div>
            <ScheduleChangesList schedule={schedule} />
          </div>
        )}

        <div className="report-reader-body">
          {!run && (
            <div className="report-reader-empty">
              No reports yet: this automation hasn&apos;t run. Open the row&apos;s ⋯ menu and
              pick &quot;Run now&quot; to get the first one.
            </div>
          )}
          {run && run.status !== 'fired' && (
            <div className="report-reader-callout" style={{ borderColor: runVisualColor(scheduleRunVisual(run.status)) }}>
              {run.status === 'error' ? 'This run failed' : 'This run was skipped'}
              {run.detail ? ` — ${run.detail}` : ''}
            </div>
          )}
          {run && run.logFile && raw === undefined && loadFailure === undefined && (
            <div className="report-reader-empty" role="status">Loading the report…</div>
          )}
          {run && loadFailure !== undefined && (
            <div className="report-reader-empty" role="alert">
              Couldn&apos;t load this report: {loadFailure}{' '}
              <button className="reader-btn" onClick={() => loadReport(run)}>Retry</button>
            </div>
          )}
          {raw !== undefined && body && (
            <div className="report-md report-md-reader">{renderReportMarkdown(body)}</div>
          )}
          {raw !== undefined && !body.trim() && (
            <div className="report-reader-empty">This run&apos;s report is empty.</div>
          )}
          {run && run.status === 'fired' && !run.logFile && (
            <div className="report-reader-empty">This run left no report file.</div>
          )}
        </div>

        {(runs.length > 1 || !!body) && (
          <div className="report-reader-nav">
            <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
              {!!body && !!schedule.cwd && (
                <button
                  className="reader-btn reader-btn-primary"
                  disabled={spawning}
                  onClick={() => void actOnReport()}
                  title={`Open a Claude session in ${schedule.cwd}, seeded with this report`}
                >{spawning ? 'Opening…' : 'Act on this ↗'}</button>
              )}
            </div>
            {runs.length > 1 && (
              <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                <button
                  className="reader-btn"
                  disabled={!older}
                  onClick={() => older && setCursor(older.at)}
                  title={older ? `${fmtRunDate(older.at)}` : 'This is the oldest kept run'}
                >‹ Older</button>
                <span className="report-reader-pos">{pos + 1} of {runs.length}</span>
                <button
                  className="reader-btn"
                  disabled={!newer}
                  onClick={() => newer && setCursor(newer.at)}
                  title={newer ? `${fmtRunDate(newer.at)}` : 'This is the latest run'}
                >Newer ›</button>
              </div>
            )}
          </div>
        )}
    </>
  );
}

// -------------------------------------------------------------- run list --

type RunLog =
  | { state: 'loading' }
  | { state: 'ready'; text: string }
  | { state: 'error'; message: string };

function RunListDialog({ schedule, onClose }: { schedule: Schedule; onClose: () => void }) {
  const panes = useStore((s) => s.panes);
  const revealPane = useStore((s) => s.revealPane);
  const toast = useStore((s) => s.toast);
  // Per-run expanded log: run.at → its text, still loading, or why the load
  // failed (kept on the row with Retry).
  const [logs, setLogs] = useState<Record<string, RunLog>>({});
  // One auto-expand per schedule per open — never re-fight a user who
  // collapsed the log, and re-arm if the dialog is retargeted.
  const autoExpandedFor = useRef<string | null>(null);

  const runs = schedule.runs || [];

  useEffect(() => {
    if (autoExpandedFor.current === schedule.id) return;
    autoExpandedFor.current = schedule.id;
    const newest = runs.find((r) => r.logFile);
    if (newest) void toggleLog(newest);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [schedule.id]);

  async function loadLog(run: ScheduleRun) {
    if (!run.logFile) return;
    setLogs((m) => ({ ...m, [run.at]: { state: 'loading' } }));
    try {
      const text = await api.scheduleRunLog(schedule.id, run.logFile);
      // Only if the row is still open: the person may have hidden it meanwhile.
      setLogs((m) => (run.at in m ? { ...m, [run.at]: { state: 'ready', text } } : m));
    } catch (e) {
      setLogs((m) => (run.at in m
        ? { ...m, [run.at]: { state: 'error', message: (e as Error).message || 'unknown error' } }
        : m));
    }
  }

  async function toggleLog(run: ScheduleRun) {
    if (!run.logFile) return;
    if (logs[run.at] !== undefined) {
      setLogs((m) => {
        const next = { ...m };
        delete next[run.at];
        return next;
      });
      return;
    }
    await loadLog(run);
  }

  async function copyLog(text: string) {
    try {
      await navigator.clipboard.writeText(text);
      toast('Log copied', { kind: 'success' });
    } catch {
      toast('Copy failed — select the text instead', { kind: 'warn', icon: '⚠' });
    }
  }

  return createPortal(
    <div className="dialog-backdrop" onMouseDown={onClose}>
      <div
        className="dialog"
        onMouseDown={(e) => e.stopPropagation()}
        style={{ width: 560, maxWidth: '92vw' }}
      >
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <h2 style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
            Runs — {schedule.name}
          </h2>
          <button
            onClick={onClose}
            title="Close (Esc)"
            aria-label="Close"
            style={{ padding: '1px 8px' }}
          >×</button>
        </div>

        <div style={{ fontSize: 11, color: 'var(--fg-dim)', marginTop: 2 }}>
          Ran {schedule.fireCount}× · skipped {schedule.skipCount} · errors {schedule.errorCount}
          {runs.length > 0 && ` · showing the last ${runs.length}`}
        </div>

        <ScheduleChangesList schedule={schedule} />

        {runs.length === 0 && (
          <div style={{ fontSize: 12, color: 'var(--fg-dim)', marginTop: 10 }}>
            No runs yet: this automation hasn&apos;t run.
          </div>
        )}

        {/* flex:1 + minHeight:0 — if the user resizes the dialog taller,
            the reading area absorbs the space (a fixed-height report box
            inside a stretched dialog reads as a broken empty card). */}
        <div style={{ display: 'flex', flexDirection: 'column', gap: 2, marginTop: 8, flex: '1 1 auto', minHeight: 0, overflow: 'auto' }}>
          {runs.map((run) => {
            const paneAlive = !!(run.paneId && panes[run.paneId]);
            const expanded = logs[run.at];
            return (
              <div key={run.at} style={{ borderBottom: '1px solid var(--border, #333)', padding: '5px 0' }}>
                <div style={{ display: 'flex', alignItems: 'baseline', gap: 8, fontSize: 12 }}>
                  <span style={{ width: 14, textAlign: 'center' }}>
                    <RunStatusGlyph visual={scheduleRunVisual(run.status)} size={13} />
                  </span>
                  <span
                    style={{ whiteSpace: 'nowrap' }}
                    title={new Date(run.at).toLocaleString()}
                  >{relativeTime(new Date(run.at).getTime())}</span>
                  <span style={{ color: 'var(--fg-dim)', flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={run.detail}>
                    {run.detail || run.status}
                  </span>
                  {expanded?.state === 'ready' && !!expanded.text && (
                    <button
                      style={{ fontSize: 10, padding: '1px 8px' }}
                      title="Copy the full log to the clipboard"
                      onClick={() => void copyLog(expanded.text)}
                    >copy</button>
                  )}
                  {run.logFile && (
                    <button
                      style={{ fontSize: 10, padding: '1px 8px' }}
                      onClick={() => void toggleLog(run)}
                    >{expanded !== undefined ? 'hide' : 'view log'}</button>
                  )}
                  {run.paneId && (
                    <button
                      style={{ fontSize: 10, padding: '1px 8px' }}
                      disabled={!paneAlive}
                      title={paneAlive ? 'Focus the pane this run delivered into' : 'Pane no longer exists'}
                      onClick={() => { void revealPane(run.paneId!); onClose(); }}
                    >go to pane</button>
                  )}
                </div>
                {expanded?.state === 'error' && (
                  <div role="alert" style={{ fontSize: 11, color: 'var(--fg-dim)', marginTop: 4 }}>
                    Couldn&apos;t load this log: {expanded.message}{' '}
                    <button
                      style={{ fontSize: 10, padding: '1px 8px' }}
                      onClick={() => void loadLog(run)}
                    >Retry</button>
                  </div>
                )}
                {expanded !== undefined && expanded.state !== 'error' && (
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
                  }}>{expanded.state === 'loading' ? 'Loading the log…' : expanded.text}</pre>
                )}
              </div>
            );
          })}
        </div>
      </div>
    </div>,
    document.body,
  );
}

// Who turned this automation on or off, newest first: the daemon's record
// (Schedule.changes). Nothing is shown for one never switched since records
// began.
function ScheduleChangesList({ schedule }: { schedule: Schedule }) {
  const changes = schedule.changes ?? [];
  if (changes.length === 0) return null;
  return (
    <div className="sched-changes" style={{ fontSize: 11, color: 'var(--fg-dim)', marginTop: 6 }}>
      <div>Turned on or off:</div>
      <ul style={{ margin: '2px 0 0', paddingLeft: 16 }}>
        {changes.slice(0, 5).map((c) => (
          <li key={`${c.at}-${c.action}`} title={new Date(c.at).toLocaleString()}>{describeChange(c)}</li>
        ))}
      </ul>
      {changes.length > 5 && <div>and {changes.length - 5} earlier</div>}
    </div>
  );
}
