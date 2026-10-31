import { test, expect } from './fixtures';
import type { Page } from 'playwright/test';
import type { Layout, PaneInfo, Workspace } from '../src/types';
import { buildNxMFromLeaves, removePane } from '../src/lib/layout';

const ids = ['tree-lead', 'tree-w1', 'tree-w2', 'tree-w3', 'tree-sub', 'tree-human'];
const labels = ['Release lead', 'Build worker', 'Finished worker', 'Missing layout worker', 'Approval sub-worker', 'My shell'];
async function setup(page: Page, nativeWorker = false) {
  const panes = ids.map((id, i) => ({
    id, title: labels[i], cwd: '/project', cmd: ['sh'], status: 'running', state: 'idle', pid: i + 10, cols: 80, rows: 24,
    createdAt: '2026-10-30T00:00:00Z', lastActivityMs: 0,
    ...(nativeWorker && i === 3 ? { kind: 'native', agent: 'claude', hasClaude: true } : {}),
    ...(i > 0 && i < 5 ? { origin: { source: 'agent', byPaneId: i === 4 ? ids[1] : ids[0], leadPaneId: ids[0], workspace: 'Tree project', at: i } } : {}),
    ...(i === 4 ? { state: 'choice-prompt', attention: 'needs-user' } : {}),
    ...(i === 2 ? { turn: { active: false, count: 1, last: { n: 1, startedMs: 1, endedMs: Date.now(), outcome: 'completed', files: 0, added: 0, removed: 0 } } } : {}),
  } as PaneInfo));
  const savedIds = ids.filter(id => id !== 'tree-w3');
  const layout = buildNxMFromLeaves(savedIds.map(paneId => ({ type: 'pane', paneId } as Layout)), 2, 3);
  const workspace = { name: 'Tree project', layoutMode: 'free', paneIds: savedIds, panes: panes.filter(p => savedIds.includes(p.id)), layout, activeAt: '2026-10-30T00:00:00Z' } as Workspace;
  const saves: Workspace[] = [];
  const terminalInputs: Array<{ paneId: string; data: string }> = [];
  await page.route('**/api/panes', route => route.fulfill({ json: panes }));
  await page.route('**/api/workspaces**', route => {
    if (route.request().method() === 'POST') saves.push(route.request().postDataJSON());
    return route.fulfill({ json: route.request().method() === 'POST' ? route.request().postDataJSON() : [workspace] });
  });
  await page.routeWebSocket(/\/api\/panes\/.*\/io/, ws => {
    ws.onMessage(data => terminalInputs.push({ paneId: new URL(ws.url()).pathname.split('/').at(-2)!, data: typeof data === 'string' ? data : data.toString() }));
    ws.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
    ws.send(Buffer.from('\r\nWorkspace tree acceptance fixture\r\n'));
  });
  await page.goto('/?ws=Tree%20project');
  return { saves, layout, workspace, terminalInputs };
}
const row = (page: Page, name: string) => page.getByRole('treeitem', { name, exact: true });
async function visible(page: Page, expected: string[]) {
  await expect.poll(async () => {
    const result = [];
    for (const id of ids) if (await page.locator(`[data-pane-id="${id}"] .xterm`).isVisible()) result.push(id);
    return result;
  }).toEqual(ids.filter(id => expected.includes(id)));
}

