// Rebindable keyboard shortcuts — the subset users most often want to
// change, kept intentionally small for v1. Rebinding the whole Alt+vim
// nav set is a rabbit hole (xterm interaction, spatial nav vs. pane
// cycle, mobile key handling) that we defer until someone actually asks.
//
// Design:
//   * Each rebindable action has a stable id ("palette.actions") + a
//     human-readable label + a default combo.
//   * Users can override the combo. App.tsx checks user overrides
//     first, then falls back to defaults. Any action the user has
//     rebound skips its default — so Ctrl+K on a user who moved
//     palette.actions to Ctrl+Space no longer triggers the palette.
//   * Combos are stored as lowercase strings like "ctrl+k",
//     "alt+shift+b", or "?". Order is fixed: ctrl, alt, shift, meta,
//     key. Serialization is tested via parseCombo/comboFromEvent
//     round-trip so the dialog and the matcher agree.

export interface RebindableAction {
  id: string;
  label: string;
  hint: string;
  // Combo string — see `comboFromEvent` for the serialization.
  defaultCombo: string;
}

export const REBINDABLE_ACTIONS: RebindableAction[] = [
  {
    id: 'palette.actions',
    label: 'Open command palette (actions)',
    hint: 'Ctrl+K — the one-stop action palette',
    defaultCombo: 'ctrl+k',
  },
  {
    id: 'palette.files',
    label: 'Open command palette (files)',
    hint: 'Ctrl+P — palette, seeded to file finder',
    defaultCombo: 'ctrl+p',
  },
  {
    id: 'palette.findInPane',
    label: 'Find in focused pane',
    hint: 'Ctrl+F — in-pane find overlay',
    defaultCombo: 'ctrl+f',
  },
  {
    id: 'view.sidebar',
    label: 'Toggle sidebar',
    hint: 'Ctrl+Q — show / hide the left sidebar',
    defaultCombo: 'ctrl+q',
  },
  {
    id: 'pane.new',
    label: 'New pane dialog',
    hint: 'Alt+N — open New Pane dialog (no split)',
    defaultCombo: 'alt+n',
  },
  {
    id: 'browser.new',
    label: 'Toggle browser',
    hint: 'Alt+B — open the built-in browser; Alt+B again closes it. Inside a terminal, Alt+B moves back a word instead.',
    defaultCombo: 'alt+b',
  },
  {
    id: 'pane.realign',
    label: 'Refresh view',
    hint: 'Alt+R — redraw panes at this window\'s size if text looks jumbled, for example after an update or after using them on another device',
    defaultCombo: 'alt+r',
  },
  {
    id: 'pane.zoom',
    label: 'Maximise / restore focused pane',
    hint: 'Alt+Z — zoom the focused pane to fill the window; press again to restore. Rebind this if a GPU overlay (NVIDIA/AMD both default to Alt+Z) swallows the chord system-wide.',
    defaultCombo: 'alt+z',
  },
  // NOTE: "Force redraw focused pane" is intentionally NOT a rebindable
  // keyboard action. comboFromEvent drops Shift for letter keys, so
  // Ctrl+Shift+R can never serialize to "ctrl+shift+r" — the binding was
  // permanently dead. Ctrl+Shift+R is now an explicit "reload app" handler in
  // App.tsx. Force redraw remains available via the pane context menu (it fixes
  // Codex ghost/duplicate cells after a stress resize or workspace switch).
  {
    id: 'edit.commandLine',
    label: 'Edit current line in $EDITOR',
    hint: 'Alt+G — sends Ctrl+X Ctrl+E to the shell, opening the current command line in $EDITOR (readline / zsh edit-command-line)',
    defaultCombo: 'alt+g',
  },
  {
    id: 'system.toggle',
    label: 'Toggle System page',
    hint: 'Alt+O — open the System page (memory, disk and app health); Alt+O again closes it.',
    defaultCombo: 'alt+o',
  },
  {
    id: 'pane.reopenClosed',
    label: 'Reopen closed pane',
    hint: 'Alt+U — bring back the most recently closed pane: same folder and command, Claude/Codex conversation resumed. Inside a terminal, Alt+U uppercases a word instead.',
    defaultCombo: 'alt+u',
  },
  {
    id: 'pane.lastTurnDiff',
    label: "Show the focused pane's last turn changes",
    hint: "Alt+D — open the diff of what changed in the repo during the focused agent pane's last turn. Inside a terminal, Alt+D deletes a word instead.",
    defaultCombo: 'alt+d',
  },
  {
    id: 'help.toggle',
    label: 'Show keyboard shortcuts',
    hint: '? — overlay listing every shortcut',
    defaultCombo: '?',
  },
];

