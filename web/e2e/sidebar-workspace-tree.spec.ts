import { test, expect } from './fixtures';
import type { Page } from 'playwright/test';
import type { Layout, PaneInfo, Workspace } from '../src/types';
import { buildNxMFromLeaves, removePane } from '../src/lib/layout';

const ids = ['tree-lead', 'tree-w1', 'tree-w2', 'tree-w3', 'tree-sub', 'tree-human'];
const labels = ['Release lead', 'Build worker', 'Finished worker', 'Missing layout worker', 'Approval sub-worker', 'My shell'];
// Workers start folded under a top-level pane. Most tests here are about views,
// not folding, so they start with the lead open, as if the person had opened
// it (only when nothing is stored yet, so reloads keep later choices).
async function openTree(page: Page, keys: string[]) {
  await page.addInitScript(keys => {
    if (localStorage.getItem('solu.sidebarTreeCollapsed.v1') === null) localStorage.setItem('solu.sidebarTreeCollapsed.v1', JSON.stringify(Object.fromEntries(keys.map(key => [key, false]))));
  }, keys);
}
async function setup(page: Page, nativeWorker = false, options: { secondWorkspace?: boolean; agentOnly?: boolean; orphan?: boolean; auto?: boolean; folded?: boolean } = {}) {
  if (!options.folded) await openTree(page, ['Tree project:pane:tree-lead']);
  const panes = ids.map((id, i) => ({
    id, title: labels[i], cwd: '/project', cmd: ['sh'], status: 'running', state: 'idle', pid: i + 10, cols: 80, rows: 24,
    createdAt: '2026-10-30T00:00:00Z', lastActivityMs: 0,
    ...(nativeWorker && i === 3 ? { kind: 'native', agent: 'claude', hasClaude: true } : {}),
    ...(i > 0 && i < 5 ? { origin: { source: 'agent', byPaneId: i === 4 ? ids[1] : ids[0], leadPaneId: ids[0], workspace: 'Tree project', at: i } } : {}),
    ...(i === 4 ? { state: 'choice-prompt', attention: 'needs-user' } : {}),
    ...(i === 2 ? { turn: { active: false, count: 1, last: { n: 1, startedMs: 1, endedMs: Date.now(), outcome: 'completed', files: 0, added: 0, removed: 0 } } } : {}),
  } as PaneInfo));
  if (options.agentOnly) {
    panes[0].origin = { source: 'agent', byPaneId: 'closed', at: 1 };
    panes[5].origin = { source: 'agent', byPaneId: 'closed', at: 2 };
  }
  if (options.orphan) panes.push({ ...panes[3], id: 'tree-orphan', title: 'Unattached worker', origin: { source: 'agent', byPaneId: 'closed', at: 3 } });
  const savedIds = ids.filter(id => id !== 'tree-w3');
  const layout = buildNxMFromLeaves(savedIds.map(paneId => ({ type: 'pane', paneId } as Layout)), 2, 3);
  const workspace = { name: 'Tree project', layoutMode: options.auto ? 'auto' : 'free', ...(options.auto ? { layoutColumns: 4, layoutAspect: 1.6 } : {}), paneIds: savedIds, panes: panes.filter(p => savedIds.includes(p.id)), layout, activeAt: '2026-10-30T00:00:00Z' } as Workspace;
  const otherPane = { ...panes[5], id: 'other-human', title: 'Other shell' };
  if (options.secondWorkspace) panes.push(otherPane);
  const other = { ...workspace, name: 'Other project', paneIds: [otherPane.id], panes: [otherPane], layout: { type: 'pane', paneId: otherPane.id } } as Workspace;
  const saves: Workspace[] = [];
  const terminalInputs: Array<{ paneId: string; data: string }> = [];
  await page.route('**/api/panes', route => route.fulfill({ json: panes }));
  await page.route('**/api/workspaces**', route => {
    if (route.request().method() === 'POST') saves.push(route.request().postDataJSON());
    return route.fulfill({ json: route.request().method() === 'POST' ? route.request().postDataJSON() : options.secondWorkspace ? [workspace, other] : [workspace] });
  });
  await page.routeWebSocket(/\/api\/panes\/.*\/io/, ws => {
    ws.onMessage(data => terminalInputs.push({ paneId: new URL(ws.url()).pathname.split('/').at(-2)!, data: typeof data === 'string' ? data : data.toString() }));
    ws.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
    ws.send(Buffer.from('\r\nWorkspace tree acceptance fixture\r\n'));
  });
  await page.goto('/?ws=Tree%20project');
  return { saves, layout, workspace, terminalInputs, panes };
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
  const full = ids;
  const topLevel = ['tree-lead', 'tree-human'];
  await visible(page, topLevel);
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
  await visible(page, topLevel);
  expect(await page.evaluate(() => {
    const w = window as unknown as { __soluTerms: Map<string, unknown>; __treeOriginalHuman?: unknown };
    return [...w.__soluTerms.entries()].find(([key]) => JSON.parse(key)[1] === 'tree-human')?.[1] === w.__treeOriginalHuman;
  })).toBe(true);
  await row(page, 'Missing layout worker').locator(':scope > .workspace-pane-row').click();
  await visible(page, ['tree-w3']);
  await page.locator('.ws-row .ws-name').click();
  await visible(page, topLevel);
  await page.getByRole('button', { name: 'All panes in Tree project' }).click();
  await visible(page, full);
  await expect(row(page, 'Finished worker')).toBeVisible();
  await page.screenshot({ animations: 'disabled', path: testInfo.outputPath('all-panes.png') });
  await row(page, 'Release lead').focus();
  await page.keyboard.press('Escape');
  await visible(page, full);
  // Collapse preserves the attention dot and descendant count; keyboard moves
  // through visible rows and expands the same branch.
  await page.getByRole('button', { name: 'Collapse Release lead', exact: true }).click();
  await expect(row(page, 'Release lead')).toContainText('4 workers');
  // The lead itself is idle: a ring and a count say what below it needs you.
  await expect(row(page, 'Release lead').locator('.pane-tree-dot')).toHaveClass(/is-needs-you-below/);
  await expect(row(page, 'Release lead').locator('.pane-tree-attn-count')).toHaveText('1●');
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

test('chevrons take near misses, × takes no width, and branches open and close together', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  await setup(page, false, { secondWorkspace: true });
  const topLevel = ['tree-lead', 'tree-human'];
  await visible(page, topLevel);
  // The chevron is a full-height column: a click 2px above the row's bottom
  // edge, well outside the glyph, folds the branch and leaves the view alone.
  const chevron = page.getByRole('button', { name: 'Collapse Release lead', exact: true });
  const box = (await chevron.boundingBox())!;
  const rowBox = (await row(page, 'Release lead').locator(':scope > .workspace-pane-row').boundingBox())!;
  expect(box.width).toBe(24);
  expect(box.height).toBeGreaterThanOrEqual(rowBox.height - 1);
  await page.mouse.click(box.x + box.width / 2, rowBox.y + rowBox.height - 2);
  await expect(row(page, 'Release lead')).toHaveAttribute('aria-expanded', 'false');
  await visible(page, topLevel);
  await page.getByRole('button', { name: 'Expand Release lead', exact: true }).click();
  // The × floats over the row's end on hover instead of taking width.
  const buildRow = row(page, 'Build worker').locator(':scope > .workspace-pane-row');
  const text = buildRow.locator(':scope > .pane-tree-text');
  await page.mouse.move(900, 500);
  const before = (await text.boundingBox())!.width;
  await buildRow.hover();
  await expect(buildRow.locator('.pane-tree-close')).toHaveCSS('opacity', '1');
  expect((await text.boundingBox())!.width).toBe(before);
  expect(before).toBeGreaterThan(rowBox.width - 100);
  // Alt+click folds the lead with everything below it; a plain reopen keeps
  // the inner branch folded.
  await page.getByRole('button', { name: 'Collapse Release lead', exact: true }).click({ modifiers: ['Alt'] });
  await page.getByRole('button', { name: 'Expand Release lead', exact: true }).click();
  await expect(row(page, 'Build worker')).toHaveAttribute('aria-expanded', 'false');
  await expect(row(page, 'Approval sub-worker')).toHaveCount(0);
  await expect(row(page, 'Build worker').locator('.pane-tree-attn-count')).toHaveText('1●');
  await page.getByRole('button', { name: 'Collapse Release lead', exact: true }).click();
  await page.getByRole('button', { name: 'Expand Release lead', exact: true }).click({ modifiers: ['Alt'] });
  await expect(row(page, 'Approval sub-worker')).toBeVisible();
  // Collapse all / Expand all from the header, remembered across a reload.
  await page.getByRole('button', { name: 'Collapse all', exact: true }).click();
  for (const name of ['Tree project', 'Other project']) await expect(row(page, name)).toHaveAttribute('aria-expanded', 'false');
  await expect(row(page, 'Release lead')).toHaveCount(0);
  await page.screenshot({ animations: 'disabled', path: testInfo.outputPath('collapse-all.png') });
  await page.reload();
  await expect(row(page, 'Tree project')).toHaveAttribute('aria-expanded', 'false');
  await page.getByRole('button', { name: 'Expand all', exact: true }).click();
  await expect(row(page, 'Approval sub-worker')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Collapse all', exact: true })).toBeVisible();
  // * and - open and close a row with its siblings.
  await row(page, 'Tree project').focus();
  await page.keyboard.press('-');
  await expect(row(page, 'Other project')).toHaveAttribute('aria-expanded', 'false');
  await page.keyboard.press('*');
  await expect(row(page, 'Other project')).toHaveAttribute('aria-expanded', 'true');
  await expect(row(page, 'Release lead')).toBeVisible();
  // A workspace is deleted from its menu, not a × on every row.
  await expect(page.getByRole('button', { name: 'Delete workspace' }).first()).toBeHidden();
  await page.locator('.ws-row').first().click({ button: 'right' });
  await expect(page.getByRole('menuitem', { name: 'Delete' })).toBeVisible();
  await page.keyboard.press('Escape');
  await page.screenshot({ animations: 'disabled', path: testInfo.outputPath('tree-targets.png') });
});

test('workers start folded under their lead, still raise its flag, and an opened lead stays open', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  await setup(page, false, { folded: true });
  const lead = row(page, 'Release lead');
  await expect(lead).toHaveAttribute('aria-expanded', 'false');
  await expect(row(page, 'Build worker')).toHaveCount(0);
  // The approval two levels down shows on the folded lead: a ring and 1●.
  await expect(lead.locator(':scope > .workspace-pane-row .pane-tree-dot')).toHaveClass(/is-needs-you-below/);
  await expect(lead.locator(':scope > .workspace-pane-row .pane-tree-attn-count')).toHaveText('1●');
  await expect(page.getByRole('button', { name: 'Expand all', exact: true })).toBeVisible();
  await page.screenshot({ animations: 'disabled', path: testInfo.outputPath('folded-by-default.png') });
  await lead.getByRole('button', { name: 'Expand Release lead', exact: true }).click();
  await expect(row(page, 'Approval sub-worker')).toBeVisible();
  await page.reload();
  await expect(row(page, 'Release lead')).toHaveAttribute('aria-expanded', 'true');
  await expect(row(page, 'Approval sub-worker')).toBeVisible();
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


test('pane refresh order preserves team grid order and mounted terminals', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  const { panes } = await setup(page);
  await row(page, 'Release lead').locator(':scope > .workspace-pane-row').click();
  await visible(page, ids.filter(id => id !== 'tree-human'));
  const capture = () => page.locator('[data-pane-id] .xterm:visible').evaluateAll(elements =>
    elements.map(element => element.closest('[data-pane-id]')!.getAttribute('data-pane-id')));
  const order = await capture();
  await page.locator('[data-pane-id] .xterm:visible').evaluateAll(elements => {
    (window as unknown as { __orderTestTerms: Element[] }).__orderTestTerms = elements;
  });
  for (const changed of [false, true]) {
    panes.reverse();
    if (changed) panes[0].lastActivityMs = 12345;
    await page.waitForResponse(response => new URL(response.url()).pathname === '/api/panes' && response.request().method() === 'GET');
    await expect.poll(capture).toEqual(order);
    await expect.poll(() => page.locator('[data-pane-id] .xterm:visible').evaluateAll(elements => {
      const previous = (window as unknown as { __orderTestTerms: Element[] }).__orderTestTerms;
      return elements.length === previous.length && elements.every((element, i) => element === previous[i]);
    })).toBe(true);
  }
});


test('workspace overview, persistent All panes, nested teams and selection stay distinct', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  await setup(page, false, { secondWorkspace: true, orphan: true });
  const workspace = row(page, 'Tree project');
  const lead = row(page, 'Release lead');
  const toggle = page.getByRole('button', { name: 'All panes in Tree project' });
  await visible(page, ['tree-lead', 'tree-human']);
  await expect(workspace).toHaveAttribute('aria-selected', 'true');
  await expect(row(page, 'My shell')).not.toContainText('lead');
  await expect(row(page, 'My shell')).not.toContainText('0 workers');
  // A worker with no surviving starter and no recorded workspace has one
  // place: Detached sessions, never some other workspace's tree.
  const detached = page.locator('.bg-session-row', { hasText: 'Unattached worker' });
  if (!await detached.isVisible()) await page.getByText('Detached sessions').first().click();
  await expect(detached).toBeVisible();
  await expect(row(page, 'Unattached worker')).toHaveCount(0);
  await lead.locator(':scope > .workspace-pane-row').click();
  await expect(lead).toHaveAttribute('aria-selected', 'true');
  await expect(lead).toHaveAttribute('aria-current', 'true');
  await expect(workspace).toHaveAttribute('aria-selected', 'false');
  await expect(workspace.locator(':scope > .ws-row')).toHaveClass(/contains-selection/);
  await page.locator('[data-pane-id="tree-w1"] .xterm-helper-textarea').focus();
  await expect(lead).toHaveAttribute('aria-selected', 'true');
  await expect(row(page, 'Build worker')).toHaveAttribute('aria-selected', 'false');
  await row(page, 'Build worker').locator(':scope > .workspace-pane-row').click();
  await visible(page, ['tree-w1', 'tree-sub']);
  await expect(row(page, 'Build worker')).toHaveAttribute('aria-selected', 'true');
  await workspace.locator(':scope > .ws-row .ws-name').click();
  await visible(page, ['tree-lead', 'tree-human']);
  await expect(workspace).toHaveAttribute('aria-selected', 'true');
  await expect(lead).toHaveAttribute('aria-selected', 'false');
  await toggle.click();
  await visible(page, ids);
  await expect(toggle).toHaveAttribute('aria-pressed', 'true');
  await row(page, 'Other project').locator(':scope > .ws-row .ws-name').click();
  await expect(page.locator('[data-pane-id="other-human"] .xterm')).toBeVisible();
  await expect(page.getByRole('button', { name: 'All panes in Other project' })).toHaveAttribute('aria-pressed', 'false');
  await workspace.locator(':scope > .ws-row .ws-name').click();
  await visible(page, ids);
  await expect(toggle).toHaveAttribute('aria-pressed', 'true');
  await expect(row(page, 'Unattached worker')).toHaveCount(0);
  await page.reload();
  await visible(page, ids);
  await expect(toggle).toHaveAttribute('aria-pressed', 'true');
  await toggle.click();
  await visible(page, ['tree-lead', 'tree-human']);
});

