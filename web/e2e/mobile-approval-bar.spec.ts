import { expect, test } from './fixtures';
import type { PaneInfo, Workspace } from '../src/types';

// Phone viewport: a Claude pane at a permission menu gets one-tap answer keys
// above the reply field, the terminal is not covered by them, and a tap
// writes the bare key (no Enter after it) to the pane's socket.
test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

async function openMenuPane(page: import('playwright/test').Page) {
  const pane = { id: 'phone-menu', title: 'Claude', cwd: '/repo', cmd: ['claude'], status: 'running',
    pid: 31, cols: 44, rows: 30, state: 'choice-prompt', hasClaude: true,
    createdAt: '2026-10-28T00:00:00Z', lastActivityMs: 0 } as PaneInfo;
  const workspace: Workspace = { name: 'Phone', paneIds: [pane.id], panes: [pane],
    layout: { type: 'pane', paneId: pane.id }, activeAt: '2026-10-28T00:00:00Z' };
  await page.route('**/api/workflow/tasks', route => route.fulfill({ json: [] }));
  await page.route('**/api/panes', route => route.fulfill({ json: [pane] }));
  await page.route('**/api/workspaces**', route =>
    route.fulfill({ json: route.request().method() === 'POST' ? workspace : [workspace] }));
  const writes: { bytes: string; atMs: number }[] = [];
  await page.routeWebSocket(/\/api\/panes\/phone-menu\/io/, ws => {
    ws.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
    ws.send(Buffer.from([
      '', '● Bash(rm -rf build)', '',
      ' Bash command', '   rm -rf build', '   Remove old build output', '',
      ' Do you want to proceed?',
      ' ❯ 1. Yes',
      "   2. Yes, and don't ask again for rm",
      '   3. No, and tell Claude what to do',
      '      differently (esc)', '',
    ].join('\r\n')));
    ws.onMessage(data => {
      if (typeof data !== 'string') writes.push({ bytes: Buffer.from(data).toString('utf8'), atMs: Date.now() });
    });
  });
  await page.addInitScript(() => localStorage.setItem('solu.activeWorkspace', 'Phone'));
  await page.goto('/?ws=Phone');
  return writes;
}

async function expectKeysOnScreen(bar: import('playwright/test').Locator, width: number) {
  for (const button of await bar.getByRole('button').all()) {
    const box = (await button.boundingBox())!;
    expect(box.height).toBeGreaterThanOrEqual(44);
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(width);
    // Label fits inside its button (no clipped "Esc · Cancel").
    expect(await button.evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
  }
}

test('phone answers a terminal choice prompt with one tap', async ({ page }, testInfo) => {
  const pane = { id: 'phone-menu' };
  const writes = await openMenuPane(page);
  const terminal = page.locator(`[data-pane-id="${pane.id}"]`);
  await expect(terminal.locator('.xterm')).toBeVisible();
  const bar = page.getByRole('group', { name: 'Answer terminal prompt' });
  await expect(bar).toBeVisible();
  await expect(bar.getByRole('button')).toHaveText(['1', '2', '3', '⏎ Enter', 'Esc · Cancel']);
  // Desktop strip is not shown alongside the phone bar.
  await expect(page.locator('.approval-actions')).toBeHidden();

  // Every key is a full touch target and stays on screen.
  await expectKeysOnScreen(bar, 390);
  // The answer bar and reply field sit below the terminal, not over it.
  const reply = (await page.locator('.mobile-quick-reply').boundingBox())!;
  await expect.poll(async () => (await terminal.locator('.xterm-screen').boundingBox())!.y
    + (await terminal.locator('.xterm-screen').boundingBox())!.height).toBeLessThanOrEqual(reply.y + 1);
  const tabbar = (await page.locator('.mobile-bottombar').boundingBox())!;
  expect(reply.y + reply.height).toBeLessThanOrEqual(tabbar.y + 1);

  await page.screenshot({ path: testInfo.outputPath('mobile-approval-bar-390x844.png') });
  if (process.env.SOLU_E2E_SCREENSHOT) await page.screenshot({ path: process.env.SOLU_E2E_SCREENSHOT });

  // Wait out writes from attach (resize nudges etc.), then tap 1 once.
  await page.waitForTimeout(500);
  const before = writes.length;
  await bar.getByRole('button', { name: 'Press 1 — menu option 1' }).tap();
  await expect.poll(() => writes.slice(before).map(w => w.bytes)).toContain('1');
  await page.waitForTimeout(1000);
  expect(writes.slice(before).map(w => w.bytes)).toEqual(['1']);

  // With the on-screen keys open, the stack is terminal / keys / answer bar
  // + reply / tab bar, each above the next and none over the terminal.
  await page.getByRole('button', { name: 'Show on-screen keys' }).tap();
  const keybar = page.locator('.mobile-keybar');
  await expect(keybar).toBeVisible();
  await expect.poll(async () => {
    const k = (await keybar.boundingBox())!;
    const r = (await page.locator('.mobile-quick-reply').boundingBox())!;
    const t = (await terminal.locator('.xterm-screen').boundingBox())!;
    return k.y + k.height <= r.y + 1 && t.y + t.height <= k.y + 1;
  }).toBe(true);
  await page.screenshot({ path: testInfo.outputPath('mobile-approval-bar-keybar-390x844.png') });
  if (process.env.SOLU_E2E_SCREENSHOT) await page.screenshot({ path: process.env.SOLU_E2E_SCREENSHOT.replace(/\.png$/, '-keybar.png') });
});

test.describe('narrowest phones', () => {
  test.use({ viewport: { width: 320, height: 568 } });
  test('every answer key stays on screen at 320px', async ({ page }, testInfo) => {
    await openMenuPane(page);
    const bar = page.getByRole('group', { name: 'Answer terminal prompt' });
    await expect(bar).toBeVisible();
    await expect(bar.getByRole('button')).toHaveText(['1', '2', '3', '⏎', 'Esc'], { useInnerText: true });
    await expectKeysOnScreen(bar, 320);
    // One row: all keys share the first key's top edge.
    const tops = await bar.getByRole('button').evaluateAll(els => els.map(el => Math.round(el.getBoundingClientRect().top)));
    expect(new Set(tops).size).toBe(1);
    await page.screenshot({ path: testInfo.outputPath('mobile-approval-bar-320x568.png') });
    if (process.env.SOLU_E2E_SCREENSHOT) await page.screenshot({ path: process.env.SOLU_E2E_SCREENSHOT.replace(/390x844\.png$/, '320x568.png') });
  });
});
