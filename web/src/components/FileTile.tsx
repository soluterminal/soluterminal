import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import {
  IconSearch, IconLock, IconPencil, IconEye, IconEyeOff, IconMaximize, IconMinimize,
} from './ui/icons';
import { api } from '../api';
import { isSaveApprovalRefusal, saveFailureText } from '../lib/saveRefusalCopy';
import { useStore } from '../store';
import type { FileTileInfo } from '../types';
import { detectLang, tokenizeLine, type Tok } from '../lib/highlight';
import { fileSearchMatches } from '../lib/fileSearch';
import { collectLeaves } from '../lib/layout';
import './fileHighlight.css';

// Line-level file viewer with deliberate one-line-at-a-time editing.
// Design commitments (see conversation that introduced this feature):
//   - No "edit mode for the whole file" — each edit is a single input
//     on a single line, committed one at a time. Prevents accidental
//     refactors in a tool that isn't meant to be an IDE.
//   - `.env`-style files auto-detect and mask values by default. Click
//     eye icon per row to reveal, or copy to clipboard without revealing.
//   - mtime-based optimistic concurrency: client passes the loaded mtime
//     with each write; server rejects with 409 if the file changed.
//     We refetch on conflict so the user sees the new state.

interface LoadedFile {
  path: string;
  lines: string[];
  mtime: number;
  isEnv: boolean;
  size: number;
  trailingNewline: boolean;
}

interface Props { info: FileTileInfo }