test('an agent-only workspace falls back to every pane', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  await setup(page, false, { agentOnly: true });
  await row(page, 'Tree project').locator(':scope > .ws-row .ws-name').click();
  await visible(page, ids);
  await expect(page.getByRole('group', { name: 'Unattached', exact: true })).toBeVisible();
});


test('overview splitter edits persist canonical ratios without dropping hidden workers', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  const { saves } = await setup(page);
  await visible(page, ['tree-lead', 'tree-human']);
  await expect(page.locator('[data-pane-id="tree-lead"] .pane-header')).toHaveAttribute('draggable', 'true');
  const handle = page.locator('.workspace-pane-grid [data-panel-resize-handle-id]:visible').first();
  const bounds = (await handle.boundingBox())!;
  const x = bounds.x + bounds.width / 2, y = bounds.y + bounds.height / 2;
  await page.mouse.move(x, y);
  await page.mouse.down();
  await page.mouse.move(x + (bounds.height > bounds.width ? 80 : 0), y + (bounds.width > bounds.height ? 80 : 0), { steps: 8 });
  await page.mouse.up();
  await expect.poll(() => saves.length).toBeGreaterThan(0);
  expect(saves.at(-1)!.paneIds).toEqual(expect.arrayContaining(ids.filter(id => id !== 'tree-w3')));
  await visible(page, ['tree-lead', 'tree-human']);
});


