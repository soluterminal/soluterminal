import type { Page } from 'playwright/test';
import type { PaneInfo, Workspace } from '../src/types';
import { expect, test } from './fixtures';

// The footer StatusBar shares the grid column with the pane area. The pane
// area must shrink to make room for it at every window size and after a
// reload; otherwise .main clips the footer at the bottom of the window.
async function footerGap(page: Page) {
  return page.evaluate(() => {
    const bar = document.querySelector('.grid-panel > .statusbar')!.getBoundingClientRect();
    const panel = document.querySelector('.grid-panel')!.getBoundingClientRect();
    return {
      belowWindow: Math.round(bar.bottom - window.innerHeight),
      belowPanel: Math.round(bar.bottom - panel.bottom),
    };
  });
}

test('the footer stays inside the window at desktop sizes and after a reload', async ({ page }) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  const panes: PaneInfo[] = ['left', 'right'].map((side) => ({
    id: `footer_${side}`, title: side, cwd: '/tmp', cmd: ['bash'],
    status: 'running', createdAt: '2026-11-01T00:00:00Z', cols: 80, rows: 30,
    pid: 1, lastActivityMs: 0, state: 'idle',
  }));
  const workspaces: Workspace[] = [{
    name: 'Footer', layout: { type: 'split', direction: 'horizontal', ratio: 0.5,
      a: { type: 'pane', paneId: panes[0].id }, b: { type: 'pane', paneId: panes[1].id } },
    paneIds: panes.map((pane) => pane.id), activeAt: '2026-11-01T00:00:00Z',
  }];
  await page.addInitScript(() => localStorage.setItem('solu.activeWorkspace', 'Footer'));
  await page.route('**/api/panes', (route) => route.fulfill({ json: panes }));
  await page.route('**/api/workspaces**', (route) => route.fulfill({ json: workspaces }));
  await page.routeWebSocket(/\/api\/panes\/footer_.*\/io/, (ws) => ws.send(Buffer.from('\r\nready\r\n')));

  await page.goto('/');
  await expect(page.locator('.grid-panel > .statusbar')).toBeVisible();
  await expect(page.locator('[data-pane-id="footer_left"] .xterm')).toBeVisible();
  await expect.poll(() => footerGap(page)).toEqual({ belowWindow: 0, belowPanel: 0 });

  // A larger window (maximise), a smaller one (restore down), then a reload
  // in place: the reported cases where the footer went missing.
  for (const size of [{ width: 2560, height: 1392 }, { width: 1280, height: 800 }, { width: 2560, height: 1392 }]) {
    await page.setViewportSize(size);
    await expect.poll(() => footerGap(page)).toEqual({ belowWindow: 0, belowPanel: 0 });
  }
  await page.reload();
  await expect(page.locator('[data-pane-id="footer_left"] .xterm')).toBeVisible();
  await expect.poll(() => footerGap(page)).toEqual({ belowWindow: 0, belowPanel: 0 });

  // A banner that appears and goes away (a reconnect or update notice after a
  // reload or relaunch) shrinks the pane area without any window resize.
  await page.locator('.app-banner-row').evaluate((row) => {
    const banner = document.createElement('div');
    banner.id = 'transient-banner';
    banner.style.height = '40px';
    row.appendChild(banner);
  });
  await expect.poll(() => footerGap(page)).toEqual({ belowWindow: 0, belowPanel: 0 });
  await page.locator('#transient-banner').evaluate((banner) => banner.remove());
  await expect.poll(() => footerGap(page)).toEqual({ belowWindow: 0, belowPanel: 0 });

  // The pane area keeps its height when its contents are taller than the
  // space left above the footer (a terminal fitted to an earlier, larger
  // layout); it must not push the footer out.
  await page.locator('.grid-area').evaluate((el) => {
    const tall = document.createElement('div');
    tall.style.height = `${window.innerHeight * 2}px`;
    tall.dataset.test = 'tall';
    el.appendChild(tall);
  });
  expect(await footerGap(page)).toEqual({ belowWindow: 0, belowPanel: 0 });
});
