import { useCallback, useEffect, useRef, useState } from 'react';
import { IconLock } from './ui/icons';
import { api } from '../api';
import { useStore } from '../store';
import type { Layout, TurnRecord } from '../types';
import { formatDuration, formatAgo } from '../lib/turnTime';
import { useTurnClock } from '../lib/useTurnClock';

// Read-only "review what changed" tile. Renders `git diff` (or `--cached`)
// for a repo as a file summary + a syntax-coloured unified diff. Strictly a
// viewer: no stage/apply/commit — those stay in the terminal. Built for the
// core loop "Claude edited files → is it good?", reviewable from the phone.
//
// State (repo path + staged flag) lives in the layout leaf, not a store map,
// so the tile is self-contained and survives reloads. The diff text itself is
// always re-fetched on mount and whenever path/staged change.

type DiffLeaf = Extract<Layout, { type: 'diff' }>;

interface DiffData {
  repo: string;
  branch: string;
  staged: boolean;
  files: { path: string; status: string; added: number; removed: number; secret: boolean }[];
  diff: string;
  truncated: boolean;
  hiddenSecret: number;
  // Turn mode only (see Layout diff leaf `turn`).
  turn?: number;
  startedMs?: number;
  endedMs?: number;
  overlapPanes?: string[] | null;
  skippedUntracked?: number;
}

interface Props { leaf: DiffLeaf }

// Cap DOM nodes for huge diffs (the server already caps bytes at 512KB).
const maxDiffLines = 5000;