test('Escape closes the mobile drawer from a workspace overview', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setup(page);
  await page.getByRole('button', { name: 'Open workspaces', exact: true }).click();
  await row(page, 'Tree project').focus();
  await page.keyboard.press('Escape');
  await expect(row(page, 'Tree project')).not.toBeVisible();
});


test('dropping on an appended tile never removes or saves away the source pane', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  const { saves } = await setup(page);
  await page.getByRole('button', { name: 'All panes in Tree project' }).click();
  await visible(page, ids);
  const appended = page.locator('[data-pane-id="tree-w3"]');
  for (const edge of ['left', 'right', 'top', 'bottom']) {
    await appended.evaluate((element, edge) => {
      const rect = element.getBoundingClientRect();
      const x = edge === 'left' ? rect.left + 1 : edge === 'right' ? rect.right - 1 : rect.left + rect.width / 2;
      const y = edge === 'top' ? rect.top + 1 : edge === 'bottom' ? rect.bottom - 1 : rect.top + rect.height / 2;
      const dataTransfer = new DataTransfer();
      dataTransfer.setData('application/x-solu-pane', 'tree-human');
      element.dispatchEvent(new DragEvent('dragover', { bubbles: true, cancelable: true, dataTransfer, clientX: x, clientY: y }));
      element.dispatchEvent(new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer, clientX: x, clientY: y }));
    }, edge);
  }
  await visible(page, ids);
  await page.waitForTimeout(600); // cross the autosave debounce
  expect(saves).toEqual([]);
  await page.reload();
  await visible(page, ids);
});

