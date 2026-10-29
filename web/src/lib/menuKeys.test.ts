import { afterEach, describe, expect, it, vi } from 'vitest';
import { focusedMenuLabel, focusedMenuOption, isMenuKeyReply, menuFingerprint, menuNeedsSubmit, menuTail, MENU_SUBMIT_SETTLE_MS, paneScreen, sendMenuKey } from './menuKeys';

const CLAUDE_MENU = [
  ' Bash command',
  '   rm -rf build',
  ' Do you want to proceed?',
  ' ❯ 1. Yes',
  "   2. Yes, and don't ask again for rm commands in /repo",
  '   3. No, and tell Claude what to do differently (esc)',
].join('\n');

const CODEX_SUBMIT_MENU = (focus: number) => [
  'Allow the example MCP server to run tool "write_file"?',
  `${focus === 1 ? '›' : ' '} 1. Allow`,
  `${focus === 2 ? '›' : ' '} 2. Cancel`,
  'enter to submit | esc to cancel',
].join('\n');

describe('menu screen parsing', () => {
  it('keeps only the bottom-most menu', () => {
    const screen = `${CODEX_SUBMIT_MENU(1)}\nlater output\n${CLAUDE_MENU}`;
    expect(menuTail(screen).startsWith(' ❯ 1. Yes')).toBe(true);
    expect(menuNeedsSubmit(screen)).toBe(false);
    expect(menuTail('no menu here')).toBe('');
  });

  it('recognises Codex submit-style menus and their focus row', () => {
    expect(menuNeedsSubmit(CODEX_SUBMIT_MENU(1))).toBe(true);
    expect(focusedMenuOption(CODEX_SUBMIT_MENU(2))).toBe('2');
    expect(menuNeedsSubmit(CLAUDE_MENU)).toBe(false);
    expect(focusedMenuOption(CLAUDE_MENU)).toBe('1');
  });

  it('reads the highlighted option text from the bottom-most menu', () => {
    expect(focusedMenuLabel(CLAUDE_MENU)).toBe('Yes');
    expect(focusedMenuLabel(CODEX_SUBMIT_MENU(2))).toBe('Cancel');
    // An older menu higher on the screen does not count.
    expect(focusedMenuLabel(`${CODEX_SUBMIT_MENU(2)}\nlater output\n${CLAUDE_MENU}`)).toBe('Yes');
    // Repaint glitch without the dot, trailing padding trimmed.
    expect(focusedMenuLabel(' Proceed?\n ❯ 1  Yes, allow     \n   2  No')).toBe('Yes, allow');
  });

  it('reads no option text when it cannot tell which option is highlighted', () => {
    expect(focusedMenuLabel('')).toBe('');
    expect(focusedMenuLabel('no menu here')).toBe('');
    // A menu with no focus marker, two markers, or a marker on an empty row.
    expect(focusedMenuLabel(' Proceed?\n   1. Yes\n   2. No')).toBe('');
    expect(focusedMenuLabel(' Proceed?\n ❯ 1. Yes\n ❯ 2. No')).toBe('');
    expect(focusedMenuLabel(' Proceed?\n ❯ 1.\n   2. No')).toBe('');
    // The marker must sit in the bottom-most menu, not in output above it.
    expect(focusedMenuLabel(' ❯ 2. Old choice\nlater output\n Proceed?\n   1. Yes\n   2. No')).toBe('');
  });

  it('treats a single digit or y/n as a menu key, anything longer as text', () => {
    for (const t of ['1', ' 2 ', '9', 'y', 'N']) expect(isMenuKeyReply(t)).toBe(true);
    for (const t of ['0', '12', 'yes', '1.', 'no thanks', '']) expect(isMenuKeyReply(t)).toBe(false);
  });
});

