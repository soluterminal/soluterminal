import { expect, test } from './fixtures';
import type { PaneInfo, Workspace } from '../src/types';

const widths = [1200, 600, 400, 320, 256];
const before = process.env.PANE_HEADER_BEFORE === '1';

test('busy pane header keeps readable details and keyboard access at every desktop width', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 3000, height: 900 });
  const now = Date.now();
  const pane: PaneInfo = {
    id: 'overflow_header', title: 'Fix approval header overflow', cwd: '/home/user/launch', cmd: ['codex'],
    status: 'running', pid: 11, cols: 80, rows: 24, state: 'busy', hasCodex: true,
    createdAt: '2026-10-31T00:00:00Z', lastActivityMs: now, autoMode: 'always', agentRssMB: 111,
    codexThreadId: 'overflow-header-session', resumeReady: true,
    identity: { handle: 'header-fix', workingOn: 'Keep pane details readable' },
    origin: { source: 'agent', byPaneId: 'lead', leadPaneId: 'lead', byPaneTitle: 'Lead', at: now },
    lastAgentMessage: { fromPaneId: 'lead', fromLabel: '@lead', at: now, outcome: 'sent' },
    review: { verdict: 'approve', reviewer: 'claude', author: 'codex', round: 1, mustFix: 0, notes: 0, at: now },
    credential: { account: 'work', activeAccount: 'work', liveState: 'switching' },
  };
  const lead: PaneInfo = { ...pane, id: 'lead', title: 'Lead', identity: undefined, origin: undefined, lastAgentMessage: undefined };
  const workspaces: Workspace[] = [{ name: 'Overflow header', layout: { type: 'split', direction: 'horizontal', ratio: 0.8, a: { type: 'pane', paneId: pane.id }, b: { type: 'pane', paneId: lead.id } }, paneIds: [pane.id, lead.id], activeAt: pane.createdAt }];
  await page.route('**/api/panes', route => route.fulfill({ json: [pane, lead] }));
  await page.route('**/api/workspaces**', route => route.fulfill({ json: workspaces }));
  await page.route('**/api/schedules', route => route.fulfill({ json: [{
    id: 'header-schedule', name: 'Header check', paneId: pane.id, cron: '0 9 * * *', prompt: 'Check the header', enabled: true,
    nextFire: new Date(now + 720_000).toISOString(), createdAt: pane.createdAt, fireCount: 0, skipCount: 0, errorCount: 0,
  }] }));
  await page.routeWebSocket(/\/api\/panes\/overflow_header\/io/, ws => ws.send(Buffer.from('\r\nReady\r\n')));
  await page.goto('/?ws=Overflow%20header');
  const header = page.locator(`[data-pane-id="${pane.id}"] .pane-header`);
  await expect(header.locator('.pane-title')).toBeVisible();
  for (const width of widths) {
    await header.evaluate((el, width) => { el.style.width = `${width}px`; el.style.maxWidth = 'none'; }, width);
    await page.waitForTimeout(250);
    if (!before) {
      const outer = (await header.boundingBox())!;
      expect((await header.locator('.pane-title').boundingBox())!.width).toBeGreaterThanOrEqual(60);
      for (const name of ['Pane menu', 'Close session']) {
        const button = header.getByRole('button', { name, exact: true });
        expect((await button.boundingBox())!.x + (await button.boundingBox())!.width).toBeLessThanOrEqual(outer.x + outer.width + 1);
        expect((await button.boundingBox())!.y).toBeLessThanOrEqual((await header.locator('.pane-title').boundingBox())!.y + 4);
        await button.click({ trial: true });
      }
      for (const chip of await header.locator('[data-pane-detail] button:visible').all()) {
        expect(await chip.evaluate(el => el.scrollWidth <= el.clientWidth + 1)).toBe(true);
      }
      if (width === 1200) await expect(header.getByRole('button', { name: /more pane details/ })).toHaveCount(0);
      if (width <= 600) {
        const more = header.getByRole('button', { name: /\d+ more pane details/ });
        await expect(more).toBeVisible();
        await more.focus();
        await page.keyboard.press('Enter');
        const details = page.getByRole('dialog', { name: 'More pane details', exact: true });
        await expect(details).toBeFocused();
        for (const selector of ['.pane-automode-badge', '.pane-resume-badge', '.pane-identity-chip', '.pane-origin', '.pane-message-chip', '.pane-review-chip', '.pane-schedule-chip', '.pane-account-chip', '.pane-mem-chip', '.pane-zoom-btn']) {
          await expect(details.locator(selector)).toBeVisible();
        }
        await expect(more).toHaveAttribute('aria-label', `${await details.locator('.pane-detail-row').count()} more pane details`);
        await page.locator('[data-pane-id="overflow_header"] .xterm-viewport').dispatchEvent('scroll');
        await expect(details).toBeVisible();
        await details.locator('.pane-mem-chip').dblclick();
        await expect(details.getByRole('button', { name: 'Maximise pane', exact: true })).toBeVisible();
        await details.screenshot({ animations: 'disabled', path: testInfo.outputPath(`after-popover-${width}.png`) });
        await details.getByRole('button', { name: /Auto-approve:/ }).click();
        await expect(page.getByRole('menu', { name: 'Auto-approve', exact: true })).toBeVisible();
        await page.keyboard.press('Escape');
        await details.locator('.pane-resume-badge').focus();
        await page.keyboard.press('Escape');
        await expect(details).toBeHidden();
        await expect(more).toBeFocused();
      }
    }
    await page.screenshot({ animations: 'disabled', clip: (await header.boundingBox())!, path: testInfo.outputPath(`${before ? 'before' : 'after'}-${width}.png`) });
  }
  await page.setViewportSize({ width: 390, height: 844 });
  await header.evaluate(el => { el.style.width = ''; el.style.maxWidth = ''; });
  await page.waitForTimeout(250);
  await expect(header.getByRole('button', { name: /more pane details/ })).toHaveCount(0);
  await page.screenshot({ animations: 'disabled', clip: (await header.boundingBox())!, path: testInfo.outputPath(`${before ? 'before' : 'after'}-phone.png`) });
  if (!before) {
    // Tauri keeps desktop mode below 800px. The same mode decision must
    // enable both measurement and CSS, without a fold/unfold loop.
    await page.evaluate(() => { (window as unknown as { __SOLU_TAURI__: boolean }).__SOLU_TAURI__ = true; });
    await page.setViewportSize({ width: 600, height: 900 });
    await header.getByRole('button', { name: /more pane details/ }).click();
    await page.getByRole('dialog', { name: 'More pane details', exact: true }).getByRole('button', { name: 'Maximise pane', exact: true }).click();
    await page.waitForTimeout(250);
    for (const width of [400, 320, 256]) {
      await header.evaluate((el, width) => { el.style.width = `${width}px`; }, width);
      await expect(header.getByRole('button', { name: /more pane details/ })).toBeVisible();
      await page.waitForTimeout(250);
      const samples = await header.evaluate(async el => {
        const samples: (string | null)[] = [];
        for (let n = 0; n < 8; n++) {
          samples.push(el.querySelector('.pane-details-more')?.textContent ?? null);
          await new Promise(resolve => setTimeout(resolve, 50));
        }
        return samples;
      });
      expect(samples.every(value => value === samples[0] && value !== null)).toBe(true);
      const title = (await header.locator('.pane-title').boundingBox())!;
      for (const name of ['Pane menu', 'Close session']) {
        const button = header.getByRole('button', { name, exact: true });
        expect((await button.boundingBox())!.y).toBeLessThanOrEqual(title.y + 4);
        await button.click({ trial: true });
      }
    }
    await page.screenshot({ animations: 'disabled', clip: (await header.boundingBox())!, path: testInfo.outputPath('after-tauri-600.png') });
    // A message from someone other than the origin must count separately,
    // including below the old 23rem message-hiding container tier.
    pane.origin = { source: 'schedule', at: now };
    await page.setViewportSize({ width: 3000, height: 900 });
    await page.reload();
    await expect(header.locator('.pane-title')).toBeVisible();
    for (const width of [320, 256]) {
      await header.evaluate((el, width) => { el.style.width = `${width}px`; }, width);
      await page.waitForTimeout(250);
      const more = header.getByRole('button', { name: /more pane details/ });
      await more.click();
      const details = page.getByRole('dialog', { name: 'More pane details', exact: true });
      await expect(details.locator('.pane-message-chip')).toBeVisible();
      await expect(more).toHaveAttribute('aria-label', `${await details.locator('.pane-detail-row').count()} more pane details`);
      if (width === 256) await details.screenshot({ animations: 'disabled', path: testInfo.outputPath('after-separate-message-256.png') });
      await details.locator('.pane-resume-badge').focus();
      await page.keyboard.press('Escape');
    }
  }
});

