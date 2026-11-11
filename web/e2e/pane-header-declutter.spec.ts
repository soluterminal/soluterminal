import { expect, test } from './fixtures';
import type { PaneInfo, Workspace } from '../src/types';

const shots = process.env.PANE_HEADER_SHOTS;

test('a quiet pane header shows its title; the number, Copy ID and Maximise are one step away', async ({ page }) => {
  await page.setViewportSize({ width: 1400, height: 800 });
  const pane: PaneInfo = {
    id: 'quiet', title: 'Fix the login page', cwd: '/home/user/app', cmd: ['codex'],
    status: 'running', pid: 11, cols: 80, rows: 24, state: 'idle', hasCodex: true,
    createdAt: '2026-11-10T00:00:00Z', lastActivityMs: Date.now(), agentRssMB: 111,
    codexThreadId: 'quiet-thread', resumeReady: true,
  };
  const other: PaneInfo = { ...pane, id: 'other', title: 'Run the tests', codexThreadId: 'other-thread' };
  const workspaces: Workspace[] = [{ name: 'Quiet', layout: { type: 'split', direction: 'horizontal', ratio: 0.5, a: { type: 'pane', paneId: pane.id }, b: { type: 'pane', paneId: other.id } }, paneIds: [pane.id, other.id], activeAt: pane.createdAt }];
  await page.route('**/api/panes', route => route.fulfill({ json: [pane, other] }));
  await page.route('**/api/workspaces**', route => route.fulfill({ json: workspaces }));
  await page.routeWebSocket(/\/api\/panes\/(quiet|other)\/io/, ws => ws.send(Buffer.from('\r\nReady\r\n')));
  await page.goto('/?ws=Quiet');
  const header = page.locator('[data-pane-id="quiet"] .pane-header');
  await expect(header.locator('.pane-title')).toHaveText('Fix the login page');

  // Manual auto-approve is the default and says nothing; Copy ID and Maximise left the header.
  await expect(header.getByRole('button', { name: /Auto-approve/ })).toHaveCount(0);
  await expect(header.getByRole('button', { name: /Copy|Maximise pane/ })).toHaveCount(0);
  await expect(header.getByRole('button', { name: 'Pane menu', exact: true })).toBeVisible();
  await expect(header.getByRole('button', { name: 'Close session', exact: true })).toBeVisible();
  if (shots) await header.screenshot({ animations: 'disabled', path: `${shots}/header-quiet.png` });

  // The pane number shows only while Alt is held, and moves nothing.
  const badge = header.locator('.pane-index-badge');
  await expect(badge).toBeHidden();
  const titleBefore = (await header.locator('.pane-title').boundingBox())!;
  await page.keyboard.down('Alt');
  await expect(badge).toBeVisible();
  await expect(badge).toHaveText('#1');
  await expect(page.locator('[data-pane-id="other"] .pane-index-badge')).toHaveText('#2');
  expect((await header.locator('.pane-title').boundingBox())!).toEqual(titleBefore);
  const headerBox = (await header.boundingBox())!;
  const badgeBox = (await badge.boundingBox())!;
  expect(badgeBox.x).toBeGreaterThanOrEqual(headerBox.x);
  expect(badgeBox.y + badgeBox.height).toBeLessThanOrEqual(headerBox.y + headerBox.height + 1);
  if (shots) await header.screenshot({ animations: 'disabled', path: `${shots}/header-alt-held.png` });
  await page.keyboard.up('Alt');
  await expect(badge).toBeHidden();

  // Copy thread ID and Maximise live in the pane menu.
  await header.getByRole('button', { name: 'Pane menu', exact: true }).click();
  if (shots) await page.screenshot({ animations: 'disabled', path: `${shots}/pane-menu.png` });
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  await page.getByRole('menuitem', { name: 'Copy thread ID', exact: true }).click();
  await expect(page.getByText('Thread ID copied', { exact: true })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe('quiet-thread');
  await header.getByRole('button', { name: 'Pane menu', exact: true }).click();
  await page.getByRole('menuitem', { name: /^Maximise pane/ }).click();
  const restore = header.getByRole('button', { name: 'Restore pane', exact: true });
  await expect(restore).toBeVisible();
  await expect(page.locator('[data-pane-id="other"] .pane-header')).toBeHidden();
  if (shots) await header.screenshot({ animations: 'disabled', path: `${shots}/header-maximised.png` });
  await header.getByRole('button', { name: 'Pane menu', exact: true }).click();
  await expect(page.getByRole('menuitem', { name: /^Restore layout/ })).toBeVisible();
  await page.keyboard.press('Escape');
  await restore.click();
  await expect(restore).toHaveCount(0);
  await expect(page.locator('[data-pane-id="other"] .pane-header')).toBeVisible();
});
