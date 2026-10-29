// Single-key answers to a terminal menu (pane.state === 'choice-prompt').
//
// Claude Code and Codex numbered menus act on the bare keypress: "1" picks
// option 1 at once, with no Enter. An Enter sent after it lands on whatever
// screen comes next — the next queued permission prompt (whose focused row
// is usually "Yes"), or the next question of a multi-question menu — and
// answers that too. So a menu key goes out alone.
//
// Codex's submit-style menus (footer "enter to submit") are the exception:
// there a digit only moves the focus marker and Enter confirms it. The
// daemon's answer_prompt handles them the same way (see
// daemon/mcp_answer_prompt.go): Enter is sent only if, after the digit
// settles, the live screen (never scrollback, where a closed menu's footer
// lingers) still shows the same menu, still says "enter to submit", and its
// focus marker now sits on the chosen option. Otherwise nothing more is sent.

// The line that starts the option list: optional focus arrow, then "1." /
// "1)" / "1" followed by 2+ spaces (Claude's repaint glitches drop the dot).
const MENU_START_RE = /^[\s\p{Zs}]*(?:[❯❭›▶][\s\p{Zs}]*)?1(?:[.)]|[\s\p{Zs}]{2,})/u;
const FOCUS_ROW_RE = /^[\s\p{Zs}]*[❯❭›▶][\s\p{Zs}]*([0-9]+)(?:[.)]|[\s\p{Zs}]{2,})/u;
const SUBMIT_FOOTER_RE = /enter to submit/i;

// Matches answer_prompt's settle before it re-reads the screen.
export const MENU_SUBMIT_SETTLE_MS = 300;

// menuTail returns the bottom-most menu on a screen: from the last line that
// starts the option list to the end. Empty when no menu is visible.
export function menuTail(screen: string): string {
  const lines = screen.split('\n');
  for (let i = lines.length - 1; i >= 0; i--) {
    if (MENU_START_RE.test(lines[i])) return lines.slice(i).join('\n');
  }
  return '';
}

// menuFingerprint identifies the bottom-most menu apart from its focus: up
// to six lines of question above the options (as the daemon's menuRegion
// does) plus the numbered option rows, focus markers removed and whitespace
// collapsed. Footers are left out — Codex's shows "option 2/4", which moves
// with the focus. '' when no menu is on screen.
const OPTION_ROW_RE = /^[\s\p{Zs}]*(?:[❯❭›▶][\s\p{Zs}]*)?[0-9]+(?:[.)]|[\s\p{Zs}]{2,})/u;
export function menuFingerprint(screen: string): string {
  const lines = screen.split('\n');
  let start = -1;
  for (let i = lines.length - 1; i >= 0; i--) {
    if (MENU_START_RE.test(lines[i])) { start = i; break; }
  }
  if (start < 0) return '';
  const norm = (l: string) => l.replace(/[❯❭›▶]/gu, ' ').replace(/[\s\p{Zs}]+/gu, ' ').trim();
  const question = lines.slice(Math.max(0, start - 6), start);
  const options = lines.slice(start).filter((l) => OPTION_ROW_RE.test(l));
  return [...question, ...options].map(norm).filter(Boolean).join('\n');
}

export function menuNeedsSubmit(screen: string): boolean {
  return SUBMIT_FOOTER_RE.test(menuTail(screen));
}

export function focusedMenuOption(screen: string): string {
  let focused = '';
  for (const line of menuTail(screen).split('\n')) {
    const m = FOCUS_ROW_RE.exec(line);
    if (m) focused = m[1];
  }
  return focused;
}

// isMenuKeyReply reports whether a typed reply is a single menu key: one
// digit, or y / n (Codex approval shortcuts). At a choice-prompt these must
// not get the usual trailing Enter.
export function isMenuKeyReply(text: string): boolean {
  return /^\s*(?:[1-9]|[yn])\s*$/i.test(text);
}

// sendMenuKey writes one menu key and, only for a submit-style menu whose
// focus moved to the chosen digit, a confirming Enter. `screen` must return
// the pane's live screen rows only (no scrollback). `send` must carry the
// caller's own target/process guard; it is re-checked for the delayed Enter.
// Returns whether the key itself was written.
export function sendMenuKey(
  send: (bytes: string) => boolean,
  key: string,
  screen: () => string | undefined,
): boolean {
  const before = /^[1-9]$/.test(key) ? screen() ?? '' : '';
  if (!send(key)) return false;
  if (!before || !menuNeedsSubmit(before)) return true;
  const menu = menuFingerprint(before);
  setTimeout(() => {
    const after = screen() ?? '';
    if (menuFingerprint(after) === menu && menuNeedsSubmit(after) && focusedMenuOption(after) === key) send('\r');
  }, MENU_SUBMIT_SETTLE_MS);
  return true;
}

// paneScreen reads a pane's live screen rows through its registered
// snapshot getter, for sendMenuKey.
export function paneScreen(snapshot: ((opts?: { screen?: boolean }) => string) | undefined): string | undefined {
  try { return snapshot?.({ screen: true }); } catch { return undefined; }
}