for (const dismiss of ['fold', 'close'] as const) {
  test(`renderer confirmation cancels on ${dismiss} and leaves terminal Enter/Escape intact`, async ({ page }) => {
    await page.setViewportSize({ width: 1800, height: 900 });
    const pane: PaneInfo = {
      id: 'mode-confirm', title: 'Busy renderer switch', cwd: '/', cmd: ['codex'],
      status: 'running', pid: 1, cols: 80, rows: 24, state: 'busy', hasCodex: true,
      createdAt: '', lastActivityMs: Date.now(), codexThreadId: 'mode-confirm-thread',
    };
    const workspace: Workspace = { name: 'Confirm', layout: { type: 'pane', paneId: pane.id }, paneIds: [pane.id], activeAt: '' };
    const input: string[] = [];
    const switches: string[] = [];
    await page.route('**/api/health', route => route.fulfill({ json: { status: 'ok', version: 'dev', focusEnabled: true } }));
    await page.route('**/api/panes', route => route.fulfill({ json: [pane] }));
    await page.route('**/api/workspaces**', route => route.fulfill({ json: [workspace] }));
    await page.route('**/api/panes/mode-confirm/switch-mode', route => { switches.push(route.request().postData() ?? ''); return route.fulfill({ json: pane }); });
    await page.routeWebSocket(/\/api\/panes\/mode-confirm\/io/, ws => {
      const grant = () => ws.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
      ws.onMessage(message => {
        if (typeof message !== 'string') input.push(Buffer.from(message).toString());
        else if (JSON.parse(message).type === 'claim') grant();
      });
      grant();
      ws.send(Buffer.from('\r\nReady\r\n'));
    });
    await page.goto('/?ws=Confirm');
    const terminal = page.locator('[data-pane-id="mode-confirm"]');
    const header = terminal.locator('.pane-header');
    await expect(header.getByRole('button', { name: 'Focus renderer', exact: true })).toBeVisible();
    if (dismiss === 'close') {
      await header.evaluate(el => { el.style.width = '256px'; });
      await header.getByRole('button', { name: /more pane details/ }).click();
    }
    await page.getByRole('button', { name: 'Focus renderer', exact: true }).click();
    await expect(page.getByRole('alertdialog')).toBeVisible();
    if (dismiss === 'fold') {
      await header.evaluate(el => { el.style.width = '256px'; });
      await expect(header.getByRole('button', { name: /more pane details/ })).toBeVisible();
    } else await header.getByRole('button', { name: /more pane details/ }).click();
    await expect(page.getByRole('alertdialog', { includeHidden: true })).toHaveCount(0);
    await terminal.locator('.xterm-helper-textarea').focus();
    await page.keyboard.press('Enter');
    await page.keyboard.press('Escape');
    await expect.poll(() => input.join('')).toContain('\r\x1b');
    expect(switches).toEqual([]);
    await header.getByRole('button', { name: /more pane details/ }).click();
    await expect(page.getByRole('button', { name: 'Focus renderer', exact: true })).toBeVisible();
    await expect(page.getByRole('alertdialog', { includeHidden: true })).toHaveCount(0);
  });
}
