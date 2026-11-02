
import { useEffect, useRef, useState } from 'react';
import { IconAlertTriangle } from './ui/icons';
import { useStore } from '../store';
import { confirmDialog } from '../lib/dialog';
import { slotList } from '../extensions';
import {
  actionById,
  comboFromEvent,
  prettyCombo,
  prettyComboParts,
  resolveCombo,
} from '../keybindings';

interface Props {
  open: boolean;
  onClose: () => void;
}

// One row of the help overlay. Rows with an `actionId` are rebindable in
// place: their combo renders from the keybindings store (default or user
// override) and clicking it records a replacement — no separate "customize"
// dialog. Rows without an actionId are informational, with hardcoded keys.
interface ShortcutItem {
  keys?: string[];
  label: string;
  actionId?: string;
}

interface ShortcutGroup {
  title: string;
  items: ShortcutItem[];
}

const GROUPS: ShortcutGroup[] = [
  {
    title: 'Sessions',
    items: [
      { actionId: 'pane.new', label: 'New session dialog' },
      { keys: ['Alt', '→'], label: 'Split right with new session' },
      { keys: ['Alt', '↓'], label: 'Split down with new session' },
      { keys: ['Alt', 'Enter'], label: 'Copy the focused pane to the right (same folder and command)' },
      { actionId: 'browser.new', label: 'Open or close the built-in browser' },
      { keys: ['Alt', 'Shift', 'B'], label: 'Send one message to several panes at once' },
      { keys: ['Alt', 'W'], label: 'Close focused tile (sessions ask for confirm)' },
      { actionId: 'pane.reopenClosed', label: 'Reopen the last closed pane (resumes its Claude/Codex session)' },
      { actionId: 'pane.lastTurnDiff', label: "Show what the focused agent pane's last turn changed" },
    ],
  },
  {
    title: 'Focus',
    items: [
      { keys: ['Alt', 'H'], label: 'Focus pane to the left' },
      { keys: ['Alt', 'J'], label: 'Focus pane below' },
      { keys: ['Alt', 'K'], label: 'Focus pane above' },
      { keys: ['Alt', 'L'], label: 'Focus pane to the right' },
      { keys: ['Ctrl', 'Shift', '← ↑ ↓ →'], label: 'Move between panes with the arrow keys (same as Alt+H/J/K/L)' },
      { keys: ['Alt', '1', '…', '9'], label: 'Focus pane N (by layout order)' },
      { keys: ['Alt', '[', '/', ']'], label: 'Focus previous / next pane (cycle)' },
    ],
  },
  {
    title: 'Scrollback',
    items: [
      { keys: ['Shift', '↑', '/', '↓'], label: 'Scroll 3 lines up / down' },
      { keys: ['Shift', 'PageUp', '/', 'PageDown'], label: 'Scroll a page up / down' },
      { keys: ['Shift', 'Home', '/', 'End'], label: 'Jump to top / bottom of scrollback' },
    ],
  },
  {
    title: 'View',
    items: [
      { actionId: 'pane.zoom', label: 'Toggle zoom on focused pane' },
      { actionId: 'pane.realign', label: 'Refresh view — redraw panes if text looks jumbled' },
      { actionId: 'view.sidebar', label: 'Toggle sidebar' },
      { actionId: 'system.toggle', label: 'Show or hide the System page (memory, disk and app health)' },
      { keys: ['Ctrl', 'Shift', 'R'], label: 'Reload the app' },
      { actionId: 'help.toggle', label: 'Show these shortcuts (not while typing in a terminal — use the ? button at the top right instead)' },
      { keys: ['Esc'], label: 'Close dialogs / help' },
    ],
  },
  {
    title: 'Files & shell',
    items: [
      { actionId: 'palette.actions', label: 'Palette — type a handle without @ to jump to another running pane in the current layout. In-palette: "/" files · "@" first searches pane scrollbacks · ">" actions' },
      { actionId: 'palette.files', label: "Find a file or folder (starts in the focused pane's folder)" },
      { actionId: 'palette.findInPane', label: 'Find in focused pane — Enter next, Shift+Enter previous, Esc close' },
      { actionId: 'edit.commandLine', label: 'Edit the current command line in $EDITOR' },
      { keys: ['Tab'], label: 'Complete the highlighted suggestion in the sidebar Files box' },
    ],
  },
  {
    title: 'Sidebar',
    items: [
      { label: 'Expand all / Collapse all — one toggle button in the Workspaces header; it shows Expand all while anything is folded' },
      { keys: ['*', '/', '-'], label: 'Expand / collapse the focused row and its sibling branches (focus the sidebar tree first)' },
      { keys: ['Alt', 'click chevron'], label: 'Toggle the whole subtree' },
      { keys: ['Shift', 'F10', '/', 'Menu'], label: "Open the focused row's menu: the workspace menu on a workspace row, the pane menu on a pane row" },
    ],
  },
  {
    title: 'Workspaces',
    items: [
      { keys: ['Alt', 'Shift', '↑', '/', '↓'], label: 'Cycle through saved workspaces in the left panel' },
      { keys: ['Alt', 'Wheel ↑', '/', 'Wheel ↓'], label: 'Cycle workspaces with the mouse wheel (same as Alt+Shift+↑/↓)' },
    ],
  },
];