test('overview re-tile and viewport changes balance visible panes and keep hidden workers saved', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  const { saves } = await setup(page);
  await visible(page, ['tree-lead', 'tree-human']);
  await page.getByRole('button', { name: 'Re-tile panes', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Re-tile panes', exact: true })).toHaveCount(0);
  await page.setViewportSize({ width: 1200, height: 900 });
  await visible(page, ['tree-lead', 'tree-human']);
  const a = (await page.locator('[data-pane-id="tree-lead"]').boundingBox())!;
  const b = (await page.locator('[data-pane-id="tree-human"]').boundingBox())!;
  expect(Math.max(a.width * a.height, b.width * b.height) / Math.min(a.width * a.height, b.width * b.height)).toBeLessThan(1.1);
  // Re-tile turns auto layout on for good; hidden workers stay in the save.
  await expect.poll(() => saves.length).toBeGreaterThan(0);
  expect(saves.at(-1)).toMatchObject({ layoutMode: 'auto' });
  expect(saves.at(-1)!.paneIds).toEqual(expect.arrayContaining(ids.filter(id => id !== 'tree-w3')));
});


// Records the panes painted on every animation frame, so a test can prove a
// view change never showed an intermediate arrangement.
async function recordFrames(page: Page) {
  await page.evaluate(() => {
    const w = window as unknown as { __frames: string[][] };
    w.__frames = [];
    const tick = () => {
      w.__frames.push([...document.querySelectorAll<HTMLElement>('[data-pane-id]')]
        .filter(e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; })
        .map(e => e.dataset.paneId!));
      requestAnimationFrame(tick);
    };
    tick();
  });
  return () => page.evaluate(() => (window as unknown as { __frames: string[][] }).__frames);
}
const focusedPane = (page: Page) => page.locator('.workspace-pane-grid .pane.focused').getAttribute('data-pane-id');