// Lookup helpers.
export function actionById(id: string): RebindableAction | undefined {
  return REBINDABLE_ACTIONS.find((a) => a.id === id);
}

// Build a combo string from a KeyboardEvent. Key names follow the
// KeyboardEvent.key contract — `Escape`, `Enter`, `ArrowUp`, letters
// are single chars, punctuation is the literal char. Lowercased so
// "K" and "k" compare equal.
//
// Modifier handling: we only serialize modifiers that are actually
// held. `shift` is intentionally dropped for single-char keys where
// the character itself already encodes the shift (e.g. `?` instead of
// `shift+/`). For non-char keys (Escape, arrows), shift is preserved.
export function comboFromEvent(e: KeyboardEvent): string | null {
  // Treat lone modifier keys as "no combo yet". Prevents the dialog
  // from recording "ctrl" as a valid binding while the user holds
  // Ctrl waiting to press the real key.
  if (e.key === 'Control' || e.key === 'Alt' || e.key === 'Shift' || e.key === 'Meta') {
    return null;
  }
  const parts: string[] = [];
  if (e.ctrlKey) parts.push('ctrl');
  if (e.altKey) parts.push('alt');
  // Shift is meaningful only for non-character keys. Typing `?`
  // technically involves Shift; serialize just the character.
  const isChar = e.key.length === 1;
  if (e.shiftKey && !isChar) parts.push('shift');
  if (e.metaKey) parts.push('meta');
  parts.push(isChar ? e.key.toLowerCase() : e.key);
  return parts.join('+');
}

// Resolve a combo to an action id. Check user overrides first; fall
// back to defaults, skipping defaults for actions the user has
// rebound (otherwise the original combo would still fire the action).
// Saved overrides for actions that no longer exist (e.g. the removed
// 'todo.quickAdd') are ignored, so their combo stays free.
export function resolveCombo(
  combo: string,
  overrides: Record<string, string>,
): string | null {
  for (const [id, c] of Object.entries(overrides)) {
    if (c && c === combo && actionById(id)) return id;
  }
  for (const action of REBINDABLE_ACTIONS) {
    if (action.id in overrides) continue; // user rebound this one
    if (action.defaultCombo === combo) return action.id;
  }
  return null;
}

// Human-readable rendering, one entry per key. Capitalizes modifiers,
// leaves key names alone ("?" stays "?", "ArrowUp" becomes "↑").
// Exposed separately from prettyCombo so the help overlay can render
// each part as its own <kbd>.
export function prettyComboParts(combo: string): string[] {
  if (!combo) return [];
  return combo
    .split('+')
    .map((part) => {
      switch (part) {
        case 'ctrl': return 'Ctrl';
        case 'alt': return 'Alt';
        case 'shift': return 'Shift';
        case 'meta': return 'Cmd';
        case 'ArrowUp': return '↑';
        case 'ArrowDown': return '↓';
        case 'ArrowLeft': return '←';
        case 'ArrowRight': return '→';
        case 'Escape': return 'Esc';
        case 'Enter': return '↵';
        case ' ': return 'Space';
        default: return part.length === 1 ? part.toUpperCase() : part;
      }
    });
}

export function prettyCombo(combo: string): string {
  return prettyComboParts(combo).join('+');
}

// Ctrl+<letter> chords that collide with shell / TUI keys (readline's Ctrl+K
// kill-line, Ctrl+P history, Ctrl+F forward-char, Ctrl+Q flow control). With
// "Terminal keys first" on, these reach the terminal when it has focus.
// Alt+letter chords that readline (bash, zsh) and Claude Code's prompt use
// for word editing: Alt+B back a word, Alt+D delete a word, Alt+T swap
// words, Alt+U uppercase a word. While a terminal has focus these default
// shortcuts go to the terminal; a user who binds one explicitly still gets it.
export const READLINE_ALT_COMBOS: ReadonlySet<string> = new Set(['alt+b', 'alt+d', 'alt+t', 'alt+u']);

export function isTerminalCollidingCombo(combo: string): boolean {
  return /^ctrl\+[a-z]$/.test(combo);
}