export function FileTile({ info }: Props) {
  const focused = useStore((s) => s.focusedPaneId === info.id);
  const setFocus = useStore((s) => s.setFocus);
  const closeFile = useStore((s) => s.closeFileInLayout);
  const setFilePath = useStore((s) => s.setFilePath);
  const launchShellHere = useStore((s) => s.launchShellHere);
  const focusedPaneId = useStore((s) => s.focusedPaneId);
  const toast = useStore((s) => s.toast);
  const isZoomed = useStore((s) => s.zoomedPaneId === info.id);
  const layout = useStore((s) => s.layout);
  const leafCount = useMemo(() => collectLeaves(layout).length, [layout]);
  const canZoom = leafCount > 1 || isZoomed;
  const toggleZoom = useStore((s) => s.toggleZoom);

  const [file, setFile] = useState<LoadedFile | null>(null);
  const [err, setErr] = useState<{ msg: string; refused: boolean } | null>(null);
  const [editingLine, setEditingLine] = useState<number | null>(null);
  const [draft, setDraft] = useState('');
  const [revealed, setRevealed] = useState<Record<number, boolean>>({});
  const [pathDraft, setPathDraft] = useState(info.path);
  const [editingPath, setEditingPath] = useState(false);
  // Viewer polish (Theme B): in-file search/jump, line-wrap toggle.
  const [searchOpen, setSearchOpen] = useState(false);
  const [search, setSearch] = useState('');
  const [matchIdx, setMatchIdx] = useState(0);
  const [wrap, setWrap] = useState(true); // default on: long lines never dead-end
  const linesRef = useRef<HTMLDivElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);

  // Syntax highlighting: detect language from the path, then pre-tokenize
  // every line once (memoized on content) so re-renders from edits/reveals
  // don't re-tokenize. Env files use the masking path and unknown types fall
  // back to plain — both leave `highlighted` null (no per-line spans).
  const lang = useMemo(() => (file ? detectLang(file.path) : 'plain'), [file?.path]);
  const highlighted = useMemo<Tok[][] | null>(() => {
    if (!file || file.isEnv || lang === 'plain') return null;
    return file.lines.map((l) => tokenizeLine(l, lang));
  }, [file?.lines, file?.isEnv, lang]);

  const load = useCallback(async (path: string) => {
    setErr(null);
    try {
      const r = await api.readFile(path);
      setFile(r);
      setEditingLine(null);
      setRevealed({});
    } catch (e) {
      setFile(null);
      const status = (e as { status?: number }).status;
      const msg = (e as Error).message;
      // A 403 (or the server's "refusing to…" message) means this is a
      // secret/credential file the daemon won't serve over the remote/cloud
      // connection. That's a deliberate, permanent policy — not a transient
      // error — so we don't offer Retry; we point at the shell instead.
      const refused = status === 403 || /refusing to/i.test(msg);
      setErr({ msg, refused });
    }
  }, []);

  useEffect(() => { void load(info.path); }, [info.path, load]);
  useEffect(() => { setPathDraft(info.path); }, [info.path]);

  // Search: case-insensitive substring match across lines. A bare ":42"
  // becomes a jump-to-line (clamped to range) so agent references like
  // "see auth.go:153" can be navigated by hand here too.
  const matches = useMemo<number[]>(
    () => (file ? fileSearchMatches(file.lines, search) : []),
    [file?.lines, search],
  );
  const matchSet = useMemo(() => new Set(matches), [matches]);
  const currentLn = matches.length ? matches[Math.min(matchIdx, matches.length - 1)] : -1;
  const isJump = /^:\d+$/.test(search.trim());
  const searchCountLabel = !search.trim()
    ? ''
    : isJump
      ? (matches.length ? `→ ${matches[0] + 1}` : 'none')
      : (matches.length ? `${Math.min(matchIdx, matches.length - 1) + 1}/${matches.length}` : '0');

  // Reset to the first hit whenever the query (or file) changes.
  useEffect(() => { setMatchIdx(0); }, [search, file?.path]);
  // Keep the current match scrolled into view.
  useEffect(() => {
    if (!searchOpen || currentLn < 0) return;
    const el = linesRef.current?.querySelector(`[data-ln="${currentLn}"]`) as HTMLElement | null;
    el?.scrollIntoView({ block: 'center' });
  }, [currentLn, searchOpen]);
  // Ctrl/Cmd+F opens the find bar — but only for the focused tile, so we
  // don't fight other panes for the shortcut.
  useEffect(() => {
    if (!focused) return;
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && (e.key === 'f' || e.key === 'F')) {
        e.preventDefault();
        setSearchOpen(true);
        setTimeout(() => searchRef.current?.focus(), 0);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [focused]);

  function openSearch() { setSearchOpen(true); setTimeout(() => searchRef.current?.focus(), 0); }
  function closeSearch() { setSearchOpen(false); }
  function go(delta: number) {
    if (!matches.length) return;
    setMatchIdx((m) => { const len = matches.length; return ((m + delta) % len + len) % len; });
  }

  const refusalToast = useRef<string | null>(null);
  const refusedDraft = useRef<string | null>(null);
  const saving = useRef(false);
  function refusalToastShown(): boolean {
    const text = refusalToast.current;
    return !!text && useStore.getState().toasts.some((t) => t.message === text);
  }
  const clearRefusalToastRef = useRef<() => void>(() => {});
  function clearRefusalToast() {
    const text = refusalToast.current;
    if (!text) return;
    refusalToast.current = null;
    refusedDraft.current = null;
    const { toasts, dismissToast } = useStore.getState();
    for (const t of toasts) if (t.message === text) dismissToast(t.id);
  }
  clearRefusalToastRef.current = clearRefusalToast;

  // The tile closing takes its refusal with it.
  useEffect(() => () => clearRefusalToastRef.current(), []);

  async function commitLine(lineNo: number, fromBlur = false) {
    if (!file) return;
    const text = draft;
    const draftText = text;
    // Clicking the refusal's dismiss button blurs the input: don't save again
    // (and re-show it) for text that was just refused. Enter still retries.
    if (fromBlur && refusedDraft.current === `${lineNo}:${text}` && refusalToastShown()) return;
    // Leaving a line you did not change has nothing to save (or to refuse).
    if (fromBlur && text === file.lines[lineNo]) { setEditingLine(null); return; }
    saving.current = true;
    try {
      const res = await api.writeFileLine(file.path, lineNo, text, file.mtime);
      setFile({
        ...file,
        lines: file.lines.map((l, i) => (i === lineNo ? text : l)),
        mtime: res.mtime,
      });
      setEditingLine(null);
      clearRefusalToast();
      toast('Saved', { kind: 'success', icon: '✓', ttlMs: 1500 });
    } catch (e) {
      const msg = (e as Error).message;
      if (/mtime mismatch/i.test(msg)) {
        clearRefusalToast();
        toast('File changed on disk — reloading', { kind: 'warn', icon: '⚠' });
        setEditingLine(null);
        await load(file.path);
      } else {
        clearRefusalToast();
        const refusal = isSaveApprovalRefusal(e);
        const text = 'Save failed: ' + saveFailureText(e);
        const text0 = draftText;
        // The approval advice has a next step to read: keep it until it is
        // dismissed or the person edits again.
        refusalToast.current = refusal ? text : null;
        refusedDraft.current = refusal ? `${lineNo}:${text0}` : null;
        toast(text, { kind: 'error', icon: '⚠', ...(refusal ? { ttlMs: 0 } : {}) });
      }
    } finally {
      saving.current = false;
    }
  }

  function startEdit(lineNo: number, current: string) {
    // A refused change is still only in the input: never replace it silently.
    if (editingLine !== null && editingLine !== lineNo && saving.current) return; // its save is still answering
    if (editingLine !== null && editingLine !== lineNo && refusedDraft.current?.startsWith(`${editingLine}:`)) {
      // The click blurred the input, which is where Enter and Escape are heard.
      linesRef.current?.querySelector<HTMLInputElement>('input.file-line-input')?.focus();
      toast(`Line ${editingLine + 1} has a change that isn't saved. Press Enter to try again, or Escape to discard it.`, { kind: 'warn', icon: '⚠', ttlMs: 6000 });
      return;
    }
    clearRefusalToast();
    setEditingLine(lineNo);
    setDraft(current);
  }

  function commitPath() {
    const next = pathDraft.trim();
    setEditingPath(false);
    if (next && next !== info.path) setFilePath(info.id, next);
  }

  // Split a ".env" line into key/value so we can mask values independently.
  // Lines without "=" are treated as plain (comments, blanks).
  function splitEnvLine(line: string): { key: string; sep: string; value: string } | null {
    if (!file?.isEnv) return null;
    const trimmed = line.trimStart();
    if (trimmed.startsWith('#') || trimmed === '') return null;
    const eq = line.indexOf('=');
    if (eq <= 0) return null;
    return { key: line.slice(0, eq), sep: '=', value: line.slice(eq + 1) };
  }

  return (
    <div
      className={`pane file-pane ${focused ? 'focused' : ''}`}
      onMouseDownCapture={() => setFocus(info.id)}
    >
      <div
        className="pane-header file-toolbar"
        onDoubleClick={(e) => {
          if (!canZoom) return;
          // PaneHeader gets this via per-control stopPropagation; this toolbar
          // also guards its controls and path's own double-click-to-edit action.
          if ((e.target as HTMLElement).closest('button, input, .file-path')) return;
          e.stopPropagation();
          toggleZoom(info.id);
        }}
      >
        <span className="activity-dot idle" title="file viewer" />
        {editingPath ? (
          <input
            className="file-path-input"
            autoFocus
            value={pathDraft}
            onChange={(e) => setPathDraft(e.target.value)}
            onBlur={commitPath}
            onKeyDown={(e) => {
              if (e.key === 'Enter') (e.target as HTMLInputElement).blur();
              if (e.key === 'Escape') { setPathDraft(info.path); setEditingPath(false); }
              e.stopPropagation();
            }}
            onClick={(e) => e.stopPropagation()}
          />
        ) : (
          <span
            className="file-path"
            title={info.path + '\n(double-click to change path)'}
            onDoubleClick={(e) => { e.stopPropagation(); setEditingPath(true); }}
          >{info.path.replace(/^\/home\/[^/]+/, '~')}</span>
        )}
        {file && <span className="file-meta">{file.lines.length}L · {formatSize(file.size)}{file.isEnv ? ' · env' : ''}</span>}
        <button
          className="iconbtn"
          title="Reload from disk"
          onClick={(e) => { e.stopPropagation(); clearRefusalToast(); void load(info.path); }}
        >⟳</button>
        <button
          className="iconbtn"
          title={`Open in shell (vim / $EDITOR) — for changes beyond one-liners`}
          onClick={(e) => {
            e.stopPropagation();
            launchShellHere(focusedPaneId || info.id, 'horizontal', 'policy').catch(() => {});
            // Hint the user — opening a shell in the right cwd is the
            // easiest approximation of "open in editor" here.
            toast('Shell opened — run `$EDITOR ' + info.path + '`', { kind: 'info', icon: '⎋', ttlMs: 4500 });
          }}
        >⎋ shell</button>
        {file && (
          <>
            <button
              className={`iconbtn ${searchOpen ? 'active' : ''}`}
              title="Find in file (Ctrl/Cmd+F) — type :42 to jump to a line"
              onClick={(e) => { e.stopPropagation(); searchOpen ? closeSearch() : openSearch(); }}
            ><IconSearch size={14} /></button>
            <button
              className={`iconbtn ${wrap ? 'active' : ''}`}
              title={wrap ? 'Line wrap on — click for no-wrap (horizontal scroll)' : 'Line wrap off — click to wrap long lines'}
              onClick={(e) => { e.stopPropagation(); setWrap((w) => !w); }}
            >⮐</button>
          </>
        )}
        <div className="pane-actions">
          {canZoom && (
            <button
              className="iconbtn pane-zoom-btn"
              title={isZoomed ? 'Restore layout (Alt+Z)' : 'Maximise pane (Alt+Z)'}
              aria-label={isZoomed ? 'Restore pane' : 'Maximise pane'}
              onClick={(e) => {
                e.stopPropagation();
                toggleZoom(info.id);
              }}
              onDoubleClick={(e) => e.stopPropagation()}
            >{isZoomed ? <IconMinimize size={13} /> : <IconMaximize size={13} />}</button>
          )}
          <button
            className="iconbtn"
            title="Close file tile"
            onClick={(e) => { e.stopPropagation(); closeFile(info.id); }}
          >×</button>
        </div>
      </div>
      {searchOpen && file && (
        <div className="file-search" onMouseDownCapture={(e) => e.stopPropagation()}>
          <span className="file-search-icon"><IconSearch size={13} style={{ verticalAlign: '-2px' }} /></span>
          <input
            ref={searchRef}
            className="file-search-input"
            placeholder="Find… or :42 to jump"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') { e.preventDefault(); go(e.shiftKey ? -1 : 1); }
              if (e.key === 'Escape') { e.preventDefault(); closeSearch(); }
              e.stopPropagation();
            }}
          />
          <span className="file-search-count">{searchCountLabel}</span>
          <button className="iconbtn" title="Previous (Shift+Enter)" disabled={!matches.length} onClick={() => go(-1)}>↑</button>
          <button className="iconbtn" title="Next (Enter)" disabled={!matches.length} onClick={() => go(1)}>↓</button>
          <button className="iconbtn" title="Close (Esc)" onClick={closeSearch}>×</button>
        </div>
      )}
      <div className="pane-body file-body">
        {err && (
          <div className="file-error">
            {err.refused ? (
              <>
                <div><strong><IconLock size={13} style={{ verticalAlign: '-2px' }} /> Secret file — not shown here.</strong></div>
                <div style={{ fontSize: 12, opacity: 0.85, marginTop: 6 }}>
                  Secret &amp; credential files (<code>.env</code>, keys, tokens)
                  aren't served over the remote connection for safety. Open them
                  on the local machine, or use a shell to view/edit.
                </div>
                <button
                  className="primary"
                  style={{ marginTop: 8 }}
                  onClick={(e) => {
                    e.stopPropagation();
                    launchShellHere(focusedPaneId || info.id, 'horizontal', 'policy').catch(() => {});
                    toast('Shell opened — run `$EDITOR ' + info.path + '`', { kind: 'info', icon: '⎋', ttlMs: 4500 });
                  }}
                >⎋ Open in shell</button>
              </>
            ) : (
              <>
                <div><strong>Couldn't open:</strong> {err.msg}</div>
                <button className="primary" onClick={() => load(info.path)} style={{ marginTop: 8 }}>Retry</button>
              </>
            )}
          </div>
        )}
        {!err && !file && <div className="file-loading">loading…</div>}
        {file && (
          <div ref={linesRef} className={`file-lines ${wrap ? 'wrap' : 'nowrap'}`}>
            {file.lines.map((line, i) => {
              const env = splitEnvLine(line);
              const isRevealed = revealed[i];
              const hit = searchOpen && matchSet.has(i);
              const cls = `file-line${hit ? ' search-match' : ''}${searchOpen && i === currentLn ? ' search-current' : ''}`;
              if (editingLine === i) {
                return (
                  <div key={i} data-ln={i} className={cls + ' editing'}>
                    <span className="file-lineno">{i + 1}</span>
                    <input
                      autoFocus
                      className="file-line-input"
                      value={draft}
                      onChange={(e) => { clearRefusalToast(); setDraft(e.target.value); }}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter') { e.preventDefault(); void commitLine(i); }
                        if (e.key === 'Escape') { e.preventDefault(); clearRefusalToast(); setEditingLine(null); }
                        e.stopPropagation();
                      }}
                      onBlur={() => commitLine(i, true)}
                    />
                  </div>
                );
              }
              return (
                <div key={i} data-ln={i} className={cls}>
                  <span className="file-lineno">{i + 1}</span>
                  {env ? (
                    <span className="file-line-content env-line">
                      <span className="env-key">{env.key}</span>
                      <span className="env-sep">=</span>
                      <span className={`env-value ${isRevealed ? '' : 'masked'}`}>
                        {isRevealed ? env.value : maskValue(env.value)}
                      </span>
                    </span>
                  ) : (
                    <span className="file-line-content"><code>{renderLineTokens(highlighted?.[i], line)}</code></span>
                  )}
                  <span className="file-line-actions">
                    {env && (
                      <button
                        className="iconbtn"
                        title={isRevealed ? 'Hide value' : 'Reveal value'}
                        onClick={() => setRevealed((r) => ({ ...r, [i]: !r[i] }))}
                      >{isRevealed ? <IconEyeOff size={14} /> : <IconEye size={14} />}</button>
                    )}
                    <button
                      className="iconbtn"
                      title={env ? 'Copy value' : 'Copy line'}
                      onClick={() => {
                        const text = env ? env.value : line;
                        void navigator.clipboard.writeText(text).catch(() => {});
                        toast('Copied', { kind: 'success', icon: '⧉', ttlMs: 1200 });
                      }}
                    >⧉</button>
                    <button
                      className="iconbtn file-edit-btn"
                      title="Edit this line"
                      onClick={() => startEdit(i, line)}
                    ><IconPencil size={12} /></button>
                  </span>
                </div>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}

// Render pre-tokenized line spans, or the raw line when not highlighted
// (plain/unknown/env files). Empty lines keep a non-breaking space so the row
// retains its height. Every token is a span for clean React keys; plain tokens
// get no class (transparent), highlighted ones get hl-<class>.
function renderLineTokens(toks: Tok[] | undefined, line: string): ReactNode {
  if (!line) return ' ';
  if (!toks) return line;
  return toks.map((t, i) => (
    <span key={i} className={t.cls ? `hl-${t.cls}` : undefined}>{t.text}</span>
  ));
}

function maskValue(v: string): string {
  if (!v) return '';
  // Keep first and last 2 chars visible if value is long, otherwise full mask.
  // Helps the user confirm "right file, right key" without revealing the secret.
  if (v.length <= 4) return '•'.repeat(v.length);
  return v.slice(0, 2) + '•'.repeat(Math.max(4, v.length - 4)) + v.slice(-2);
}

function formatSize(n: number): string {
  if (n < 1024) return `${n}B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)}KB`;
  return `${(n / 1024 / 1024).toFixed(1)}MB`;
}