test('an attention jump to a hidden worker opens its team and focuses it', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  await setup(page);
  await visible(page, ['tree-lead', 'tree-human']);
  await page.getByRole('button', { name: '1 item is waiting on you' }).click();
  await page.locator('.attention-row', { hasText: 'Approval sub-worker' }).click();
  // The worker's top-level ancestor's team: never the lead alone.
  await visible(page, ['tree-lead', 'tree-w1', 'tree-w2', 'tree-w3', 'tree-sub']);
  await expect.poll(() => focusedPane(page)).toBe('tree-sub');
  await expect(row(page, 'Release lead')).toHaveAttribute('aria-selected', 'true');
  await expect(row(page, 'Tree project').locator(':scope > .ws-row')).toHaveClass(/contains-selection/);
  // Escape from the tree returns to the overview.
  await row(page, 'Release lead').focus();
  await page.keyboard.press('Escape');
  await visible(page, ['tree-lead', 'tree-human']);
});

test('switching workspaces never paints hidden workers or an in-between view', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  await setup(page, false, { secondWorkspace: true });
  await visible(page, ['tree-lead', 'tree-human']);
  await row(page, 'Other project').locator(':scope > .ws-row .ws-name').click();
  await expect(page.locator('[data-pane-id="other-human"] .xterm')).toBeVisible();
  let frames = await recordFrames(page);
  await row(page, 'Tree project').locator(':scope > .ws-row .ws-name').click();
  await visible(page, ['tree-lead', 'tree-human']);
  const workers = ['tree-w1', 'tree-w2', 'tree-w3', 'tree-sub'];
  expect((await frames()).filter(f => f.some(id => workers.includes(id)))).toEqual([]);
  // A pane row in another workspace opens straight into that pane's team.
  await row(page, 'Other project').locator(':scope > .ws-row .ws-name').click();
  await expect(page.locator('[data-pane-id="other-human"] .xterm')).toBeVisible();
  frames = await recordFrames(page);
  await row(page, 'Build worker').locator(':scope > .workspace-pane-row').click();
  await visible(page, ['tree-w1', 'tree-sub']);
  expect((await frames()).filter(f => f.includes('tree-lead') || f.includes('tree-human'))).toEqual([]);
});

test('an auto layout overview stays resizable and saving it keeps hidden workers', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  const { saves } = await setup(page, false, { auto: true });
  await visible(page, ['tree-lead', 'tree-human']);
  const lead = page.locator('[data-pane-id="tree-lead"]');
  const before = (await lead.boundingBox())!;
  const handle = page.locator('.workspace-pane-grid [data-panel-resize-handle-id]:visible').first();
  await expect(handle).toHaveAttribute('data-panel-resize-handle-enabled', 'true');
  const bounds = (await handle.boundingBox())!;
  const x = bounds.x + bounds.width / 2, y = bounds.y + bounds.height / 2;
  await page.mouse.move(x, y);
  await page.mouse.down();
  // Starting the drag saves the shown arrangement; no tile moves yet.
  const pressed = (await lead.boundingBox())!;
  expect(Math.abs(pressed.width - before.width)).toBeLessThan(2);
  await page.mouse.move(x + 80, y, { steps: 8 });
  await page.mouse.up();
  await expect.poll(async () => (await lead.boundingBox())!.width).toBeGreaterThan(before.width + 40);
  await expect.poll(() => saves.length).toBeGreaterThan(0);
  expect(saves.at(-1)).toMatchObject({ layoutMode: 'free' });
  expect(saves.at(-1)!.paneIds).toEqual(expect.arrayContaining(ids.filter(id => id !== 'tree-w3')));
  await visible(page, ['tree-lead', 'tree-human']);
  // All panes shows each hidden worker beside its lead.
  await page.getByRole('button', { name: 'All panes in Tree project' }).click();
  await visible(page, ids);
});

test('an archived worker leaves its team view calmly', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  const { panes, workspace } = await setup(page);
  await row(page, 'Release lead').locator(':scope > .workspace-pane-row').click();
  await visible(page, ['tree-lead', 'tree-w1', 'tree-w2', 'tree-w3', 'tree-sub']);
  await page.locator('[data-pane-id="tree-w2"] .xterm').click();
  await expect.poll(() => focusedPane(page)).toBe('tree-w2');
  const frames = await recordFrames(page);
  // The daemon archives a finished worker: it leaves /api/panes and the workspace.
  panes.splice(panes.findIndex(p => p.id === 'tree-w2'), 1);
  workspace.paneIds = workspace.paneIds!.filter(id => id !== 'tree-w2');
  workspace.panes = workspace.panes?.filter(p => p.id !== 'tree-w2');
  workspace.layout = removePane(workspace.layout, 'tree-w2');
  await visible(page, ['tree-lead', 'tree-w1', 'tree-w3', 'tree-sub']);
  await expect(row(page, 'Release lead')).toHaveAttribute('aria-selected', 'true');
  expect(await focusedPane(page)).not.toBeNull();
  expect((await frames()).filter(f => f.includes('tree-human'))).toEqual([]);
});

