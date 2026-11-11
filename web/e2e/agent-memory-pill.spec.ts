import { expect, test } from './fixtures';
import type { Page } from 'playwright/test';
import type { PaneInfo, Workspace } from '../src/types';

const shots = process.env.PANE_HEADER_SHOTS;

async function openTwoPanes(page: Page, heavyMB: number) {
  await page.setViewportSize({ width: 1400, height: 800 });
  const heavy: PaneInfo = {
    id: 'heavy', title: 'Refactor the parser', cwd: '/home/user/app', cmd: ['codex'],
    status: 'running', pid: 11, cols: 80, rows: 24, state: 'idle', hasCodex: true,
    createdAt: '2026-11-11T00:00:00Z', lastActivityMs: Date.now(), agentRssMB: heavyMB,
    codexThreadId: 'heavy-thread', resumeReady: true,
  };
  const light: PaneInfo = { ...heavy, id: 'light', title: 'Run the tests', agentRssMB: 384, codexThreadId: 'light-thread' };
  const workspaces: Workspace[] = [{ name: 'Memory', layout: { type: 'split', direction: 'horizontal', ratio: 0.5, a: { type: 'pane', paneId: heavy.id }, b: { type: 'pane', paneId: light.id } }, paneIds: [heavy.id, light.id], activeAt: heavy.createdAt }];
  await page.route('**/api/panes', route => route.fulfill({ json: [heavy, light] }));
  await page.route('**/api/workspaces**', route => route.fulfill({ json: workspaces }));
  const statusCalls: string[] = [];
  await page.route('**/api/system/status**', route => { statusCalls.push(route.request().url()); return route.fulfill({ json: { host: { memTotalMB: 16384 } } }); });
  const restarts: string[] = [];
  await page.route('**/api/panes/heavy/restart**', route => { restarts.push(route.request().url()); return route.fulfill({ json: heavy }); });
  await page.routeWebSocket(/\/api\/panes\/(heavy|light)\/io/, ws => ws.send(Buffer.from('\r\nReady\r\n')));
  await page.goto('/?ws=Memory');
  return {
    heavy: page.locator('[data-pane-id="heavy"] .pane-header'),
    light: page.locator('[data-pane-id="light"] .pane-header'),
    restarts,
    statusCalls,
  };
}

test('unusual agent memory shows "Restart?" and restarts in place after a confirm; usual memory stays hidden', async ({ page }) => {
  const { heavy, light, restarts, statusCalls } = await openTwoPanes(page, 1843);
  const pill = heavy.getByRole('button', { name: 'Agent memory 1.8 GB. Restart?' });
  await expect(pill).toBeVisible();
  await expect(pill).toHaveText('1.8 GB · Restart?');
  await expect(light.locator('.pane-mem-chip')).toHaveCount(0);
  expect(statusCalls).toHaveLength(1);
  if (shots) await heavy.screenshot({ animations: 'disabled', path: `${shots}/memory-pill.png` });

  await pill.click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toContainText('Restart this agent?');
  await expect(dialog).toContainText('It is using 1.8 GB of memory. Restarting it frees that memory. The conversation is kept.');
  if (shots) await page.screenshot({ animations: 'disabled', path: `${shots}/memory-pill-confirm.png` });
  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toHaveCount(0);
  expect(restarts).toHaveLength(0);

  await pill.click();
  await page.getByRole('dialog').getByRole('button', { name: 'Restart' }).click();
  await expect.poll(() => restarts.length).toBe(1);
  expect(restarts[0]).not.toMatch(/fresh|force/);
});

test('"Always show agent memory" in Settings shows usual memory as a plain readout', async ({ page }) => {
  const { heavy, light, statusCalls } = await openTwoPanes(page, 700);
  await expect(heavy.locator('[data-pane-detail] .pane-mem-chip')).toHaveCount(0);
  await expect(light.locator('[data-pane-detail] .pane-mem-chip')).toHaveCount(0);

  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('tab', { name: 'Terminal & panes', exact: true }).click();
  await page.getByRole('checkbox', { name: 'Always show agent memory', exact: true }).check();
  await page.keyboard.press('Escape');

  const readout = light.getByLabel('Agent memory 384 MB');
  await expect(readout).toBeVisible();
  await expect(light.getByRole('button', { name: /Restart\?/ })).toHaveCount(0);
  await expect(heavy.getByLabel('Agent memory 700 MB')).toBeVisible();
  // Nothing passed the 1.5 GB floor, so the machine's RAM was never asked for.
  expect(statusCalls).toHaveLength(0);
  expect(await page.evaluate(() => localStorage.getItem('solu.alwaysShowAgentMemory'))).toBe('1');
  if (shots) await light.screenshot({ animations: 'disabled', path: `${shots}/memory-readout.png` });
});
