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

// ST-317 (C3): the Recording item is never hidden. On a daemon that only
// records agent panes (pane-recording.v1, as this fixture serves) a shell's
// item is greyed with the reason and what to do.
test('a plain shell pane shows Recording greyed with the reason on an older daemon', async ({ page }) => {
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
  const start = page.getByRole('menuitem', { name: 'Start recording output…' });
  await expect(start).toBeDisabled();
  await expect(page.getByText('Update SoluTerminal to record this pane. This version records Claude and Codex panes only.')).toBeVisible();
  await expect(page.getByTestId('statusbar-recording')).toHaveCount(0);
});

// ST-317 (amendment section 4): one switch and one confirm record every pane
// in the workspace; the confirm lists each pane with its defaults and the
// skipped ones; each recorded pane shows all three marks. Synthetic fixtures
// against a mocked pane-recording.v2 daemon; nothing is recorded.
test('record every pane in this workspace: one confirm, a mark on each recorded pane', async ({ page }, info) => {
  await page.setViewportSize({ width: 1600, height: 1000 });
  const base = { cwd: '/project', status: 'running', state: 'idle', attention: 'none', createdAt: '2026-11-20T00:00:00Z', cols: 80, rows: 24, pid: 1, lastActivityMs: 0 };
  const agent = { ...base, id: 'ws-agent', title: 'Builder', cmd: ['claude'], agent: 'claude', hasClaude: true } as PaneInfo;
  const shell = { ...base, id: 'ws-shell', title: 'Terminal', cmd: ['bash'] } as PaneInfo;
  const keys = { ...base, id: 'ws-keys', title: 'Keys', cmd: ['bash'] } as PaneInfo;
  const panes = [agent, shell, keys];
  const layout = { type: 'split', direction: 'horizontal', ratio: 0.5, a: { type: 'pane', paneId: agent.id }, b: { type: 'split', direction: 'vertical', ratio: 0.5, a: { type: 'pane', paneId: shell.id }, b: { type: 'pane', paneId: keys.id } } };
  const ws = { name: 'Team', layout, paneIds: panes.map(p => p.id), activeAt: '2026-11-20T00:00:00Z' } as unknown as Workspace;
  const checks: Record<string, unknown> = { [agent.id]: {}, [shell.id]: { defaults: ['shell'] }, [keys.id]: { hard: 'hard_folder' } };
  await page.route('**/api/health', async route => {
    const res = await route.fetch();
    const json = await res.json();
    return route.fulfill({ json: { ...json, features: ['pane-recording.v1', 'pane-recording.v2'] } });
  });
  await page.route('**/api/settings/recording', route => route.fulfill({ json: { deleteAfterDays: 5, capMB: 2000, bounds: { deleteAfterDaysMin: 1, deleteAfterDaysMax: 30, capMBMin: 500, capMBMax: 10000, hoursMin: 1, hoursMax: 24, hoursDefault: 8 } } }));
  await page.route('**/api/panes', route => route.fulfill({ json: panes }));
  await page.route('**/api/workspaces**', route => route.fulfill({ json: [ws] }));
  await page.route(/\/api\/panes\/ws-[a-z]+\/recording$/, route => {
    const id = new URL(route.request().url()).pathname.split('/')[3];
    const p = panes.find(x => x.id === id)!;
    return route.fulfill({ json: { ...(p.recording ?? { on: false }), check: checks[id] } });
  });
  const sent: unknown[] = [];
  await page.route('**/api/recording/workspace', async route => {
    const body = route.request().postDataJSON() as { on: boolean; hours?: number; panes?: { id: string; override?: string[] }[] };
    sent.push(body);
    if (body.on) {
      for (const { id } of body.panes ?? []) panes.find(p => p.id === id)!.recording = { on: true, startedAt: 1795100000000, hours: body.hours };
      return route.fulfill({ json: { recorded: (body.panes ?? []).map(p => p.id), skipped: [] } });
    }
    const stopped = panes.filter(p => p.recording?.on).map(p => p.id);
    for (const p of panes) delete p.recording;
    return route.fulfill({ json: { recorded: [], stopped, skipped: [] } });
  });
  await page.routeWebSocket(/\/api\/panes\/.*\/io/, sock => {
    sock.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
  });
  await page.goto('/?ws=Team');

  const header = (id: string) => page.locator(`[data-pane-id="${id}"] .pane-header`);
  const row = (title: string) => page.getByRole('treeitem', { name: title, exact: true });
  await expect(header(agent.id)).toBeVisible();
  await header(agent.id).click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Record every pane in this workspace…' }).click();

  const dialog = page.getByRole('dialog');
  await expect(dialog).toContainText('deleted after 5 days');
  await expect(dialog).toContainText('Panes opened later are not added');
  await expect(dialog.locator('[data-pane-id="ws-keys"]')).toContainText('Skipped: This pane works in a folder that is never recorded');
  const shellRow = dialog.locator('[data-pane-id="ws-shell"]');
  await expect(shellRow).toContainText('Shells print secrets (tokens, env, cat). Switch recording off before a login or sudo.');
  await shellRow.getByRole('checkbox').check();
  await dialog.getByLabel('Time limit').selectOption('4');
  await page.screenshot({ animations: 'disabled', path: info.outputPath('workspace-confirm.png') });
  await dialog.getByRole('button', { name: 'Start recording' }).click();

  // Each recorded pane: header, sidebar row and the status-bar count.
  for (const p of [agent, shell]) {
    await expect(header(p.id).getByTestId('pane-rec-header')).toHaveText('REC', { timeout: 8000 });
    await expect(row(p.title).getByTestId('pane-rec-row')).toHaveText('REC');
  }
  await expect(header(keys.id).getByTestId('pane-rec-header')).toHaveCount(0);
  await expect(page.getByTestId('statusbar-recording')).toHaveText('2 recording');
  await page.screenshot({ animations: 'disabled', path: info.outputPath('workspace-recording.png') });

  // One switch stops them all.
  await header(shell.id).click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Stop recording this workspace' }).click();
  for (const p of [agent, shell]) await expect(header(p.id).getByTestId('pane-rec-header')).toHaveCount(0, { timeout: 8000 });
  await expect(page.getByTestId('statusbar-recording')).toHaveCount(0);
  expect(sent).toEqual([
    // Each pane names the process the person confirmed (the fixtures' pid).
    { workspace: 'Team', on: true, hours: 4, panes: [{ id: 'ws-agent', override: [], pid: 1 }, { id: 'ws-shell', override: ['shell'], pid: 1 }] },
    { workspace: 'Team', on: false },
  ]);
});