test('workspace, lead, nested lead, worker and return preserve the saved layout', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  const { saves, workspace, terminalInputs } = await setup(page);
  const full = ids.filter(id => id !== 'tree-w3');
  await visible(page, full);
  await page.evaluate(() => {
    const w = window as unknown as { __soluTerms: Map<string, unknown>; __treeOriginalHuman?: unknown };
    w.__treeOriginalHuman = [...w.__soluTerms.entries()].find(([key]) => JSON.parse(key)[1] === 'tree-human')?.[1];
  });
  await expect(row(page, 'Approval sub-worker')).toHaveAttribute('aria-level', '4');
  await expect(page.getByText('Started by agents', { exact: true })).toHaveCount(0);
  await page.screenshot({ animations: 'disabled', path: testInfo.outputPath('workspace-tree.png') });
  await row(page, 'Release lead').locator(':scope > .workspace-pane-row').click();
  await visible(page, ids.filter(id => id !== 'tree-human'));
  await page.screenshot({ animations: 'disabled', path: testInfo.outputPath('team-view.png') });
  await expect(page.locator('[data-pane-id="tree-w1"] .pane-header')).toHaveAttribute('draggable', 'false');
  // Even a synthetic drop (e.g. a drag begun before the view opened) cannot
  // rearrange or autosave the full workspace while a temporary view is active.
  await page.locator('[data-pane-id="tree-w2"]').evaluate(element => {
    const dataTransfer = new DataTransfer();
    dataTransfer.setData('application/x-solu-pane', 'tree-w1');
    element.dispatchEvent(new DragEvent('dragover', { bubbles: true, cancelable: true, dataTransfer }));
    element.dispatchEvent(new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer }));
  });
  await visible(page, ids.filter(id => id !== 'tree-human'));
  await row(page, 'Build worker').locator(':scope > .workspace-pane-row').click();
  await visible(page, ['tree-w1', 'tree-sub']);
  await row(page, 'Approval sub-worker').locator(':scope > .workspace-pane-row').click();
  await visible(page, ['tree-sub']);
  await row(page, 'Finished worker').locator(':scope > .workspace-pane-row').click();
  await visible(page, ['tree-w2']);
  await expect(row(page, 'Finished worker')).toContainText('finished');
  await expect(row(page, 'Finished worker')).not.toContainText('close?');
  await page.screenshot({ animations: 'disabled', path: testInfo.outputPath('single-view.png') });
  const focusedPane = page.locator('[data-pane-id="tree-w2"]');
  await focusedPane.locator('.pane-title').dblclick();
  await expect(focusedPane.locator('.pane-title-input')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(focusedPane.locator('.pane-title-input')).toHaveCount(0);
  await visible(page, ['tree-w2']);
  await focusedPane.locator('.xterm-helper-textarea').focus();
  await page.keyboard.press('Escape');
  await expect.poll(() => terminalInputs.some(input => input.paneId === 'tree-w2' && input.data.length === 1 && input.data.charCodeAt(0) === 27)).toBe(true);
  await visible(page, ['tree-w2']);
  await row(page, 'Finished worker').focus();
  await page.keyboard.press('Escape');
  await visible(page, full);
  expect(await page.evaluate(() => {
    const w = window as unknown as { __soluTerms: Map<string, unknown>; __treeOriginalHuman?: unknown };
    return [...w.__soluTerms.entries()].find(([key]) => JSON.parse(key)[1] === 'tree-human')?.[1] === w.__treeOriginalHuman;
  })).toBe(true);
  await row(page, 'Missing layout worker').locator(':scope > .workspace-pane-row').click();
  await visible(page, ['tree-w3']);
  await page.locator('.ws-row .ws-name').click();
  await visible(page, full);
  await page.getByRole('button', { name: 'Leads only in Tree project' }).click();
  await visible(page, ['tree-lead', 'tree-w1']);
  await expect(row(page, 'Finished worker')).toHaveCount(0);
  await page.screenshot({ animations: 'disabled', path: testInfo.outputPath('leads-only.png') });
  await row(page, 'Release lead').focus();
  await page.keyboard.press('Escape');
  await visible(page, full);
  // Collapse preserves the attention dot and descendant count; keyboard moves
  // through visible rows and expands the same branch.
  await page.getByRole('button', { name: 'Collapse Release lead', exact: true }).click();
  await expect(row(page, 'Release lead')).toContainText('4 workers');
  await expect(row(page, 'Release lead').locator('.pane-tree-dot')).toHaveClass(/is-needs-you/);
  await expect(page.locator('.ws-row .pane-tree-dot')).toHaveClass(/is-needs-you/);
  await page.screenshot({ animations: 'disabled', path: testInfo.outputPath('collapsed-attention.png') });
  await page.setViewportSize({ width: 1000, height: 800 });
  await page.screenshot({ animations: 'disabled', path: testInfo.outputPath('narrow-tree.png') });
  await page.setViewportSize({ width: 1440, height: 960 });
  await page.reload();
  await expect(row(page, 'Release lead')).toHaveAttribute('aria-expanded', 'false');
  await row(page, 'Release lead').focus();
  await page.keyboard.press('ArrowRight');
  await expect(row(page, 'Release lead')).toHaveAttribute('aria-expanded', 'true');
  await page.keyboard.press('ArrowDown');
  await expect(row(page, 'Build worker')).toBeFocused();
  await page.keyboard.press('Enter');
  await visible(page, ['tree-w1', 'tree-sub']);
  // View changes themselves must not write any layout at all. The store
  // regression separately triggers autosave while a view is active.
  expect(saves).toEqual([]);
  // #835's server tombstone wins even while /api/panes still has a stale DTO.
  workspace.removedPaneIds = ['tree-w2'];
  workspace.paneIds = workspace.paneIds.filter(id => id !== 'tree-w2');
  workspace.panes = workspace.panes?.filter(p => p.id !== 'tree-w2');
  workspace.layout = removePane(workspace.layout, 'tree-w2');
  await expect(row(page, 'Finished worker')).toHaveCount(0, { timeout: 10_000 });
  await page.reload();
  await expect(row(page, 'Release lead')).toBeVisible();
  await expect(row(page, 'Finished worker')).toHaveCount(0);
});

test('mobile keeps the hierarchy and capped indentation', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setup(page);
  await page.getByRole('button', { name: 'Open workspaces', exact: true }).click();
  await expect(row(page, 'Approval sub-worker')).toBeVisible();
  await page.screenshot({ animations: 'disabled', path: testInfo.outputPath('mobile-tree.png') });
  await row(page, 'Approval sub-worker').locator(':scope > .workspace-pane-row').click();
  await visible(page, ['tree-sub']);
  await page.getByRole('button', { name: 'Open workspaces', exact: true }).click();
  await row(page, 'Approval sub-worker').focus();
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: 'Open workspaces', exact: true }).click();
  await expect(row(page, 'Release lead')).toBeVisible();
});


test('a recovered Focus worker receives live events outside the saved layout', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  let connected = false;
  await page.route('**/api/panes/tree-w3/native/history*', route => route.fulfill({ json: { since: 0, events: [], truncated: false } }));
  await page.route('**/api/panes/tree-w3/native/allowlist', route => route.fulfill({ json: { tools: [] } }));
  await page.routeWebSocket(/\/api\/panes\/tree-w3\/native\/ws/, ws => {
    connected = true;
    ws.send(JSON.stringify({ seq: 1, type: 'text', text: 'Native worker live updates', messageId: 'message-1' }));
  });
  await setup(page, true);
  await row(page, 'Missing layout worker').locator(':scope > .workspace-pane-row').click();
  await expect.poll(() => connected).toBe(true);
  await expect(page.locator('[data-pane-id="tree-w3"]').getByText('Native worker live updates', { exact: true })).toBeVisible();
});
