import { expect, test } from './fixtures';
import type { PaneInfo, Workspace } from '../src/types';

// The recording mark comes from daemon state only: the pane header, the pane's
// sidebar row and the status bar's "N recording" (which counts panes in every
// workspace). Synthetic fixtures; nothing is recorded.
test('the recording mark follows the daemon on the header, the sidebar row and the status bar', async ({ page }, info) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  const base = { cwd: '/project', status: 'running', state: 'idle', attention: 'none', createdAt: '2026-11-19T00:00:00Z', cols: 80, rows: 24, pid: 1, lastActivityMs: 0 };
  const agent = { ...base, id: 'rec-agent', title: 'Recorded agent', cmd: ['claude'], agent: 'claude', hasClaude: true } as PaneInfo;
  const elsewhere = {
    ...base, id: 'rec-elsewhere', title: 'Other workspace agent', cmd: ['codex'], agent: 'codex', hasCodex: true,
    recording: { on: true, startedAt: 1795060000000, bytesRecorded: 10 },
  } as PaneInfo;
  const here = { name: 'Recording', layout: { type: 'pane', paneId: agent.id }, paneIds: [agent.id], activeAt: '2026-11-19T00:00:01Z' } as Workspace;
  const there = { name: 'Elsewhere', layout: { type: 'pane', paneId: elsewhere.id }, paneIds: [elsewhere.id], activeAt: '2026-11-19T00:00:00Z' } as Workspace;
  await page.route('**/api/panes', route => route.fulfill({ json: [agent, elsewhere] }));
  await page.route('**/api/workspaces**', route => route.fulfill({ json: [here, there] }));
  const calls: unknown[] = [];
  await page.route('**/api/panes/rec-agent/recording', async route => {
    const body = route.request().postDataJSON() as { on?: boolean; dismiss?: boolean };
    calls.push(body);
    if (body.on === true) agent.recording = { on: true, startedAt: 1795060100000, bytesRecorded: 0 };
    else if (body.on === false) delete agent.recording;
    else if (body.dismiss) delete agent.recording;
    return route.fulfill({ json: agent.recording ?? { on: false } });
  });
  await page.routeWebSocket(/\/api\/panes\/.*\/io/, ws => {
    ws.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
    ws.send(Buffer.from('\r\nRecording fixture\r\n'));
  });
  await page.goto('/?ws=Recording');

  const header = page.locator('[data-pane-id="rec-agent"] .pane-header');
  const row = page.getByRole('treeitem', { name: 'Recorded agent', exact: true });
  const statusChip = page.getByTestId('statusbar-recording');
  await expect(header).toBeVisible();
  // Off: no mark here; the other workspace's recording pane still counts.
  await expect(header.getByTestId('pane-rec-header')).toHaveCount(0);
  await expect(row.getByTestId('pane-rec-row')).toHaveCount(0);
  await expect(statusChip).toHaveText('1 recording');

  // The person switches it on from the pane menu, after the explanation.
  await header.click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Start recording output…' }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toContainText('Saved on this computer only');
  await dialog.getByRole('button', { name: 'Start recording' }).click();
  await expect(header.getByTestId('pane-rec-header')).toHaveText('REC');
  await expect(row.getByTestId('pane-rec-row')).toHaveText('REC');
  await expect(statusChip).toHaveText('2 recording');
  await expect(row).toHaveAccessibleDescription(/recording/);
  await page.screenshot({ animations: 'disabled', path: info.outputPath('recording-on.png') });

  // Off again: the marks go; the other workspace's recording still counts.
  await header.click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Stop recording' }).click();
  await expect(header.getByTestId('pane-rec-header')).toHaveCount(0);
  await expect(row.getByTestId('pane-rec-row')).toHaveCount(0);
  await expect(statusChip).toHaveText('1 recording');

  // A stop the person should see stays until they dismiss it.
  agent.recording = { on: false, stoppedReason: 'disk cap', stoppedAt: 1795060200000 };
  await expect(header.getByTestId('pane-rec-stopped-header')).toContainText('recording stopped: disk cap', { timeout: 8000 });
  await expect(row.getByTestId('pane-rec-stopped-row')).toBeVisible();
  await header.getByTestId('pane-rec-stopped-header').getByRole('button', { name: 'Dismiss' }).click();
  await expect(header.getByTestId('pane-rec-stopped-header')).toHaveCount(0);
  expect(calls).toEqual([{ on: true }, { on: false }, { dismiss: true }]);
});

// Plain shells can't be recorded: the menu doesn't offer it.
test('a plain shell pane offers no recording', async ({ page }) => {
  await page.setViewportSize({ width: 1400, height: 900 });
  const shell = { id: 'rec-shell', title: 'Shell', cwd: '/project', cmd: ['bash'], status: 'running', state: 'idle', attention: 'none', createdAt: '2026-11-19T00:00:00Z', cols: 80, rows: 24, pid: 1, lastActivityMs: 0 } as PaneInfo;
  const ws = { name: 'Shell', layout: { type: 'pane', paneId: shell.id }, paneIds: [shell.id], activeAt: '2026-11-19T00:00:00Z' } as Workspace;
  await page.route('**/api/panes', route => route.fulfill({ json: [shell] }));
  await page.route('**/api/workspaces**', route => route.fulfill({ json: [ws] }));
  await page.routeWebSocket(/\/api\/panes\/.*\/io/, ws2 => {
    ws2.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
  });
  await page.goto('/?ws=Shell');
  await page.locator('[data-pane-id="rec-shell"] .pane-header').click({ button: 'right' });
  await expect(page.getByRole('menuitem', { name: 'Rename' })).toBeVisible();
  await expect(page.getByRole('menuitem', { name: 'Start recording output…' })).toHaveCount(0);
  await expect(page.getByTestId('statusbar-recording')).toHaveCount(0);
});