export function DiffTile({ leaf }: Props) {
  const { diffId, path, staged, turn } = leaf;
  const turnPaneId = turn?.paneId ?? null;
  const turnN = turn?.n ?? 0;
  const turnPaneTitle = useStore((s) => {
    const p = turnPaneId ? s.panes[turnPaneId] : undefined;
    return p ? p.title || p.cmd.join(' ') : null;
  });
  const turnSummary = useStore((s) => turnPaneId ? s.panes[turnPaneId]?.turn : undefined);
  const now = useTurnClock(turnPaneId ? `${turnPaneId}:${turnN}` : undefined);
  const setDiffTurn = useStore((s) => s.setDiffTurn);
  const focused = useStore((s) => s.focusedPaneId === diffId);
  const setFocus = useStore((s) => s.setFocus);
  const setDiffStaged = useStore((s) => s.setDiffStaged);
  const closeDiff = useStore((s) => s.closeDiffInLayout);
  const launchShellHere = useStore((s) => s.launchShellHere);
  const focusedPaneId = useStore((s) => s.focusedPaneId);

  const selection = JSON.stringify([path, staged, turnPaneId, turnN]);
  const selectedRef = useRef(selection);
  selectedRef.current = selection;
  const diffRequest = useRef(0);
  const historyRequest = useRef(0);
  const [result, setResult] = useState<{ selection: string; data: DiffData } | null>(null);
  const data = result?.selection === selection ? result.data : null;
  const [failure, setFailure] = useState<{ selection: string; message: string } | null>(null);
  const err = failure?.selection === selection ? failure.message : null;
  const [loading, setLoading] = useState(false);
  const [history, setHistory] = useState<{ pane: string; turns: TurnRecord[] } | null>(null);
  const [historyFailure, setHistoryFailure] = useState<{ selection: string; message: string } | null>(null);
  const historyError = historyFailure?.selection === selection ? historyFailure.message : null;
  const historyKnown = history?.pane === turnPaneId;
  const retained = history?.pane === turnPaneId ? history.turns : [];
  const position = retained.findIndex((r) => r.n === turnN);
  const previous = position >= 0 ? retained[position + 1] : undefined;
  const next = position > 0 ? retained[position - 1] : undefined;
  const selectedRecord = position >= 0 ? retained[position] : undefined;
  const bodyRef = useRef<HTMLDivElement | null>(null);

  const loadHistory = useCallback(async () => {
    const request = ++historyRequest.current;
    setHistoryFailure(null);
    if (!turnPaneId) { setHistory(null); return; }
    const current = () => request === historyRequest.current && selectedRef.current === selection;
    try {
      const response = await api.paneTurns(turnPaneId);
      if (current()) setHistory({ pane: turnPaneId, turns: response.turns });
    } catch (e) {
      if (current()) { setHistory(null); setHistoryFailure({ selection, message: (e as Error).message }); }
    }
  }, [selection, turnPaneId]);

  const load = useCallback(async () => {
    const request = ++diffRequest.current;
    const current = () => request === diffRequest.current && selectedRef.current === selection;
    setResult(null);
    setFailure(null);
    setLoading(true);
    try {
      const response = turnPaneId ? await api.paneTurnDiff(turnPaneId, turnN) : await api.gitDiff(path, staged);
      if (current()) setResult({ selection, data: response });
    } catch (e) {
      if (current()) setFailure({ selection, message: (e as Error).message });
    } finally {
      if (current()) setLoading(false);
    }
  }, [path, staged, turnPaneId, turnN, selection]);

  const reload = () => { void loadHistory(); void load(); };
  useEffect(() => {
    void loadHistory();
    void load();
    return () => { ++diffRequest.current; ++historyRequest.current; };
  }, [load, loadHistory, turnSummary]);

  function scrollToFile(p: string) {
    const el = bodyRef.current?.querySelector(`[data-diff-path="${cssEscape(p)}"]`);
    el?.scrollIntoView({ block: 'start', behavior: 'smooth' });
  }

  const lines = data ? parseDiffLines(data.diff) : [];
  const capped = lines.length > maxDiffLines;
  const shown = capped ? lines.slice(0, maxDiffLines) : lines;

  const totalAdded = data?.files.reduce((n, f) => n + Math.max(0, f.added), 0) ?? 0;
  const totalRemoved = data?.files.reduce((n, f) => n + Math.max(0, f.removed), 0) ?? 0;

  return (
    <div
      className={`pane file-pane diff-pane ${focused ? 'focused' : ''}`}
      onMouseDownCapture={() => setFocus(diffId)}
    >
      <div className="pane-header file-toolbar">
        <span className="activity-dot idle" title="git diff" />
        <span className="file-path" title={data?.repo || path}>
          {(data?.repo || path).replace(/^\/home\/[^/]+/, '~')}
        </span>
        {data && (
          <span className="file-meta">
            {(turn ? data.branch.replace(/^Turn \d+ · ?/, '') : data.branch) || 'HEAD'} · <span className="diff-add">+{totalAdded}</span>{' '}
            <span className="diff-del">−{totalRemoved}</span>
            {turn && data.endedMs ? (
              <span className="diff-turn-when" title={new Date(data.endedMs).toLocaleString()}>
                {' '}· elapsed {formatDuration((data.endedMs ?? 0) - (data.startedMs ?? 0))} · {formatAgo(data.endedMs, now)}
              </span>
            ) : null}
          </span>
        )}
        {turn ? (
          <span className="diff-turn-nav" role="group" aria-label="Turn">
            <button
              className="diff-turn-btn"
              title={previous ? `Show turn ${previous.n}` : 'No older retained turn'}
              disabled={!previous}
              onClick={(e) => { e.stopPropagation(); setDiffTurn(diffId, { paneId: turn.paneId, n: previous!.n }); }}
            >‹ Prev</button>
            <span className="diff-turn-pos">Turn {turnN}{position >= 0 ? ` · ${position + 1} of ${retained.length} retained` : historyError ? ' · history unavailable' : historyKnown ? ' · not retained' : ' · history loading'}</span>
            <button
              className="diff-turn-btn"
              title={next ? `Show turn ${next.n}` : 'No newer retained turn'}
              disabled={!next}
              onClick={(e) => { e.stopPropagation(); setDiffTurn(diffId, { paneId: turn.paneId, n: next!.n }); }}
            >Next ›</button>
            <button
              className="diff-turn-btn"
              title="Switch to everything currently uncommitted in this repo (git diff)"
              onClick={(e) => { e.stopPropagation(); setDiffTurn(diffId, null); }}
            >All uncommitted</button>
          </span>
        ) : (
        <span className="diff-staged-toggle" role="tablist" aria-label="Diff source">
          <button
            className={`diff-seg ${!staged ? 'on' : ''}`}
            title="Unstaged changes (working tree)"
            onClick={(e) => { e.stopPropagation(); setDiffStaged(diffId, false); }}
          >Working</button>
          <button
            className={`diff-seg ${staged ? 'on' : ''}`}
            title="Staged changes (git diff --cached)"
            onClick={(e) => { e.stopPropagation(); setDiffStaged(diffId, true); }}
          >Staged</button>
        </span>
        )}
        <button
          className="iconbtn"
          title="Reload diff"
          onClick={(e) => { e.stopPropagation(); reload(); }}
        >⟳</button>
        <button
          className="iconbtn"
          title="Open a shell here (to stage / commit / edit)"
          onClick={(e) => {
            e.stopPropagation();
            launchShellHere(focusedPaneId || diffId, 'horizontal', 'policy').catch(() => {});
          }}
        >⎋ shell</button>
        <div className="pane-actions">
          <button
            className="iconbtn"
            title="Close diff tile"
            onClick={(e) => { e.stopPropagation(); closeDiff(diffId); }}
          >×</button>
        </div>
      </div>
      <div className="pane-body diff-body" ref={bodyRef}>
        {historyError && turn && <div className="diff-turn-note" role="status">History unavailable: {historyError}</div>}
        {turn && selectedRecord && !data && <div className="diff-turn-note">Turn {selectedRecord.n} · {new Date(selectedRecord.endedMs).toLocaleString()} · elapsed {formatDuration(selectedRecord.endedMs - selectedRecord.startedMs)} · {formatAgo(selectedRecord.endedMs, now)}{selectedRecord.note ? ` · ${selectedRecord.note}` : ''}</div>}
        {err && (
          <div className="file-error">
            <div><strong>Couldn't load diff:</strong> {err}</div>
            <button className="primary" style={{ marginTop: 8 }} onClick={reload}>Retry</button>
          </div>
        )}
        {!err && !data && loading && <div className="file-loading">loading diff…</div>}
        {turn && data && (
          <div className="diff-turn-banner">
            <strong>Turn {data.turn ?? turnN}</strong>
            {turnPaneTitle ? <> of <span className="diff-turn-pane">{turnPaneTitle}</span></> : null}
            {' '}— repository changes between asynchronous captured snapshots; this does not establish exclusive authorship.
            {data.overlapPanes && data.overlapPanes.length > 0 && (
              <div className="diff-turn-warn">
                {data.overlapPanes.join(', ')} {data.overlapPanes.length === 1 ? 'was' : 'were'} also working in this repo during this turn — some changes may be theirs.
              </div>
            )}
            {!!data.skippedUntracked && (
              <div className="diff-turn-note">
                {data.skippedUntracked} path{data.skippedUntracked > 1 ? 's were' : ' was'} not captured (size, resource or sensitive-file limits).
              </div>
            )}
          </div>
        )}
        {!turn && data && !!data.skippedUntracked && (
          <div className="diff-turn-note">
            {data.skippedUntracked} file{data.skippedUntracked > 1 ? 's are' : ' is'} not shown here (secret-looking, over the size limit, or too many new files).
          </div>
        )}
        {data && data.files.length === 0 && (
          <div className="file-loading">
            {turn ? 'No changes observed between these snapshots.' : `No ${staged ? 'staged' : 'unstaged'} changes in this repo.`}
          </div>
        )}
        {data && data.files.length > 0 && (
          <>
            <div className="diff-filelist">
              {data.files.map((f) => (
                <button
                  key={f.path}
                  className="diff-filerow"
                  title={f.secret ? `${f.path} — hidden (secret file)` : `Jump to ${f.path}`}
                  disabled={f.secret}
                  onClick={() => { if (!f.secret) scrollToFile(f.path); }}
                >
                  <span className={`diff-status diff-status-${(f.status[0] || 'M').toLowerCase()}`}>{f.status || 'M'}</span>
                  <span className="diff-filename">{f.secret && <IconLock size={11} style={{ verticalAlign: '-1px', marginRight: 3 }} />}{f.path}</span>
                  <span className="diff-counts">
                    {f.added >= 0 && <span className="diff-add">+{f.added}</span>}{' '}
                    {f.removed >= 0 && <span className="diff-del">−{f.removed}</span>}
                    {f.added < 0 && <span className="diff-bin">bin</span>}
                  </span>
                </button>
              ))}
            </div>
            {data.hiddenSecret > 0 && (
              <div className="diff-secret-note">
                <IconLock size={12} style={{ verticalAlign: '-2px' }} /> {data.hiddenSecret} secret file{data.hiddenSecret > 1 ? 's' : ''} hidden from this remote diff.
              </div>
            )}
            <div className="diff-hunks">
              {shown.map((ln, i) => (
                <div
                  key={i}
                  className={`diff-line diff-${ln.cls}`}
                  data-diff-path={ln.filePath}
                >{ln.text || ' '}</div>
              ))}
            </div>
            {(capped || data.truncated) && (
              <div className="diff-secret-note">
                Diff truncated for display — open a shell and run <code>git diff</code> for the full output.
              </div>
            )}
          </>
        )}
      </div>
    </div>
  );
}

