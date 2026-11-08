import { useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../../api';
import { useStore } from '../../store';
import { SectionHeader } from './SectionHeader';
import { resolveDraft, resolveForOpen as resolvePath, stripTrailingSep } from './filesPath';
import { IconFolder, IconPin, IconX, IconSparkle, IconPlus, IconNotebook, IconPencil } from '../ui/icons';

interface FilesSectionProps {
  collapsed: boolean;
  onToggle: () => void;
}

interface DirEntry { name: string; isDir: boolean; size: number }
function formatBytes(n: number): string {
  if (n < 1024) return `${n}B`;
  if (n < 1024 * 1024) return `${Math.round(n / 1024)}KB`;
  return `${(n / 1024 / 1024).toFixed(1)}MB`;
}

interface TouchedFile {
  path: string; name: string; dir: string; tool: string;
  paneId?: string; paneTitle?: string; ts: number;
}

function toolGlyph(tool: string): React.ReactElement {
  switch (tool) {
    case 'Write': return <IconPlus size={11} />;        // created / overwrote
    case 'NotebookEdit': return <IconNotebook size={11} />;
    default: return <IconPencil size={11} />;           // Edit, MultiEdit
  }
}

function relTime(ts: number): string {
  const s = Math.max(0, Math.round((Date.now() - ts) / 1000));
  if (s < 10) return 'now';
  if (s < 60) return `${s}s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h}h`;
  return `${Math.round(h / 24)}d`;
}

export function FilesSection({ collapsed, onToggle }: FilesSectionProps) {
  const openFileHere = useStore((s) => s.openFileHere);
  const openDiffHere = useStore((s) => s.openDiffHere);
  const recent = useStore((s) => s.recentFilePaths);
  const recentDirs = useStore((s) => s.recentDirs);
  const pushRecentDir = useStore((s) => s.pushRecentDir);
  const filesAnchor = useStore((s) => s.filesAnchor);
  const setFilesAnchor = useStore((s) => s.setFilesAnchor);
  const focusedPaneId = useStore((s) => s.focusedPaneId);
  const panes = useStore((s) => s.panes);
  const [draft, setDraft] = useState('');
  const [entries, setEntries] = useState<DirEntry[] | null>(null);
  const [selIdx, setSelIdx] = useState(0);
  const [showDropdown, setShowDropdown] = useState(false);
  const [touched, setTouched] = useState<TouchedFile[]>([]);
  const inputRef = useRef<HTMLInputElement | null>(null);

  // Poll "files the agent just touched" while the section is open. Cheap
  // single GET; the daemon serves an in-memory ring (recency, not history).
  useEffect(() => {
    if (collapsed) return;
    let cancelled = false;
    const load = () => api.recentFiles()
      .then((r) => { if (!cancelled) setTouched(r.files); })
      .catch(() => { /* keep the previous list on a blip */ });
    load();
    const id = setInterval(load, 5000);
    return () => { cancelled = true; clearInterval(id); };
  }, [collapsed]);

  const pollCwd = focusedPaneId
    ? (panes[focusedPaneId]?.liveCwd || panes[focusedPaneId]?.cwd)
    : undefined;
  const [liveCwd, setLiveCwd] = useState<string | undefined>(pollCwd);
  const [cwdBump, setCwdBump] = useState(0);
  useEffect(() => {
    if (collapsed || !focusedPaneId || !pollCwd) {
      setLiveCwd(pollCwd);
      return;
    }
    setLiveCwd(pollCwd);
    let cancelled = false;
    api
      .paneLiveCwd(focusedPaneId)
      .then((cwd) => {
        if (!cancelled && cwd) setLiveCwd(cwd);
      })
      .catch(() => { /* keep whatever liveCwd we had */ });
    return () => { cancelled = true; };
  }, [focusedPaneId, pollCwd, cwdBump, collapsed]);

  const focusedCwd = filesAnchor || liveCwd;
  const resolved = useMemo(
    () => resolveDraft(draft, focusedCwd),
    [draft, focusedCwd],
  );

  useEffect(() => {
    if (collapsed || !showDropdown) return;
    let cancelled = false;
    api
      .listDir(resolved.dirAbs)
      .then((r) => {
        if (!cancelled) setEntries(r.entries);
      })
      .catch(() => {
        if (!cancelled) setEntries([]);
      });
    return () => { cancelled = true; };
  }, [resolved.dirAbs, collapsed, showDropdown]);

  const filtered = useMemo(() => {
    if (!entries) return [];
    const n = resolved.needle.toLowerCase();
    const out = entries.filter((e) => e.name.toLowerCase().includes(n));
    return out.slice(0, 30);
  }, [entries, resolved.needle]);

  useEffect(() => {
    if (selIdx >= filtered.length) setSelIdx(0);
  }, [filtered.length, selIdx]);

  function resolveForOpen(path: string): string {
    return resolvePath(path, focusedCwd);
  }

  function open(path: string) {
    const resolved = resolveForOpen(path);
    if (!resolved) return;
    openFileHere(resolved);
    setShowDropdown(false);
  }

  function completeWith(entry: DirEntry) {
    const next = resolved.dirInput + entry.name + (entry.isDir ? '/' : '');
    setDraft(next);
    setSelIdx(0);
    if (entry.isDir) {
      const base = resolveForOpen(stripTrailingSep(next));
      if (base) pushRecentDir(base);
    }
    inputRef.current?.focus();
  }

  function onKeyDown(e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === 'ArrowDown') {
      if (!showDropdown) { setShowDropdown(true); return; }
      e.preventDefault();
      setSelIdx((i) => (filtered.length ? (i + 1) % filtered.length : 0));
      return;
    }
    if (e.key === 'ArrowUp') {
      if (!showDropdown) return;
      e.preventDefault();
      setSelIdx((i) => (filtered.length ? (i - 1 + filtered.length) % filtered.length : 0));
      return;
    }
    if (e.key === 'Tab') {
      if (filtered[selIdx]) {
        e.preventDefault();
        completeWith(filtered[selIdx]);
      }
      return;
    }
    if (e.key === 'Escape') {
      if (showDropdown) { setShowDropdown(false); e.preventDefault(); }
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      const sel = showDropdown ? filtered[selIdx] : undefined;
      if (sel) {
        if (sel.isDir) {
          completeWith(sel);
        } else {
          open(resolved.dirInput + sel.name);
          setDraft('');
        }
      } else {
        open(draft);
        setDraft('');
      }
      return;
    }
    e.stopPropagation();
  }

  function shortcutPath(sub: string) {
    return focusedCwd ? `${focusedCwd}/${sub}` : sub;
  }

  return (
    <div className="sidebar-section">
      <SectionHeader icon={<IconFolder size={14} />} title="Files" collapsed={collapsed} onToggle={onToggle} />
      {!collapsed && (
        <div>
          <div className="files-input-wrap">
            <input
              ref={inputRef}
              value={draft}
              onChange={(e) => { setDraft(e.target.value); setShowDropdown(true); setSelIdx(0); }}
              onFocus={() => {
                setShowDropdown(true);
                setCwdBump((n) => n + 1);
              }}
              onBlur={() => {
                setTimeout(() => setShowDropdown(false), 120);
              }}
              onKeyDown={onKeyDown}
              placeholder="Path — type to browse · Tab to complete"
              style={{ width: '100%', fontSize: 11, padding: '3px 5px' }}
              autoComplete="off"
              spellCheck={false}
            />
            {focusedCwd && (
              <div
                className="files-cwd-hint"
                title={filesAnchor
                  ? `Pinned anchor — relative paths resolve here. Click × to unpin and follow the focused pane instead.`
                  : `Relative paths resolve here — refreshed each time you focus the box. Click to refresh now.`}
                onClick={() => { if (!filesAnchor) setCwdBump((n) => n + 1); }}
                style={filesAnchor ? { display: 'flex', alignItems: 'center', gap: 4 } : undefined}
              >
                {filesAnchor ? (
                  <>
                    <IconPin size={11} />
                    <code style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis' }}>
                      {focusedCwd.replace(/^\/home\/[^/]+/, '~')}
                    </code>
                    <button
                      className="iconbtn"
                      title="Unpin — follow focused pane again"
                      aria-label="Unpin Files anchor"
                      onClick={(e) => { e.stopPropagation(); setFilesAnchor(null); }}
                      style={{ padding: '0 5px', background: 'transparent', border: 'none', display: 'inline-flex' }}
                    ><IconX size={12} /></button>
                  </>
                ) : (
                  <>
                    in <code>{focusedCwd.replace(/^\/home\/[^/]+/, '~')}</code>
                    <span className="files-cwd-refresh" aria-hidden>⟳</span>
                  </>
                )}
              </div>
            )}
            {showDropdown && filtered.length > 0 && (
              <div className="files-dropdown" role="listbox">
                {filtered.map((entry, i) => (
                  <div
                    key={entry.name}
                    role="option"
                    aria-selected={i === selIdx}
                    className={`files-dropdown-row ${i === selIdx ? 'sel' : ''}`}
                    onMouseDown={(e) => {
                      e.preventDefault();
                      if (entry.isDir) completeWith(entry);
                      else { open(resolved.dirInput + entry.name); setDraft(''); }
                    }}
                    onMouseEnter={() => setSelIdx(i)}
                    title={entry.name}
                  >
                    <span className="files-dropdown-name">
                      {entry.isDir ? '▸ ' : ''}{entry.name}{entry.isDir ? '/' : ''}
                    </span>
                    {!entry.isDir && (
                      <span className="files-dropdown-size">{formatBytes(entry.size)}</span>
                    )}
                  </div>
                ))}
              </div>
            )}
          </div>
          {focusedCwd && (
            <div style={{ display: 'flex', gap: 4, marginTop: 4, flexWrap: 'wrap' }}>
              <button
                onClick={() => openDiffHere(focusedCwd)}
                title={`Review git changes in ${focusedCwd} — open a diff tile`}
                style={{ fontSize: 10, padding: '1px 6px' }}
              >⎇ Review changes</button>
              {/* No .env shortcuts: a one-click secrets file is a bad default
                  (screen shares, demos), and they rarely exist. */}
              {['package.json'].map((n) => (
                <button
                  key={n}
                  onClick={() => open(shortcutPath(n))}
                  title={`Open ${shortcutPath(n)} in a file tile`}
                  style={{ fontSize: 10, padding: '1px 6px' }}
                >{n}</button>
              ))}
            </div>
          )}
          {touched.length > 0 && (
            <div style={{ marginTop: 6 }}>
              <div
                style={{ fontSize: 10, color: 'var(--fg-dim)', marginBottom: 2, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}
                title="Files your agents just created or edited (newest first). Click to open."
              >
                <span><IconSparkle size={11} style={{ verticalAlign: '-1px' }} /> agent touched</span>
                <span style={{ fontSize: 9, opacity: 0.7 }}>tap to open</span>
              </div>
              {touched.slice(0, 8).map((tf) => (
                <div
                  key={tf.path}
                  className="row"
                  title={`${tf.path.replace(/^\/home\/[^/]+/, '~')}\n${tf.tool}${tf.paneTitle ? ' · ' + tf.paneTitle : ''} · ${relTime(tf.ts)} ago`}
                  onClick={() => open(tf.path)}
                  style={{ fontSize: 12, fontFamily: "'SF Mono','Menlo',monospace", display: 'flex', alignItems: 'center', gap: 6 }}
                >
                  <span aria-hidden style={{ fontSize: 10, opacity: 0.7, flexShrink: 0, width: 12, textAlign: 'center' }}>{toolGlyph(tf.tool)}</span>
                  <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', minWidth: 0, flex: 1 }}>{tf.name}</span>
                  <span style={{ fontSize: 9, color: 'var(--fg-dim)', flexShrink: 0 }}>{relTime(tf.ts)}</span>
                </div>
              ))}
            </div>
          )}
          {recentDirs.length > 0 && (
            <div style={{ marginTop: 6 }}>
              <div
                style={{ fontSize: 10, color: 'var(--fg-dim)', marginBottom: 2, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}
                title="Directories you've drilled into recently. Click to pin as the Files anchor."
              >
                <span>recent folders</span>
                <span style={{ fontSize: 9, opacity: 0.7 }}>click to pin</span>
              </div>
              {recentDirs.slice(0, 3).map((p) => {
                const abbr = p.replace(/^\/home\/[^/]+/, '~');
                const isPinned = filesAnchor === p;
                return (
                  <div
                    key={p}
                    className={`row ${isPinned ? 'active' : ''}`}
                    title={isPinned ? `${p} (currently pinned)` : `${p} — click to pin as Files anchor`}
                    onClick={() => setFilesAnchor(isPinned ? null : p)}
                    style={{ fontSize: 12, fontFamily: "'SF Mono','Menlo',monospace", display: 'flex', alignItems: 'center', gap: 4 }}
                  >
                    <IconPin size={11} style={{ opacity: isPinned ? 1 : 0.4, flexShrink: 0 }} />
                    <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', minWidth: 0, flex: 1 }}>{abbr}</span>
                  </div>
                );
              })}
            </div>
          )}
          {recent.length > 0 && (
            <div style={{ marginTop: 6 }}>
              <div style={{ fontSize: 10, color: 'var(--fg-dim)', marginBottom: 2 }}>recent files</div>
              {recent.slice(0, 3).map((p) => {
                const abbr = p.replace(/^\/home\/[^/]+/, '~');
                return (
                  <div
                    key={p}
                    className="row"
                    title={p}
                    onClick={() => open(p)}
                    style={{ fontSize: 12, fontFamily: "'SF Mono','Menlo',monospace" }}
                  >
                    <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', minWidth: 0 }}>{abbr}</span>
                  </div>
                );
              })}
            </div>
          )}
          {recent.length === 0 && recentDirs.length === 0 && !focusedCwd && (
            <div style={{ fontSize: 10, color: 'var(--fg-dim)', marginTop: 4 }}>
              Type a path and press Enter to open a file tile. Ctrl+P for fuzzy finder.
            </div>
          )}
        </div>
      )}
    </div>
  );
}
