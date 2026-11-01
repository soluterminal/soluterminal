// Hidden agent workers keep their terminal cores only while the grid hides
// them; with nothing hidden, zoom and mobile mount just the shown terminal.
import { test, expect } from './fixtures';
import type { Page } from 'playwright/test';
import type { Layout, PaneInfo, Workspace } from '../src/types';
import { buildNxMFromLeaves } from '../src/lib/layout';

async function setup(page: Page, withWorkers: boolean) {
  const ids = ['lead', 'w1', 'w2', 'human'];
  const panes = ids.map((id, i) => ({ id, title: id, cwd: '/p', cmd: ['sh'], status: 'running', state: 'idle', pid: i + 10, cols: 80, rows: 24,
    createdAt: '2026-10-30T00:00:00Z', lastActivityMs: 0,
    ...(withWorkers && id.startsWith('w') ? { origin: { source: 'agent', byPaneId: 'lead', leadPaneId: 'lead', workspace: 'P', at: i } } : {}) } as PaneInfo));
  const layout = buildNxMFromLeaves(ids.map(paneId => ({ type: 'pane', paneId } as Layout)), 2, 2);
  const ws = { name: 'P', layoutMode: 'free', paneIds: ids, panes, layout, activeAt: '2026-10-30T00:00:00Z' } as Workspace;
  await page.route('**/api/panes', r => r.fulfill({ json: panes }));
  await page.route('**/api/workspaces**', r => r.fulfill({ json: r.request().method() === 'POST' ? r.request().postDataJSON() : [ws] }));
  await page.routeWebSocket(/\/api\/panes\/.*\/io/, w => { w.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' })); w.send(Buffer.from('fixture\r\n')); });
  await page.goto('/?ws=P');
}
type TermWindow = { __soluTerms?: Map<string, unknown>; __w1?: unknown };
// Terminal sessions the app holds right now, by pane id.
const terms = (page: Page) => page.evaluate(() => [...((window as unknown as TermWindow).__soluTerms ?? new Map()).keys()].map(k => JSON.parse(k)[1] as string).sort());

test('hidden workers keep their terminals in the overview; revealing reuses it', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  await setup(page, true);
  await expect(page.locator('[data-pane-id="lead"] .xterm')).toBeVisible();
  await expect(page.locator('[data-pane-id="w1"] .xterm')).not.toBeVisible();
  await expect.poll(() => terms(page)).toEqual(['human', 'lead', 'w1', 'w2']);
  await page.evaluate(() => {
    const w = window as unknown as TermWindow;
    w.__w1 = [...w.__soluTerms!.entries()].find(([k]) => JSON.parse(k)[1] === 'w1')?.[1];
  });
  await page.getByRole('treeitem', { name: 'w1', exact: true }).locator(':scope > .workspace-pane-row').click();
  await expect(page.locator('[data-pane-id="w1"] .xterm')).toBeVisible();
  expect(await page.evaluate(() => {
    const w = window as unknown as TermWindow;
    return [...w.__soluTerms!.entries()].find(([k]) => JSON.parse(k)[1] === 'w1')?.[1] === w.__w1;
  })).toBe(true);
});

test('with nothing hidden, zoom mounts only the shown terminal', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  await setup(page, false);
  await expect.poll(() => terms(page)).toEqual(['human', 'lead', 'w1', 'w2']);
  await page.locator('[data-pane-id="lead"] .xterm').click();
  await page.keyboard.press('Alt+z');
  await expect.poll(() => terms(page)).toEqual(['lead']);
});

test('with nothing hidden, mobile mounts only the shown terminal', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setup(page, false);
  await expect.poll(() => terms(page)).toHaveLength(1);
});