type ParsedLine = { text: string; cls: string; filePath?: string };

// Classify each unified-diff line for colouring. File-header lines carry the
// repo-relative path so the summary can scroll to them.
function parseDiffLines(diff: string): ParsedLine[] {
  if (!diff) return [];
  const raw = diff.split('\n');
  const out: ParsedLine[] = [];
  for (const line of raw) {
    if (line.startsWith('diff --git')) {
      // "diff --git a/foo b/foo" → capture the b/ path for the anchor.
      const m = /\sb\/(.+)$/.exec(line);
      out.push({ text: line, cls: 'file', filePath: m?.[1] });
    } else if (line.startsWith('index ') || line.startsWith('new file') || line.startsWith('deleted file') || line.startsWith('similarity ') || line.startsWith('rename ')) {
      out.push({ text: line, cls: 'meta' });
    } else if (line.startsWith('--- ') || line.startsWith('+++ ')) {
      out.push({ text: line, cls: 'meta' });
    } else if (line.startsWith('@@')) {
      out.push({ text: line, cls: 'hunk' });
    } else if (line.startsWith('+')) {
      out.push({ text: line, cls: 'add' });
    } else if (line.startsWith('-')) {
      out.push({ text: line, cls: 'del' });
    } else {
      out.push({ text: line, cls: 'ctx' });
    }
  }
  return out;
}

// Minimal CSS attribute-selector escaping (CSS.escape isn't in older
// webviews). Paths only need quotes/backslashes escaped for our selector.
function cssEscape(s: string): string {
  return s.replace(/["\\]/g, '\\$&');
}
