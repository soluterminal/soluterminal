import type { Page } from 'playwright/test';
import type { Terminal } from '@xterm/xterm';
import type { PaneInfo, Workspace } from '../src/types';
import { expect, test } from './fixtures';

// Feedback #73 (ST-306): a Windows IME opened its candidate bar at the app
// window's top-left corner instead of at the pane's caret. The IME anchors
// the bar to xterm's focused helper textarea. These tests start a real
// composition on the textarea and check where the textarea is at that moment.

type TerminalWindow = Window & { __soluTerms: Map<string, Terminal> };

const pane = {
  id: 'ime-pane', title: 'Claude', cwd: '/tmp', cmd: ['claude'], status: 'running',
  pid: 7, cols: 80, rows: 24, state: 'idle', createdAt: '2026-11-19T00:00:00Z', lastActivityMs: 0, hasClaude: true,
} as PaneInfo;
const workspaces: Workspace[] = [
  { name: 'Main', paneIds: [pane.id], panes: [pane], layout: { type: 'pane', paneId: pane.id }, activeAt: '2026-11-19T00:00:00Z' },
];

async function open(page: Page, opts: { width: number; height: number; desktopShell?: boolean; output?: string }) {
  await page.setViewportSize({ width: opts.width, height: opts.height });
  if (opts.desktopShell) {
    // The marker the desktop app's init script sets (lib.rs).
    await page.addInitScript(() => { (window as unknown as { __SOLU_TAURI__: boolean }).__SOLU_TAURI__ = true; });
  }
  await page.route('**/api/panes', (route) => route.fulfill({ json: [pane] }));
  await page.route('**/api/workspaces**', (route) => route.fulfill({ json: workspaces }));
  // An idle Claude pane: nothing (or one prompt) arrives after the terminal
  // opens, so xterm gets no later cursor move to place its textarea from.
  await page.routeWebSocket(/\/api\/panes\/ime-pane\/io/, (ws) => {
    if (opts.output) ws.send(Buffer.from(opts.output));
  });
  await page.goto('/');
  await expect(page.locator('[data-pane-id="ime-pane"] .xterm-screen')).toBeVisible();
  // Let mount, fit and the settle pass finish.
  await page.waitForTimeout(800);
}

type Rect = { left: number; top: number; width: number; height: number };

/** Focus the pane's textarea the way a click does, then start a composition
 *  the way a Windows IME does. The IME may read the caret at any of these
 *  points, so the textarea's rect is taken after focus, after the IME's
 *  Process keydown, and at compositionstart. */
async function composeAndMeasure(page: Page) {
  // A mouse press on the terminal, by coordinates: at phone widths the
  // viewport, not the screen, takes the pointer.
  const box = (await page.locator('[data-pane-id="ime-pane"] .xterm-screen').boundingBox())!;
  await page.mouse.click(box.x + 5, box.y + 5);
  return page.evaluate(() => {
    const root = document.querySelector('[data-pane-id="ime-pane"]')!;
    const ta = root.querySelector<HTMLTextAreaElement>('.xterm-helper-textarea')!;
    const screen = root.querySelector<HTMLElement>('.xterm-screen')!.getBoundingClientRect();
    const rect = () => { const r = ta.getBoundingClientRect(); return { left: r.left, top: r.top, width: r.width, height: r.height }; };
    const focused = document.activeElement === ta;
    const afterFocus = rect();
    ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Process', keyCode: 229, bubbles: true, cancelable: true } as KeyboardEventInit));
    const afterProcess = rect();
    let atStart: Rect | null = null;
    // A bubble listener on the document runs after every listener on the
    // textarea, including xterm's own compositionstart handler.
    document.addEventListener('compositionstart', () => { atStart = rect(); }, { once: true });
    ta.dispatchEvent(new CompositionEvent('compositionstart', { data: '', bubbles: true }));
    ta.dispatchEvent(new CompositionEvent('compositionend', { data: '', bubbles: true }));
    const term = [...(window as unknown as TerminalWindow).__soluTerms.values()]
      .find((t) => t.element?.closest('[data-pane-id]') === root)!;
    const buf = term.buffer.active;
    return {
      focused,
      rects: { afterFocus, afterProcess, atStart: atStart as Rect | null },
      screen: { left: screen.left, top: screen.top, right: screen.right, bottom: screen.bottom },
      cell: { width: screen.width / term.cols, height: screen.height / term.rows },
      cursor: { x: buf.cursorX, y: buf.cursorY },
    };
  });
}

function expectAtCaret(m: Awaited<ReturnType<typeof composeAndMeasure>>) {
  expect(m.focused).toBe(true);
  expect(m.rects.atStart).not.toBeNull();
  for (const [when, ta] of Object.entries(m.rects) as [string, Rect][]) {
    // Inside the pane, not at the window's corner or off its left edge...
    expect(ta.left, when).toBeGreaterThanOrEqual(m.screen.left - 1);
    expect(ta.top, when).toBeGreaterThanOrEqual(m.screen.top - 1);
    expect(ta.left, when).toBeLessThan(m.screen.right);
    expect(ta.top, when).toBeLessThan(m.screen.bottom);
    // ...on the cursor cell, with a real caret size.
    expect(Math.abs(ta.left - (m.screen.left + m.cursor.x * m.cell.width)), when).toBeLessThan(2);
    expect(Math.abs(ta.top - (m.screen.top + m.cursor.y * m.cell.height)), when).toBeLessThan(2);
    expect(ta.width, when).toBeGreaterThanOrEqual(1);
    expect(ta.height, when).toBeGreaterThanOrEqual(1);
  }
}

test('an idle pane anchors the IME at its caret when a composition starts', async ({ page }) => {
  await open(page, { width: 1280, height: 800 });
  expectAtCaret(await composeAndMeasure(page));
});

test('the IME follows the caret after output moved it', async ({ page }) => {
  await open(page, { width: 1280, height: 800, output: '\x1b[6;12H> ' });
  const m = await composeAndMeasure(page);
  expect(m.cursor).toEqual({ x: 13, y: 5 });
  expectAtCaret(m);
});

test('a narrow desktop window keeps the textarea at the caret (no phone pin)', async ({ page }) => {
  await open(page, { width: 700, height: 700, desktopShell: true, output: 'prompt> ' });
  await expect(page.locator('body')).toHaveClass(/\bdesktop-shell\b/);
  expectAtCaret(await composeAndMeasure(page));
});

test('a phone keeps the textarea pinned off-screen', async ({ page }) => {
  await open(page, { width: 400, height: 800, output: 'prompt> ' });
  await expect(page.locator('body')).not.toHaveClass(/\bdesktop-shell\b/);
  const left = await page.locator('[data-pane-id="ime-pane"] .xterm-helper-textarea')
    .evaluate((ta) => ta.getBoundingClientRect().left);
  expect(left).toBeLessThan(-1000);
});
