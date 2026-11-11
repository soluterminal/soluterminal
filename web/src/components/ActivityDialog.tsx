import { useEffect, useMemo, useState } from 'react';
import { api } from '../api';
import { useStore, type ActivityEvent } from '../store';
import { confirmDialog } from '../lib/dialog';

// Cross-pane Activity feed. Shows what every running agent has been up
// to in reverse-chronological order — the "dashboard" that the
// attention bell (needs-input only) and unread markers (new output
// only) individually can't provide.
//
// Sources:
//   * Ring-buffer snapshot from GET /api/activity on open, so history
//     before this browser session is still visible.
//   * Live SSE appends via the store's pushActivity action (wired in
//     App.tsx where the existing EventSource lives).
//
// Filters are client-side: the feed knows what each event type means.
// Opening the dialog bootstraps once; reopening re-bootstraps so stale
// entries from an earlier session don't linger indefinitely.

interface Props {
  open: boolean;
  onClose: () => void;
}

type FilterKind = 'all' | 'state' | 'tool' | 'other';

export function ActivityDialog({ open, onClose }: Props) {
  const activityLog = useStore((s) => s.activityLog);
  const bootstrapActivity = useStore((s) => s.bootstrapActivity);
  const clearActivity = useStore((s) => s.clearActivity);
  const revealPane = useStore((s) => s.revealPane);

  const [filter, setFilter] = useState<FilterKind>('all');
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // Bootstrap on open. Replace the log with whatever the daemon has;
  // subsequent SSE deliveries append as usual.
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setLoading(true);
    setErr(null);
    api.activity().then((res) => {
      if (cancelled) return;
      bootstrapActivity(res.events);
    }).catch((e) => {
      if (cancelled) return;
      setErr((e as Error).message);
    }).finally(() => {
      if (!cancelled) setLoading(false);
    });
    return () => { cancelled = true; };
  }, [open, bootstrapActivity]);

  // Esc closes. Keep the handler local to the dialog so it doesn't
  // compete with the palette / help / find-bar Esc handlers already
  // installed.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { e.preventDefault(); onClose(); }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, onClose]);

  // Display newest-first — store is oldest-last, reverse on render
  // rather than mutating the store. `rows` re-derives when activityLog
  // or filter change; zustand's ref-equality check ensures this only
  // happens on genuine log updates.
  const rows = useMemo(() => {
    const filtered = activityLog.filter((e) => matchesFilter(e, filter));
    return filtered.slice().reverse();
  }, [activityLog, filter]);

  if (!open) return null;
  return (
    <div className="dialog-backdrop" onMouseDown={onClose}>
      <div
        className="dialog activity-dialog"
        onMouseDown={(e) => e.stopPropagation()}
        style={{ width: 720, maxWidth: '94vw' }}
      >
        <div className="activity-header">
          <h2 style={{ margin: 0 }}>Activity</h2>
          <div className="activity-filters">
            <FilterButton v={filter} setV={setFilter} k="all" label="All" />
            <FilterButton v={filter} setV={setFilter} k="state" label="States" />
            <FilterButton v={filter} setV={setFilter} k="tool" label="Tool calls" />
            <FilterButton v={filter} setV={setFilter} k="other" label="Other" />
          </div>
          <div style={{ flex: 1 }} />
          <button
            type="button"
            onClick={async () => {
              if (await confirmDialog({
                message: 'Clear the activity log shown here? (Daemon ring buffer is untouched.)',
                confirmLabel: 'Clear',
                danger: true,
              })) {
                clearActivity();
              }
            }}
            disabled={activityLog.length === 0}
            title="Clear the view without touching the daemon"
          >Clear</button>
          <button onClick={onClose} title="Close (Esc)" aria-label="Close" style={{ marginLeft: 4 }}>×</button>
        </div>
        {err && <div className="activity-err">Couldn't load: {err}</div>}
        <div className="activity-list" role="list">
          {rows.length === 0 && !loading && !err && (
            <div className="activity-empty">
              {filter === 'all'
                ? 'Nothing logged yet. Activity appears as panes change state and tools run.'
                : 'No events match this filter.'}
            </div>
          )}
          {rows.map((ev, i) => (
            <ActivityRow
              key={(ev.ts ?? 0) + ':' + i}
              ev={ev}
              onReveal={() => {
                if (!ev.paneId) return;
                void revealPane(ev.paneId);
                onClose();
              }}
            />
          ))}
        </div>
        <div className="activity-footer">
          {loading ? 'loading…' : `${rows.length} event${rows.length === 1 ? '' : 's'} shown · newest first · click a pane label to jump`}
        </div>
      </div>
    </div>
  );
}

