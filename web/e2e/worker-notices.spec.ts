import { expect, test } from './fixtures';
import type { PaneInfo, Workspace } from '../src/types';

test('routine worker notes stay quiet, dismiss cleanly, and person escalations still need you', async ({ page }, info) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  const note = { id: 'worker-note', text: 'Worker @build kept open: uncommitted work', source: '@lead', workerPane: 'build', createdAt: 1 };
  const lead = {
    id: 'notice-lead', title: 'Release lead', cwd: '/project', cmd: ['bash'],
    status: 'running', state: 'idle', attention: 'none', createdAt: '2026-11-03T00:00:00Z',
    cols: 80, rows: 24, pid: 1, lastActivityMs: 0, workerNotices: [note],
  } as PaneInfo;
  const workspace = { name: 'Worker notes', layout: { type: 'pane', paneId: lead.id }, paneIds: [lead.id], activeAt: '2026-11-03T00:00:00Z' } as Workspace;
  await page.route('**/api/panes', route => route.fulfill({ json: [lead] }));
  await page.route('**/api/workspaces**', route => route.fulfill({ json: [workspace] }));
  const dismissed: string[] = [];
  await page.route('**/api/asks/*/dismiss', route => {
    dismissed.push(route.request().url().split('/').at(-2)!);
    delete lead.workerNotices; // matches the daemon's omitempty response
    return route.fulfill({ json: { ok: true } });
  });
  await page.routeWebSocket(/\/api\/panes\/notice-lead\/io/, ws => {
    ws.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
    ws.send(Buffer.from('\r\nWorker notes fixture\r\n'));
  });
  await page.goto('/?ws=Worker%20notes');
  const pane = page.locator('[data-pane-id="notice-lead"]');
  const memo = pane.locator('.pane-worker-notices:visible');
  await expect(memo).toBeVisible();
  await expect(memo).toContainText(note.text);
  await expect(page.getByRole('button', { name: 'Nothing is waiting on you', exact: true })).toBeVisible();
  await expect(page.getByRole('treeitem', { name: 'Release lead', exact: true }).locator('.pane-tree-needs-you')).toHaveCount(0);
  await page.screenshot({ animations: 'disabled', path: info.outputPath('quiet-lead-worker-note.png') });
  await memo.getByRole('button', { name: 'Dismiss', exact: true }).click();
  await expect(pane.locator('.pane-worker-notices:visible')).toHaveCount(0);
  expect(dismissed).toEqual([note.id]);
  await page.reload();
  await expect(pane.locator('.pane-worker-notices:visible')).toHaveCount(0);

  lead.asks = [{ ...note, text: 'Decide how to preserve unpublished work kept for at least 72 hours' }];
  await page.reload();
  await expect(pane.locator('.pane-asks-chip:visible')).toContainText('Waiting on you: Decide how to preserve');
  await expect(page.getByRole('button', { name: '1 item is waiting on you', exact: true })).toBeVisible();
  await expect(page.getByRole('treeitem', { name: 'Release lead', exact: true }).locator('.pane-tree-needs-you')).toHaveText('1 ask');
  await page.screenshot({ animations: 'disabled', path: info.outputPath('worker-person-decision.png') });
});