test('Alt+digit and Alt+Z stay within the panes the overview shows', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  await setup(page);
  await visible(page, ['tree-lead', 'tree-human']);
  await expect(page.locator('[data-pane-id="tree-human"] .pane-header')).toContainText('#2');
  await page.locator('[data-pane-id="tree-lead"] .xterm').click();
  await page.keyboard.press('Alt+2');
  await expect.poll(() => focusedPane(page)).toBe('tree-human');
  await visible(page, ['tree-lead', 'tree-human']);
  await page.keyboard.press('Alt+z');
  await visible(page, ['tree-human']);
  await page.keyboard.press('Alt+z');
  await visible(page, ['tree-lead', 'tree-human']);
  await page.keyboard.press('Alt+]');
  await expect.poll(() => focusedPane(page)).toBe('tree-lead');
  await visible(page, ['tree-lead', 'tree-human']);
});

test('mobile lists only top-level panes and an attention jump shows the worker', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setup(page);
  await page.locator('.bottombar-picker').click();
  const options = page.locator('.bottombar-picker-menu').getByRole('option');
  await expect(options).toHaveText([/Release lead/, /My shell/]);
  await page.locator('.bottombar-picker').click();
  // One waiting pane: the bottom bar goes straight to it.
  await page.getByRole('button', { name: '1 waiting on you — go to it' }).click();
  await visible(page, ['tree-sub']);
  await page.locator('.bottombar-picker').click();
  await expect(options).toHaveCount(5);
});

