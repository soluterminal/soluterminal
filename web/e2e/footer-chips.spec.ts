import { systemStatusFixture } from '../tools/screens-lib.mjs';
import { expect, test } from './fixtures';

// The two operator footer chips say what is wrong and why, and each leads to
// one fix: the agents panel offers a single action, and the service CPU chip
// opens System, where the background service can be restarted.
//
// The fix agent was retired (ST-300): the payload below still carries its
// leg, switched off and failing as an older daemon sends it, and neither the
// chip nor the panel may mention it.
test('the PR reviewer chip says it is down and the panel opens its log', async ({ page }, info) => {
  const startedAt = Date.now();
  await page.route('**/api/agents/status**', (route) => route.fulfill({ json: {
    fixAgent: {
      present: true, running: false, disabled: true, result: 'fail',
      finishedAt: new Date(startedAt - 5 * 86_400_000).toISOString(), ageS: null,
      nextRunAt: null, stale: true, stuck: [{ issue: 42, reason: 'exhausted' }],
      schedule: { id: 'sch_fix', enabled: false },
    },
    prAgent: {
      present: true, running: false, disabled: false, result: 'ok',
      finishedAt: new Date(startedAt - 47 * 60_000).toISOString(),
      heartbeat: {
        state: 'down', downSince: new Date(startedAt - 47 * 60_000).toISOString(),
        detail: 'error: agent not installed on this machine', scheduleId: 'sch_pr',
      },
    },
    cloud: null,
    generatedAt: new Date().toISOString(),
    fetchedAt: new Date().toISOString(),
  } }));
  await page.route('**/api/schedules', (route) => route.fulfill({ json: [{
    id: 'sch_pr', name: 'PR babysitter', kind: 'agent', agentId: 'pr-agent', cron: '2,12,22,32,42,52 * * * *',
    enabled: true, createdAt: '2026-11-01T00:00:00Z', nextFire: null,
    fireCount: 4, skipCount: 0, errorCount: 1,
    runs: [{ at: new Date(startedAt - 60_000).toISOString(), status: 'error', detail: 'agent not installed on this machine' }],
  }] }));

  await page.goto('/');
  await expect(page.locator('.statusbar')).toBeVisible();
  const chip = page.locator('.statusbar-agents.down');
  await expect(chip).toHaveText('PR reviewer down');
  await expect(chip).toHaveAttribute('aria-label', /PRs to main are not getting solu\/review\. Last run error: agent not installed on this machine\./);
  await expect(page.locator('.statusbar')).not.toContainText(/fix agent/i);

  await chip.click();
  const panel = page.getByRole('dialog', { name: 'Agents status' });
  await expect(panel.locator('.vp-verdict')).toContainText("PR reviewer down for 4");
  await expect(panel).not.toContainText(/fix agent/i);
  const action = panel.locator('.ap-verdict-action');
  await expect(action).toHaveCount(1);
  await expect(action).toHaveText('Open pr-agent log');
  await page.screenshot({ path: info.outputPath('agents-panel-reviewer-down.png') });

  await action.click();
  await expect(panel).toBeHidden();
});

test('the service CPU chip opens System, where the background service restarts', async ({ page }, info) => {
  const text = 'Background service CPU: 4.2 cores';
  const reasons = [{ key: 'daemonCpu', level: 'warn', text, detail: 'SoluTerminal itself, not your panes; of 8 cores on this machine; warns when it holds 2.0 or more for about 30 s' }];
  // The chip reads the health summary; the page reads the full status.
  await page.route('**/api/health**', async (route) => {
    const health = await (await route.fetch()).json();
    health.system = { level: 'warn', reason: text, reasons, sampledAt: new Date().toISOString() };
    return route.fulfill({ json: health });
  });
  await page.route('**/api/system/status**', (route) => {
    const status = systemStatusFixture();
    status.daemon.cpuPct = 420;
    status.level = { ...status.level, overall: 'warn', reason: text, reasons, areas: { ...status.level.areas, daemonCpu: 'warn' } };
    return route.fulfill({ json: status });
  });
  await page.route('**/api/service/restart', (route) => route.fulfill({ json: { supported: true, pid: 42 } }));

  await page.goto('/');
  const chip = page.locator('.statusbar-system.warn');
  await expect(chip).toHaveText(text);
  await expect(chip).toHaveAttribute('aria-label', /not your panes.*click to open System and restart the background service/);

  await chip.click();
  const card = page.locator('.system-card', { hasText: 'Background service' });
  await expect(card).toContainText(/4\.2 of \d+ cores/);
  await expect(card).toContainText('This is SoluTerminal itself, not your panes.');
  // The card sits low on a long page; it comes into view without scrolling.
  const restart = card.getByRole('button', { name: 'Restart background service' });
  await expect(restart).toBeEnabled();
  await expect(restart).toBeInViewport();
  await page.screenshot({ path: info.outputPath('system-background-service.png') });
});