function FilterButton({ v, setV, k, label }: {
  v: FilterKind;
  setV: (v: FilterKind) => void;
  k: FilterKind;
  label: string;
}) {
  return (
    <button
      type="button"
      className={`activity-filter-btn ${v === k ? 'active' : ''}`}
      onClick={() => setV(k)}
    >{label}</button>
  );
}

function matchesFilter(e: ActivityEvent, f: FilterKind): boolean {
  switch (f) {
    case 'all': return true;
    case 'state': return e.type === 'pane-state';
    case 'tool': return e.type === 'tool-call';
    case 'other': return e.type !== 'pane-state' && e.type !== 'tool-call';
  }
}

function ActivityRow({ ev, onReveal }: { ev: ActivityEvent; onReveal: () => void }) {
  const time = ev.ts ? formatTime(ev.ts) : '';
  const paneLabel = ev.paneTitle || ev.paneId || '';

  let body: React.ReactNode = null;
  let dotClass = 'activity-dot';
  if (ev.type === 'pane-state') {
    dotClass += ` state-${ev.toState || 'idle'}`;
    body = (
      <>
        <span className="activity-from">{ev.fromState}</span>
        <span className="activity-arrow"> → </span>
        <span className="activity-to">{ev.toState}</span>
        {typeof ev.durationMs === 'number' && ev.durationMs > 500 && (
          <span className="activity-dur"> (held {formatDuration(ev.durationMs)})</span>
        )}
      </>
    );
  } else if (ev.type === 'tool-call') {
    dotClass += ' kind-tool';
    // When pane attribution is unresolved (paneTitle empty), fall back
    // to showing the cwd Claude reported. Lets the user see "Claude
    // was in /foo when this ran" even without a pane match, and
    // doubles as a debug hint for why attribution missed.
    const showCwd = !ev.paneTitle && ev.cwd;
    body = (
      <>
        <span className="activity-tool-name">tool: {ev.tool}</span>
        {ev.hint && <span className="activity-tool-hint">({ev.hint})</span>}
        {showCwd && (
          <span
            className="activity-tool-cwd"
            style={{ opacity: 0.55, marginLeft: 8 }}
            title="cwd reported by Claude Code hook — no pane matched this path"
          >in {abbreviateHome(ev.cwd!)}</span>
        )}
      </>
    );
  } else if (ev.type === 'open-file' && ev.path) {
    dotClass += ' kind-open';
    body = <span>open file: {basename(ev.path)}</span>;
  } else if (ev.type === 'schedule-fired') {
    dotClass += ' kind-schedule';
    body = <span>automation ran{ev.text ? ` · ${ev.text}` : ''}</span>;
  } else if (ev.type === 'schedule-firing-soon') {
    dotClass += ' kind-schedule';
    body = <span>automation runs soon{ev.text ? ` · ${ev.text}` : ''}</span>;
  } else if (ev.type === 'toast' && ev.text) {
    dotClass += ' kind-other';
    body = <span className="activity-toast">{ev.text}</span>;
  } else if (ev.type === 'update-available') {
    dotClass += ' kind-other';
    body = <span>update available{ev.text ? ` · ${ev.text}` : ''}</span>;
  } else {
    dotClass += ' kind-other';
    body = <span className="activity-raw">{ev.type}</span>;
  }

  return (
    <div className="activity-row" role="listitem">
      <span className={dotClass} aria-hidden />
      <span className="activity-time">{time}</span>
      {paneLabel ? (
        <button
          type="button"
          className="activity-pane"
          onClick={onReveal}
          title={`Jump to pane: ${paneLabel}`}
        >{paneLabel}</button>
      ) : (
        <span className="activity-pane activity-pane-none">—</span>
      )}
      <span className="activity-body">{body}</span>
    </div>
  );
}

function formatTime(ts: number): string {
  const d = new Date(ts);
  const hh = String(d.getHours()).padStart(2, '0');
  const mm = String(d.getMinutes()).padStart(2, '0');
  const ss = String(d.getSeconds()).padStart(2, '0');
  return `${hh}:${mm}:${ss}`;
}

function formatDuration(ms: number): string {
  if (ms < 60_000) return `${Math.round(ms / 1000)}s`;
  if (ms < 3_600_000) return `${Math.round(ms / 60_000)}m`;
  return `${(ms / 3_600_000).toFixed(1)}h`;
}

function basename(p: string): string {
  const i = p.lastIndexOf('/');
  return i >= 0 ? p.slice(i + 1) : p;
}

function abbreviateHome(p: string): string {
  if (!p) return '';
  return p.replace(/^\/home\/[^/]+/, '~');
}