describe('sendMenuKey', () => {
  afterEach(() => vi.useRealTimers());

  it('sends a digit alone on a Claude menu — no trailing Enter', () => {
    vi.useFakeTimers();
    const send = vi.fn(() => true);
    expect(sendMenuKey(send, '1', () => CLAUDE_MENU)).toBe(true);
    vi.advanceTimersByTime(5000);
    expect(send.mock.calls).toEqual([['1']]);
  });

  it('confirms a submit-style menu only once its focus moved to the digit', () => {
    vi.useFakeTimers();
    let screen = CODEX_SUBMIT_MENU(1);
    const send = vi.fn((bytes: string) => { if (bytes === '2') screen = CODEX_SUBMIT_MENU(2); return true; });
    sendMenuKey(send, '2', () => screen);
    expect(send.mock.calls).toEqual([['2']]);
    vi.advanceTimersByTime(MENU_SUBMIT_SETTLE_MS);
    expect(send.mock.calls).toEqual([['2'], ['\r']]);
  });

  it('sends no Enter when the focus did not move or the menu closed', () => {
    vi.useFakeTimers();
    const stuck = vi.fn(() => true);
    sendMenuKey(stuck, '2', () => CODEX_SUBMIT_MENU(1));
    vi.advanceTimersByTime(MENU_SUBMIT_SETTLE_MS);
    expect(stuck.mock.calls).toEqual([['2']]);

    let screen = CODEX_SUBMIT_MENU(1);
    const closed = vi.fn(() => { screen = 'working…'; return true; });
    sendMenuKey(closed, '1', () => screen);
    vi.advanceTimersByTime(MENU_SUBMIT_SETTLE_MS);
    expect(closed.mock.calls).toEqual([['1']]);
  });

  it('never sends Enter after Esc, y or n, and reports a refused write', () => {
    vi.useFakeTimers();
    const send = vi.fn(() => true);
    for (const key of ['\x1b', 'y', 'n']) sendMenuKey(send, key, () => CODEX_SUBMIT_MENU(1));
    vi.advanceTimersByTime(MENU_SUBMIT_SETTLE_MS);
    expect(send.mock.calls).toEqual([['\x1b'], ['y'], ['n']]);
    expect(sendMenuKey(() => false, '1', () => CODEX_SUBMIT_MENU(1))).toBe(false);
  });

  it('sends no Enter when a different submit-style menu replaced the first', () => {
    vi.useFakeTimers();
    let screen = CODEX_SUBMIT_MENU(1);
    const send = vi.fn((bytes: string) => {
      if (bytes === '2') screen = CODEX_SUBMIT_MENU(2).replace('write_file', 'delete_file');
      return true;
    });
    sendMenuKey(send, '2', () => screen);
    vi.advanceTimersByTime(MENU_SUBMIT_SETTLE_MS);
    expect(send.mock.calls).toEqual([['2']]);
  });

  it('still confirms when only the focus and a moving "option N/M" footer changed', () => {
    vi.useFakeTimers();
    const menu = (focus: number) => CODEX_SUBMIT_MENU(focus).replace('enter to submit', `option ${focus}/2 | enter to submit`);
    expect(menuFingerprint(menu(1))).toBe(menuFingerprint(menu(2)));
    let screen = menu(1);
    const send = vi.fn((bytes: string) => { if (bytes === '2') screen = menu(2); return true; });
    sendMenuKey(send, '2', () => screen);
    vi.advanceTimersByTime(MENU_SUBMIT_SETTLE_MS);
    expect(send.mock.calls).toEqual([['2'], ['\r']]);
  });

  it('reads the live screen, not scrollback, from the pane snapshot getter', () => {
    const getter = vi.fn((opts?: { screen?: boolean }) => (opts?.screen ? 'screen' : 'scrollback'));
    expect(paneScreen(getter)).toBe('screen');
    expect(paneScreen(undefined)).toBeUndefined();
    expect(paneScreen(() => { throw new Error('disposed'); })).toBeUndefined();
  });

  it('re-checks the guard before the delayed Enter', () => {
    vi.useFakeTimers();
    let screen = CODEX_SUBMIT_MENU(2);
    let allowed = true;
    const written: string[] = [];
    const send = (bytes: string) => { if (!allowed) return false; written.push(bytes); if (bytes === '1') screen = CODEX_SUBMIT_MENU(1); return true; };
    sendMenuKey(send, '1', () => screen);
    allowed = false;
    vi.advanceTimersByTime(MENU_SUBMIT_SETTLE_MS);
    expect(written).toEqual(['1']);
  });
});
