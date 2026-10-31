import { expect, test } from './fixtures';
import type { PaneInfo, Workspace } from '../src/types';

for (const signals of [false, true]) {
  test(`crowded pane headers keep menu and close inside a 320px pane${signals ? ' with warning/turn chips' : ''}`, async ({ page }, testInfo) => {
    // A desktop viewport with a narrow pane exercises container queries, rather
    // than the separate mobile media rules. All daemon traffic uses fixtures.
    await page.setViewportSize({ width: 1440, height: 900 });
    const pane: PaneInfo = {
      id: 'narrow_header', title: '@cmo · launch', cwd: '/home/user/launch', cmd: ['codex'],
      status: 'running', pid: 11, cols: 80, rows: 24, state: 'idle', hasCodex: true,
      createdAt: '2026-10-31T00:00:00Z', lastActivityMs: 0, autoMode: 'always',
      codexThreadId: 'narrow-header-session', resumeReady: true, role: 'task_owner',
      identity: { handle: 'cmo', workingOn: 'Preparing the launch campaign' },
      origin: { source: 'agent', byPaneId: 'lead', byPaneTitle: 'Lead', at: Date.now() },
      lastAgentMessage: { fromPaneId: 'lead', fromLabel: '@lead · #1 Campaign lead', at: Date.now(), outcome: 'sent' },
    };
    if (signals) {
      pane.unsentMessage = { id: 'unsent', sender: '@a-long-sender-handle' };
      pane.review = { verdict: 'request-changes', reviewer: 'claude', author: 'codex', round: 1, mustFix: 3, notes: 0, at: Date.now() };
      pane.turn = { active: false, count: 1, last: { n: 1, startedMs: Date.now() - 1000, endedMs: Date.now(), files: 12, added: 1200, removed: 300 } };
    }
    const lead: PaneInfo = { ...pane, id: 'lead', title: 'Lead', identity: undefined, origin: undefined, lastAgentMessage: undefined };
    const workspaces: Workspace[] = [{
      name: 'Narrow header', layout: { type: 'pane', paneId: pane.id },
      paneIds: [pane.id, lead.id], activeAt: pane.createdAt,
    }];
    await page.route('**/api/panes', route => route.fulfill({ json: [pane, lead] }));
    await page.route('**/api/workspaces**', route => route.fulfill({ json: workspaces }));
    await page.routeWebSocket(/\/api\/panes\/narrow_header\/io/, ws => ws.send(Buffer.from('\r\nReady\r\n')));
    await page.goto('/?ws=Narrow%20header');
    const header = page.locator(`[data-pane-id="${pane.id}"] .pane-header`);
    await expect(header.locator('.pane-identity-chip')).toBeVisible();
    await expect(header.locator('.pane-message-chip')).toBeVisible();
    await expect(header.locator('.pane-origin')).toBeVisible();

    const checkActions = async () => {
      const names = signals ? ['Pane menu', 'Close session', 'Send', 'Clear'] : ['Pane menu', 'Close session'];
      for (const name of names) {
        const button = header.getByRole('button', { name, exact: true });
        await expect(button).toBeVisible();
        const outer = (await header.boundingBox())!;
        const inner = (await button.boundingBox())!;
        expect(inner.x).toBeGreaterThanOrEqual(outer.x);
        expect(inner.x + inner.width).toBeLessThanOrEqual(outer.x + outer.width);
        // Visibility alone does not detect clipping or another chip covering it.
        await button.click({ trial: true });
      }
    };

    for (const width of [400, 320, 256]) {
      await header.evaluate((el, width) => { el.style.width = `${width}px`; }, width);
      await expect(header.locator('.activity-dot')).toBeVisible();
      await expect(header.locator('.pane-title')).toBeVisible();
      if (signals) {
        await expect(header.locator('.pane-review-chip.is-warn')).toBeVisible();
        await expect(header.locator('.pane-unsent-message')).toBeVisible();
        await expect(header.locator('.pane-turn-chip')).toBeVisible();
      }
      await checkActions();
      if (width <= 320) {
        await expect(header.locator('.pane-identity-chip')).toBeHidden();
      }
      if (width <= 256) {
        await expect(header.locator('.pane-message-chip')).toBeHidden();
      }
    }

    await header.evaluate(el => { el.style.width = '256px'; });
    await header.locator('.pane-message-chip').evaluate(el => el.classList.add('unread'));
    await expect(header.locator('.pane-message-chip')).toBeVisible();
    for (const state of ['is-attention', 'is-warn']) {
      await header.locator('.pane-message-chip').evaluate((el, state) => { el.className = `pane-message-chip ${state}`; }, state);
      await expect(header.locator('.pane-message-chip')).toBeVisible();
      await checkActions();
    }
    await header.evaluate(el => { el.style.width = '320px'; });
    await page.screenshot({ path: testInfo.outputPath('pane-header-320px.png') });
  });
}
