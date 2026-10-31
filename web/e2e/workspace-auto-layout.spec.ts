import { expect, test } from './fixtures';
import type { Layout, PaneInfo, Workspace } from '../src/types';
import type { Terminal } from '@xterm/xterm';
import { appendAutoPane, balancedLayout, removeCustomPane, splitLargestPane } from '../src/lib/autoLayout';
import { buildPresetLayout, collectPaneIds, removePane } from '../src/lib/layout';

test('auto layout retains sessions and balances eight panes after varied opens and closes', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  const makePane = (n: number): PaneInfo => ({ id: `layout_${n}`, title: `Session ${n}`, cwd: '/fixture/layout', cmd: ['bash'], status: 'running', pid: n, cols: 80, rows: 24, state: 'idle', createdAt: '2026-10-31T00:00:00Z', hasClaude: false, lastActivityMs: 0 });
  const panes = new Map(Array.from({ length: 8 }, (_, i) => { const p = makePane(i + 1); return [p.id, p] as const; }));
  let tree: Layout | null = null;
  for (const id of panes.keys()) tree = tree ? { type: 'split', direction: 'horizontal', ratio: .5, a: tree, b: { type: 'pane', paneId: id } } : { type: 'pane', paneId: id };
  let workspace: Workspace = { name: 'Auto layout', layout: buildPresetLayout(tree, 'left-big'), paneIds: [...panes.keys()], activeAt: '2026-10-31T00:00:00Z', layoutMode: 'free', layoutVersion: 0, layoutColumns: 4, layoutAspect: 1.6 };
  const connections: string[] = [], restarts: string[] = [];
  let creates = 0, saves = 0, next = 8;
  const definitions = () => [...panes.values()].map(p => ({ ...p, kind: undefined }));
  workspace.panes = definitions();
  await page.addInitScript(() => localStorage.setItem('solu.warmPanes', '1'));
  await page.route('**/api/panes', route => {
    if (route.request().method() === 'POST') {
      creates++;
      const pane = makePane(++next); panes.set(pane.id, pane);
      workspace = { ...workspace, layout: workspace.layoutMode === 'free'
        ? splitLargestPane(workspace.layout, pane.id, workspace.layoutAspect)
        : appendAutoPane(workspace.layout, pane.id, workspace.layoutColumns ?? 4, workspace.layoutAspect ?? 1.6),
        panes: definitions() };
      workspace.paneIds = collectPaneIds(workspace.layout);
      return route.fulfill({ status: 201, json: pane });
    }
    return route.fulfill({ json: [...panes.values()] });
  });
  await page.route('**/api/panes/*/cwd', route => route.fulfill({ json: { cwd: '/fixture/layout' } }));
  await page.route('**/api/panes/*/restart', route => { restarts.push(route.request().url()); return route.abort(); });
  await page.route('**/api/panes/*', route => {
    if (route.request().method() !== 'DELETE') return route.fallback();
    const id = new URL(route.request().url()).pathname.split('/').at(-1)!;
    panes.delete(id);
    workspace = { ...workspace, layoutVersion: (workspace.layoutVersion ?? 0) + 1, layout: workspace.layoutMode === 'free' ? removeCustomPane(workspace.layout, id) : balancedLayout(removePane(workspace.layout, id), workspace.layoutColumns, workspace.layoutAspect), panes: definitions() };
    workspace.paneIds = collectPaneIds(workspace.layout);
    return route.fulfill({ status: 204 });
  });
  await page.route('**/api/workspaces**', route => {
    if (route.request().method() === 'POST') {
      const candidate = route.request().postDataJSON() as Workspace & { layoutBaseVersion: number };
      if (candidate.layoutBaseVersion !== (workspace.layoutVersion ?? 0)) return route.fulfill({ status: 409, body: 'workspace layout changed in another view' });
      saves++;
      workspace = { ...candidate, layoutVersion: (workspace.layoutVersion ?? 0) + 1 };
      return route.fulfill({ json: workspace });
    }
    return route.fulfill({ json: [workspace] });
  });
  await page.routeWebSocket(/\/api\/panes\/layout_.*\/io/, ws => {
    const id = new URL(ws.url()).pathname.split('/').at(-2)!;
    connections.push(id);
    ws.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
    ws.send(Buffer.from(`\r\n${id.replace('_', ' ')} · transcript retained\r\n\r\n$ Ready for the next task\r\n`));
  });
  await page.goto('/?ws=Auto%20layout');
  for (const id of panes.keys()) await expect(page.locator(`[data-pane-id="${id}"] .xterm`)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Re-tile panes', exact: true })).toBeVisible();
  await page.locator('[data-pane-id="layout_5"] .pane-header').click();
  await page.evaluate(() => {
    const win = window as unknown as { __soluTerms: Map<string, Terminal>; retainedLayoutTerms: Map<string, Terminal> };
    win.retainedLayoutTerms = new Map(win.__soluTerms);
  });
  await page.screenshot({ path: testInfo.outputPath('workspace-layout-before.png') });
  await page.getByRole('button', { name: 'Re-tile panes', exact: true }).click();
  await expect(page.locator('[data-pane-id="layout_5"]')).toHaveClass(/focused/);
  await expect(page.getByRole('button', { name: 'Re-tile panes', exact: true })).toHaveCount(0);

  const geometry = async () => page.locator('[data-pane-id]').evaluateAll(nodes => {
    const boxes = nodes.map(node => node.getBoundingClientRect()).filter(box => box.width > 0 && box.height > 0).sort((a, b) => a.top - b.top || a.left - b.left);
    const rows: { top: number; widths: number[]; heights: number[] }[] = [];
    for (const box of boxes) {
      let row = rows.find(row => Math.abs(row.top - box.top) < 3);
      if (!row) { row = { top: box.top, widths: [], heights: [] }; rows.push(row); }
      row.widths.push(box.width); row.heights.push(box.height);
    }
    return rows.map(row => ({ count: row.widths.length, spread: Math.max(...row.widths) - Math.min(...row.widths) }));
  });
  await expect.poll(async () => (await geometry()).map(row => row.count)).toEqual([4, 4]);
  expect(creates).toBe(0);
  expect(connections.length).toBe(8);
  const divider = page.locator('.workspace-pane-grid [data-panel-resize-handle-id]:visible').first();
  const bounds = await divider.boundingBox();
  expect(bounds).not.toBeNull();
  await page.mouse.move(bounds!.x + bounds!.width / 2, bounds!.y + bounds!.height / 2);
  await page.mouse.down();
  await page.mouse.move(bounds!.x + bounds!.width / 2 + (bounds!.height > bounds!.width ? 35 : 0), bounds!.y + bounds!.height / 2 + (bounds!.width > bounds!.height ? 35 : 0), { steps: 5 });
  await page.mouse.up();
  await expect(page.getByRole('button', { name: 'Re-tile panes', exact: true })).toBeVisible();
  await expect.poll(() => workspace.layoutMode).toBe('free');
  await page.getByRole('button', { name: 'Re-tile panes', exact: true }).click();
  await expect.poll(async () => (await geometry()).map(row => row.count)).toEqual([4, 4]);

  const close = async (id: string) => {
    const priorSaves = saves;
    await page.locator(`[data-pane-id="${id}"]`).getByRole('button', { name: 'Close session', exact: true }).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Close', exact: true }).click();
    await expect(page.locator(`[data-pane-id="${id}"]`)).toHaveCount(0);
    await expect.poll(() => saves).toBeGreaterThan(priorSaves);
  };
  const open = async () => {
    const priorSaves = saves, newId = `layout_${next + 1}`;
    const id = collectPaneIds(workspace.layout)[0];
    await page.locator(`[data-pane-id="${id}"]`).getByRole('button', { name: 'Pane menu', exact: true }).click();
    await page.getByRole('menuitem', { name: 'Split right with shell here', exact: true }).click();
    await expect(page.locator(`[data-pane-id="${newId}"] .xterm`)).toBeVisible();
    await expect.poll(() => saves).toBeGreaterThan(priorSaves);
  };
  await close('layout_3'); await close('layout_6');
  await open(); await open();
  await close('layout_1'); await open();
  await expect.poll(async () => (await geometry()).map(row => row.count)).toEqual([4, 4]);
  expect((await geometry()).every(row => row.spread < 3)).toBe(true);
  await expect.poll(() => workspace.layoutMode).toBe('auto');
  expect(creates).toBe(3); expect(restarts).toEqual([]);
  expect(connections.every(id => connections.filter(other => other === id).length === 1)).toBe(true);
  expect(await page.evaluate(() => {
    const win = window as unknown as { __soluTerms: Map<string, Terminal>; retainedLayoutTerms: Map<string, Terminal> };
    return [...win.retainedLayoutTerms].filter(([key]) => !['layout_1', 'layout_3', 'layout_6'].includes(JSON.parse(key)[1]))
      .every(([key, term]) => win.__soluTerms.get(key) === term);
  })).toBe(true);
  await expect(page.locator('.toast-host .toast')).toHaveCount(0, { timeout: 10_000 });
  await page.screenshot({ path: testInfo.outputPath('workspace-layout-after.png') });
});