test('leads are peers: a sub-lead is top level with its workers folded under it; Make lead from the pane header, the tree row and in a batch', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  const at = (n: number) => ({ at: n, workspace: 'Leads project' });
  const base = { cwd: '/project', cmd: ['sh'], status: 'running', state: 'idle', cols: 80, rows: 24, createdAt: '2026-10-30T00:00:00Z', lastActivityMs: 0 };
  const panes = [
    { ...base, id: 'k-cmo', title: 'CMO', pid: 1, origin: { source: 'user', ...at(1) } },
    { ...base, id: 'k-social', title: 'Social', pid: 2, origin: { source: 'agent', byPaneId: 'k-cmo', leadPaneId: 'k-cmo', teamKind: 'lead', leadRole: 'social', ...at(2) } },
    { ...base, id: 'k-post', title: 'Draft post', pid: 3, origin: { source: 'agent', byPaneId: 'k-social', leadPaneId: 'k-cmo', teamKind: 'worker', ...at(3) } },
    { ...base, id: 'k-qa', title: 'QA', pid: 4, origin: { source: 'agent', byPaneId: 'closed', leadPaneId: 'closed', teamKind: 'lead', leadRole: 'qa', ...at(4) } },
    { ...base, id: 'k-shell', title: 'My shell', pid: 5, origin: { source: 'user', ...at(5) } },
  ] as PaneInfo[];
  const paneIds = panes.map(p => p.id);
  const layout = buildNxMFromLeaves(paneIds.map(paneId => ({ type: 'pane', paneId } as Layout)), 2, 3);
  const workspace = { name: 'Leads project', layoutMode: 'free', paneIds, panes, layout, activeAt: '2026-10-30T00:00:00Z' } as Workspace;
  const teamCalls: unknown[] = [];
  await page.route('**/api/panes', route => route.fulfill({ json: panes }));
  await page.route('**/api/workspaces**', route => route.fulfill({ json: route.request().method() === 'POST' ? route.request().postDataJSON() : [workspace] }));
  await page.route('**/api/panes/*/team', route => {
    const body = route.request().postDataJSON();
    teamCalls.push({ id: new URL(route.request().url()).pathname.split('/').at(-2), ...body });
    const id = new URL(route.request().url()).pathname.split('/').at(-2);
    const index = panes.findIndex(p => p.id === id);
    const origin = { ...panes[index].origin, teamKind: 'lead', leadRole: body.role };
    panes[index] = { ...panes[index], origin } as PaneInfo;
    return route.fulfill({ json: { origin } });
  });
  await page.routeWebSocket(/\/api\/panes\/.*\/io/, ws => {
    ws.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
    ws.send(Buffer.from('\r\nLeads fixture\r\n'));
  });
  await page.goto('/?ws=Leads%20project');
  await expect(row(page, 'CMO')).toBeVisible();
  const shown = (expected: string[]) => expect.poll(async () => {
    const out = [];
    for (const id of paneIds) if (await page.locator(`[data-pane-id="${id}"] .xterm`).isVisible()) out.push(id);
    return out;
  }).toEqual(paneIds.filter(id => expected.includes(id)));
  // Every lead is top level, a peer, even one the CMO started (a sub-lead);
  // who started it stays as provenance. Its worker nests under it, folded.
  await shown(['k-cmo', 'k-social', 'k-qa', 'k-shell']);
  for (const name of ['CMO', 'Social', 'QA', 'My shell']) await expect(row(page, name)).toHaveAttribute('aria-level', '2');
  await expect(row(page, 'Social')).toHaveAttribute('aria-expanded', 'false');
  await expect(row(page, 'Draft post')).toHaveCount(0);
  await expect(row(page, 'Social').locator(':scope > .workspace-pane-row')).toHaveAttribute('title', 'Social\nStarted by CMO');
  await expect(row(page, 'Social')).toHaveAccessibleDescription('idle · 1 worker · started by CMO');
  await row(page, 'Social').getByRole('button', { name: 'Expand Social', exact: true }).click();
  await expect(row(page, 'Draft post')).toHaveAttribute('aria-level', '3');
  await expect(row(page, 'Social').locator(':scope > .workspace-pane-row .pane-tree-role-chip')).toHaveText('social');
  await expect(row(page, 'QA').locator(':scope > .workspace-pane-row .pane-tree-role-chip')).toHaveText('qa');
  await expect(row(page, 'CMO').locator(':scope > .workspace-pane-row .pane-tree-role-chip')).toHaveCount(0);
  await expect(row(page, 'CMO')).not.toHaveAttribute('aria-expanded');
  // A lead with no running workers is still a lead, and its header says so.
  await expect(page.locator('[data-pane-id="k-qa"] .pane-header')).toContainText('Lead · qa');
  // A sub-lead shows its own team; its starter's team no longer includes it.
  await row(page, 'Social').locator(':scope > .workspace-pane-row').click();
  await shown(['k-social', 'k-post']);
  await row(page, 'CMO').locator(':scope > .workspace-pane-row').click();
  await shown(['k-cmo']);
  await page.locator('.ws-row .ws-name').click();
  await shown(['k-cmo', 'k-social', 'k-qa', 'k-shell']);
  // Make lead on your own pane asks for the role and shows the pill.
  await page.locator('[data-pane-id="k-shell"] .pane-header').click({ button: 'right' });
  await expect(page.getByRole('menuitem', { name: 'Make worker' })).toHaveCount(0);
  await page.getByRole('menuitem', { name: 'Make lead…' }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('textbox').fill('@security');
  await dialog.getByRole('button', { name: 'Make lead' }).click();
  await expect(row(page, 'My shell').locator(':scope > .workspace-pane-row .pane-tree-role-chip')).toHaveText('security');
  expect(teamCalls).toEqual([{ id: 'k-shell', kind: 'lead', role: 'security' }]);
  // Still top level, nothing else moved.
  await shown(['k-cmo', 'k-social', 'k-qa', 'k-shell']);
  // The same from the tree row's own menu, without opening the pane…
  await row(page, 'My shell').locator(':scope > .workspace-pane-row').click({ button: 'right' });
  await expect(page.getByRole('menuitem', { name: 'Rename lead role…' })).toBeVisible();
  await expect(page.getByRole('menuitem', { name: 'Remove lead' })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('menu')).toHaveCount(0);
  // …and for several panes at once from the workspace menu.
  await page.locator('.ws-row').click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Make leads…' }).click();
  const batch = page.getByRole('dialog', { name: 'Make leads in Leads project' });
  await expect(batch.getByRole('checkbox')).toHaveCount(1);
  await batch.getByRole('checkbox', { name: 'CMO' }).check();
  await expect(batch.getByRole('button', { name: 'Make 1 lead' })).toBeDisabled();
  await batch.getByRole('textbox', { name: 'Role for CMO' }).fill('marketing');
  await batch.getByRole('button', { name: 'Make 1 lead' }).click();
  await expect(batch).toHaveCount(0);
  expect(teamCalls.at(-1)).toEqual({ id: 'k-cmo', kind: 'lead', role: 'marketing' });
  await expect(row(page, 'CMO').locator(':scope > .workspace-pane-row .pane-tree-role-chip')).toHaveText('marketing');
});

