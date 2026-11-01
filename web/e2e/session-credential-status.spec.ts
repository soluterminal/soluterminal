import type { PaneInfo, Workspace } from '../src/types';
import { expect, test } from './fixtures';

for (const theme of ['dark', 'light']) {
  test(`sessions distinguish retained and shared active accounts (${theme})`, async ({ page }) => {
    await page.setViewportSize({ width: 1600, height: 1000 });
    const panes: PaneInfo[] = ['personal', 'broker'].map((kind) => ({
      id: `credential_${kind}`, title: kind, cwd: '/tmp', cmd: ['codex'],
      status: 'running', createdAt: '2026-10-07T00:00:00Z', cols: 80, rows: 30,
      pid: 1, lastActivityMs: 0, state: 'idle', hasCodex: true,
      credential: kind === 'personal'
        ? { provider: 'codex', account: 'personal-old', activeAccount: 'team__current', source: 'launch', restartRequired: true }
        : { provider: 'codex', account: 'team__previous', activeAccount: 'team__current', source: 'broker', restartRequired: false, teamId: 'same-team', activeTeamId: 'same-team' },
    }));
    const workspaces: Workspace[] = [{ name: 'Shared credentials',
      layout: { type: 'split', direction: 'horizontal', ratio: 0.5,
        a: { type: 'pane', paneId: panes[0].id }, b: { type: 'pane', paneId: panes[1].id } },
      paneIds: panes.map(pane => pane.id), activeAt: '2026-10-07T00:00:00Z' }];
    await page.addInitScript((selectedTheme) => {
      localStorage.setItem('solu.appTheme', selectedTheme);
      localStorage.setItem('solu.activeWorkspace', 'Shared credentials');
    }, theme);
    await page.route('**/api/panes', route => route.fulfill({ json: panes }));
    await page.route('**/api/workspaces**', route => route.fulfill({ json: workspaces }));
    await page.routeWebSocket(/\/api\/panes\/credential_.*\/io/, ws => ws.send(Buffer.from('Synthetic credential session\r\n')));
    await page.goto('/');
    const personal = page.locator('[data-pane-id="credential_personal"] .pane-account-chip');
    const broker = page.locator('[data-pane-id="credential_broker"] .pane-account-chip');
    // A retained personal login is named, and the chip offers the fix.
    await expect(personal).toHaveText('still on personal-old');
    await expect(personal).toHaveAttribute('title', /Started on personal-old; team__current is selected/);
    await personal.click();
    await expect(page.getByRole('dialog', { name: 'Still using personal-old' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Restart session' })).toBeVisible();
    // A proxied session following the active credential is healthy: no chip.
    await expect(broker).toHaveCount(0);
    panes[1].credential = { ...panes[1].credential!, activeTeamId: 'another-team', restartRequired: true };
    await page.reload();
    await expect(broker).toHaveText('still on team__previous');
    await expect(broker).toHaveAttribute('title', /team__current is selected/);
  });
}