test('passive windows retain shared geometry and custom resizing does not publish layout edits', async ({ page, context }) => {
  test.setTimeout(45_000);
  await page.setViewportSize({ width: 1600, height: 1000 });
  const panes: PaneInfo[] = Array.from({ length: 8 }, (_, i) => ({ id: `shared_${i}`, title: `Shared ${i + 1}`, cwd: '/fixture/layout', cmd: ['bash'], status: 'running', pid: i + 1, cols: 80, rows: 24, createdAt: '', state: 'idle', hasClaude: false, lastActivityMs: 0 }));
  let tree: Layout | null = null;
  for (const pane of panes) tree = tree ? { type: 'split', direction: 'horizontal', ratio: .5, a: tree, b: { type: 'pane', paneId: pane.id } } : { type: 'pane', paneId: pane.id };
  let workspace: Workspace = { name: 'Shared geometry', layout: balancedLayout(tree), paneIds: panes.map(p => p.id), panes: panes.map(p => ({ ...p, kind: undefined })), activeAt: '', layoutMode: 'auto', layoutColumns: 4, layoutAspect: 1.6, layoutVersion: 5 };
  const signature = (w: Workspace) => JSON.stringify([w.layout, w.layoutMode, w.layoutColumns, w.layoutAspect]);
  let changedWrites = 0, conflicts = 0;
  await context.route('**/api/panes', route => route.fulfill({ json: panes }));
  await context.route('**/api/workspaces**', route => {
    if (route.request().method() === 'POST') {
      const candidate = route.request().postDataJSON() as Workspace & { layoutBaseVersion: number };
      const changed = signature(candidate) !== signature(workspace);
      if (candidate.layoutBaseVersion !== workspace.layoutVersion && changed) { conflicts++; return route.fulfill({ status: 409, body: 'workspace layout changed in another view' }); }
      if (changed) { changedWrites++; workspace = { ...candidate, layoutVersion: (workspace.layoutVersion ?? 0) + 1 }; }
      return route.fulfill({ json: workspace });
    }
    return route.fulfill({ json: [workspace] });
  });
  await context.routeWebSocket(/\/api\/panes\/shared_.*\/io/, ws => {
    ws.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
    ws.send(Buffer.from('\r\nRetained shared transcript\r\n'));
  });
  await page.goto('/?ws=Shared%20geometry');
  const peer = await context.newPage();
  await peer.setViewportSize({ width: 1000, height: 900 });
  await peer.goto('/?ws=Shared%20geometry');
  await expect(page.locator('[data-pane-id="shared_0"] .xterm')).toBeVisible();
  await expect(peer.locator('[data-pane-id="shared_0"] .xterm')).toBeVisible();
  await page.waitForTimeout(3500);
  expect(changedWrites).toBe(0); expect(conflicts).toBe(0);

  await page.bringToFront();
  await page.setViewportSize({ width: 1100, height: 1000 });
  await expect.poll(() => workspace.layoutVersion).toBe(6);
  await peer.bringToFront();
  await peer.waitForTimeout(3500); // summary-poll delivery to the other view
  expect(changedWrites).toBe(1); expect(conflicts).toBe(0);
  const peerRows = await peer.locator('[data-pane-id]').evaluateAll(nodes => {
    const rows: number[] = [];
    for (const node of nodes) { const rect = node.getBoundingClientRect(); if (rect.width > 0 && !rows.some(y => Math.abs(y - rect.top) < 3)) rows.push(rect.top); }
    return rows.length;
  });
  expect(peerRows).toBe(4);

  await page.bringToFront();
  const divider = page.locator('.workspace-pane-grid [data-panel-resize-handle-id]:visible').first();
  const bounds = (await divider.boundingBox())!;
  await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2);
  await page.mouse.down();
  await page.mouse.move(bounds.x + bounds.width / 2 + (bounds.height > bounds.width ? 25 : 0), bounds.y + bounds.height / 2 + (bounds.width > bounds.height ? 25 : 0), { steps: 4 });
  await page.mouse.up();
  await expect.poll(() => workspace.layoutMode).toBe('free');
  const customVersion = workspace.layoutVersion, customWrites = changedWrites;
  await page.setViewportSize({ width: 1500, height: 1000 });
  await peer.bringToFront();
  await peer.waitForTimeout(3500);
  await expect(peer.getByRole('button', { name: 'Re-tile panes', exact: true })).toBeVisible();
  expect(workspace.layoutVersion).toBe(customVersion);
  expect(changedWrites).toBe(customWrites); expect(conflicts).toBe(0);
  await expect(page.getByText(/Your local layout has not been saved/)).toHaveCount(0);
  await expect(peer.getByText(/Your local layout has not been saved/)).toHaveCount(0);
  await peer.close();
});