test('pane rows lead with the @handle, then what the pane is doing, on two lines or one by width', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  const base = { cwd: '/project', cmd: ['sh'], status: 'running', state: 'idle', cols: 80, rows: 24, createdAt: '2026-10-30T00:00:00Z', lastActivityMs: 0 };
  const panes = [
    { ...base, id: 'h-cmo', title: '@cmo · launch', pid: 1, origin: { source: 'user', at: 1 }, identity: { handle: 'cmo', workingOn: 'Drafting the launch post' } },
    { ...base, id: 'h-sec', title: 'Security review', pid: 2, origin: { source: 'user', at: 2 }, identity: { handle: 'secure' } },
    { ...base, id: 'h-only', title: '@qa', pid: 3, origin: { source: 'user', at: 3 }, identity: { handle: 'qa' } },
    { ...base, id: 'h-plain', title: 'My shell', pid: 4, origin: { source: 'user', at: 4 } },
  ] as PaneInfo[];
  const paneIds = panes.map(p => p.id);
  const layout = buildNxMFromLeaves(paneIds.map(paneId => ({ type: 'pane', paneId } as Layout)), 2, 2);
  const workspace = { name: 'Handles', layoutMode: 'free', paneIds, panes, layout, activeAt: '2026-10-30T00:00:00Z' } as Workspace;
  await page.route('**/api/panes', route => route.fulfill({ json: panes }));
  await page.route('**/api/workspaces**', route => route.fulfill({ json: route.request().method() === 'POST' ? route.request().postDataJSON() : [workspace] }));
  await page.routeWebSocket(/\/api\/panes\/.*\/io/, ws => { ws.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' })); });
  await page.goto('/?ws=Handles');
  const line = (name: string) => row(page, name).locator(':scope > .workspace-pane-row');
  // The working-on line wins over the rest of the title; both stay in the tooltip.
  await expect(line('@cmo · launch').locator('.pane-tree-handle')).toHaveText('@cmo');
  await expect(line('@cmo · launch').locator('.pane-tree-desc')).toHaveText('Drafting the launch post');
  await expect(line('@cmo · launch')).toHaveAttribute('title', '@cmo · launch\nDrafting the launch post');
  await expect(line('@secure · Security review').locator('.pane-tree-title')).toHaveText('@secure');
  await expect(line('@secure · Security review').locator('.pane-tree-desc')).toHaveText('Security review');
  await expect(line('@qa').locator('.pane-tree-handle')).toHaveText('@qa');
  await expect(line('@qa').locator('.pane-tree-desc')).toHaveCount(0);
  await expect(line('My shell').locator('.pane-tree-title')).toHaveText('My shell');
  await expect(line('My shell').locator('.pane-tree-handle')).toHaveCount(0);
  // The handle reads dimmer-free; the description is dim.
  const colors = await line('@cmo · launch').evaluate(el => [el.querySelector('.pane-tree-handle'), el.querySelector('.pane-tree-desc')].map(n => getComputedStyle(n!).color));
  expect(colors[0]).not.toBe(colors[1]);
  // The status words are for screen readers; the dot shows the state.
  await expect(row(page, 'My shell')).toHaveAccessibleDescription('idle');
  const placement = async () => line('@cmo · launch').evaluate(el => {
    const handle = el.querySelector('.pane-tree-handle')!.getBoundingClientRect(), desc = el.querySelector('.pane-tree-desc')!.getBoundingClientRect();
    return { below: desc.top >= handle.bottom - 1, height: Math.round(el.getBoundingClientRect().height) };
  });
  // The default 260px sidebar: the description gets its own line.
  expect(await placement()).toEqual({ below: true, height: 36 });
  // From about a 340px sidebar it follows the handle on one 26px line.
  const handleBox = (await page.locator('[data-panel-resize-handle-id]').first().boundingBox())!;
  await page.mouse.move(handleBox.x + handleBox.width / 2, handleBox.y + 200);
  await page.mouse.down();
  await page.mouse.move(handleBox.x + handleBox.width / 2 + 120, handleBox.y + 200, { steps: 8 });
  await page.mouse.up();
  await expect.poll(placement).toEqual({ below: false, height: 26 });
  await expect(row(page, 'My shell').locator(':scope > .workspace-pane-row')).toHaveCSS('min-height', '26px');
  // Without a description the title may use the whole line.
  expect(await line('My shell').locator('.pane-tree-title-line').evaluate(el => el.style.maxWidth || getComputedStyle(el).maxWidth)).toBe('none');
});

test('every pane row offers × on hover or focus; a finished worker keeps it shown', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 960 });
  await setup(page);
  const close = (name: string) => row(page, name).locator(':scope > .workspace-pane-row > .pane-tree-close');
  const opacity = (name: string) => close(name).evaluate(el => getComputedStyle(el).opacity);
  await expect(row(page, 'My shell')).toBeVisible();
  await page.mouse.move(0, 0);
  await expect.poll(() => opacity('My shell')).toBe('0');
  await expect.poll(() => opacity('Release lead')).toBe('0');
  await expect.poll(() => opacity('Finished worker')).toBe('1');
  await row(page, 'My shell').locator(':scope > .workspace-pane-row').hover();
  await expect.poll(() => opacity('My shell')).toBe('1');
  await page.mouse.move(0, 0);
  await row(page, 'Release lead').focus();
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('ArrowUp');
  await expect(row(page, 'Release lead')).toBeFocused();
  await expect.poll(() => opacity('Release lead')).toBe('1');
  // A running pane asks first; cancelling leaves it and its view alone.
  let killed = false;
  await page.route('**/api/panes/tree-human', route => { if (route.request().method() === 'DELETE') killed = true; return route.fulfill({ json: {} }); });
  await row(page, 'My shell').locator(':scope > .workspace-pane-row').hover();
  await close('My shell').click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toContainText('My shell');
  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toHaveCount(0);
  expect(killed).toBe(false);
  await expect(row(page, 'My shell')).toBeVisible();
});