export function HotkeyHelp({ open, onClose }: Props) {
  const [query, setQuery] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);


  // Groups of optional features (extensions.ts); the list is fixed for the
  // page's lifetime, so the hook order is stable.
  const featureGroups = slotList('shortcutGroups');
  const featureVisible = featureGroups.map((g) => g.useVisible());
  const custom = useStore((s) => s.customKeybindings);
  const setKeybinding = useStore((s) => s.setKeybinding);
  const clearKeybinding = useStore((s) => s.clearKeybinding);
  const resetAllKeybindings = useStore((s) => s.resetAllKeybindings);
  const terminalKeysFirst = useStore((s) => s.terminalKeysFirst);
  const setTerminalKeysFirst = useStore((s) => s.setTerminalKeysFirst);

  // Action id currently being re-recorded, null = idle. While set, a
  // capture-phase window listener swallows every key so the recorded
  // combo never reaches xterm / the palette / the search box.
  const [recording, setRecording] = useState<string | null>(null);
  // Last recorded conflict (informational only — the write still goes
  // through; resolveCombo picks a winner by insertion order).
  const [lastConflict, setLastConflict] = useState<string | null>(null);

  // Clear the filter and focus the search box each time the overlay opens, so
  // you can type to narrow immediately.
  useEffect(() => {
    if (!open) return;
    setQuery('');
    setRecording(null);
    setLastConflict(null);
    const id = requestAnimationFrame(() => inputRef.current?.focus());
    return () => cancelAnimationFrame(id);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        onClose();
        return;
      }
      // "?" still toggles the overlay shut — but not while you're typing it into
      // the search box.
      if (e.key === '?') {
        const t = e.target as HTMLElement | null;
        const typing = !!t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA');
        if (!typing) {
          e.preventDefault();
          onClose();
        }
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, onClose]);

  // Capture-phase keydown while a row is recording. Runs before the
  // close handler above (and before anything else in the app), so the
  // recorded key — including Esc-to-cancel — never leaks out.
  useEffect(() => {
    if (!open || !recording) return;
    const onKey = (e: KeyboardEvent) => {
      e.preventDefault();
      e.stopPropagation();
      if (e.key === 'Escape') {
        setRecording(null);
        return;
      }
      const combo = comboFromEvent(e);
      if (!combo) return; // lone modifier — keep listening
      // Conflict check against the state BEFORE the write, so it
      // reflects "what you're about to break".
      const owner = resolveCombo(combo, custom);
      setKeybinding(recording, combo);
      setRecording(null);
      setLastConflict(owner && owner !== recording ? owner : null);
    };
    window.addEventListener('keydown', onKey, { capture: true });
    return () => window.removeEventListener('keydown', onKey, { capture: true });
  }, [open, recording, custom, setKeybinding]);

  if (!open) return null;

  // The combo a row currently answers to — override, else default.
  const effectiveCombo = (it: ShortcutItem): string | null => {
    if (!it.actionId) return null;
    const action = actionById(it.actionId);
    if (!action) return null;
    return custom[it.actionId] || action.defaultCombo;
  };

  const q = query.trim().toLowerCase();
  const visibleGroups: ShortcutGroup[] = [
    ...GROUPS,
    ...featureGroups.filter((_, i) => featureVisible[i]).map(({ title, items }) => ({ title, items })),
  ];
  const groups = q
    ? visibleGroups
        .map((g) => ({
          ...g,
          items: g.items.filter((it) => {
            const combo = effectiveCombo(it);
            const keyText = combo ? prettyComboParts(combo).join(' ') : (it.keys || []).join(' ');
            return `${it.label} ${keyText}`.toLowerCase().includes(q);
          }),
        }))
        .filter((g) => g.items.length > 0)
    : visibleGroups;

  const hasOverrides = Object.keys(custom).length > 0;
  const conflictLabel = lastConflict
    ? GROUPS.flatMap((g) => g.items).find((it) => it.actionId === lastConflict)?.label
      || actionById(lastConflict)?.label
      || lastConflict
    : null;

  return (
    <div className="dialog-backdrop" onMouseDown={onClose}>
      <div
        className="dialog hotkey-help"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="dialog-header">
          <h2 className="dialog-title">Keyboard shortcuts</h2>
          <button onClick={onClose} title="Close (Esc)" aria-label="Close">×</button>
        </div>
        <input
          ref={inputRef}
          className="hotkey-help-search"
          type="text"
          placeholder="Search shortcuts…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          aria-label="Search keyboard shortcuts"
        />
        {conflictLabel && (
          <div className="keybindings-conflict" role="alert">
            <IconAlertTriangle size={12} style={{ verticalAlign: '-2px' }} /> That combo was already bound to <strong>{conflictLabel}</strong>.
            Both now map to it — rebind one of them to resolve.
          </div>
        )}
        <label className="hotkey-terminal-first">
          <input
            type="checkbox"
            checked={terminalKeysFirst}
            onChange={(e) => setTerminalKeysFirst(e.target.checked)}
          />
          <span>
            <strong>Terminal keys first</strong> — while a terminal has focus, Ctrl+letter
            shortcuts (Ctrl+K, Ctrl+P, Ctrl+F, Ctrl+Q…) go to the shell / agent instead of the app.
            Alt shortcuts always work.
          </span>
        </label>
        {groups.length > 0 ? (
          <div className="hotkey-help-groups">
            {groups.map((g) => (
              <div key={g.title} className="hotkey-help-group">
                <h3>{g.title}</h3>
                <dl>
                  {g.items.map((it) => {
                    const combo = effectiveCombo(it);
                    const overridden = !!(it.actionId && custom[it.actionId]);
                    const isRecording = !!recording && recording === it.actionId;
                    return (
                      <div className="hotkey-help-row" key={it.label}>
                        <dt>
                          {isRecording ? (
                            <span className="keybindings-recording">press keys…</span>
                          ) : combo ? (
                            <>
                              <button
                                type="button"
                                className={`hotkey-rebind${overridden ? ' overridden' : ''}`}
                                title={`Click to rebind${overridden ? ' (custom)' : ''}`}
                                onClick={(e) => {
                                  // Blur so a Space/Enter in the recorded combo
                                  // can't re-activate this button on keyup.
                                  e.currentTarget.blur();
                                  setLastConflict(null);
                                  setRecording(it.actionId!);
                                }}
                              >
                                {prettyComboParts(combo).map((k, i) => (
                                  <span key={i}>
                                    {i > 0 && <span className="hotkey-plus">+</span>}
                                    <kbd>{k}</kbd>
                                  </span>
                                ))}
                              </button>
                              {overridden && (
                                <button
                                  type="button"
                                  className="hotkey-rebind-reset"
                                  title={`Custom binding — reset to ${prettyCombo(actionById(it.actionId!)!.defaultCombo)}`}
                                  aria-label="Reset shortcut to default"
                                  onClick={() => clearKeybinding(it.actionId!)}
                                >↺</button>
                              )}
                            </>
                          ) : (
                            (it.keys || []).map((k, i) => (
                              <span key={i}>
                                {i > 0 && k !== '/' && k !== '…' && <span className="hotkey-plus">+</span>}
                                {k === '/' || k === '…' ? <span className="hotkey-sep">{k}</span> : <kbd>{k}</kbd>}
                              </span>
                            ))
                          )}
                        </dt>
                        <dd>{it.label}</dd>
                      </div>
                    );
                  })}
                </dl>
              </div>
            ))}
          </div>
        ) : (
          <div className="hotkey-help-empty">No shortcuts match “{query.trim()}”.</div>
        )}
        <div className="hotkey-help-footer">
          <span className="hotkey-help-foot-note">
            {recording
              ? 'Press the new combo — Esc cancels.'
              : 'Underlined shortcuts are rebindable — click one and press the new keys.'}
          </span>
          {hasOverrides && (
            <button
              type="button"
              className="hotkey-help-customize"
              onClick={async () => {
                if (await confirmDialog({
                  message: 'Reset every shortcut back to its default?',
                  confirmLabel: 'Reset',
                  danger: true,
                })) {
                  resetAllKeybindings();
                }
              }}
            >
              Reset all to defaults
            </button>
          )}
          <button
            type="button"
            className="hotkey-help-customize"
            onClick={() => {
              onClose();
              useStore.getState().setSupportBundleOpen(true);
            }}
          >
            Create support bundle…
          </button>
        </div>
      </div>
    </div>
  );
}
