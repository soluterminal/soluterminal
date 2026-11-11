import { systemStatusFixture } from '../tools/screens-lib.mjs';
import { expect, test } from './fixtures';

// The two operator footer chips say what is wrong and why, and each leads to
// one fix: the agents panel offers a single action, and the service CPU chip
// opens System, where the background service can be restarted.
test('the fix agent chip says why it is down and the panel offers one fix', async ({ page }, info) => {
  const startedAt = Date.now();
  await page.clock.install();
  await page.route('**/api/agents/status**', (route) => route.fulfill({ json: {
    fixAgent: {
      present: true, running: false, disabled: false, result: 'ok',
      finishedAt: new Date(startedAt - 51.5 * 60_000).toISOString(), ageS: null,
      nextRunAt: null, stale: false, stuck: [],
    },
    prAgent: null,
    cloud: null,
    generatedAt: new Date().toISOString(),
    fetchedAt: new Date().toISOString(),
  } }));
  await page.route('**/api/schedules', (route) => route.fulfill({ json: [{
    id: 'sch_fix', name: 'Fix agent', kind: 'agent', agentId: 'fix-agent', cron: '*/15 * * * *',
    enabled: true, createdAt: '2026-11-01T00:00:00Z', nextFire: null,
    fireCount: 4, skipCount: 0, errorCount: 1,
    runs: [{ at: new Date(startedAt - 60_000).toISOString(), status: 'error', detail: 'agent not installed on this machine' }],
  }] }));
  await page.route('**/api/agents/bootstrap', (route) => route.fulfill({ json: {
    supported: true, embeddedCode: true,
    checks: { git: true, gh: true, bwrap: true, node: true, claudeSubscription: 'max' },
    agents: [{
      id: 'fix-agent', title: 'Fix agent', present: false, codeSource: 'missing',
      envReady: false, envKeys: [], scheduled: true, scheduleId: 'sch_fix', apply: true,
      env: [{ key: 'FEEDBACK_GITHUB_TOKEN', label: 'Feedback repo token', secret: true, required: true }],
    }],
  } }));

  await page.goto('/');
  await expect(page.locator('.statusbar')).toBeVisible();
  // Down is confirmed by a second poll, one minute after the first.
  await page.clock.fastForward(61_000);
  const chip = page.locator('.statusbar-agents.down');
  await expect(chip).toHaveText(/^Fix agent: no run for 5\d min$/);
  await expect(chip).toHaveAttribute('aria-label', /Last attempt failed: agent not installed on this machine · click for the fix$/);

  await chip.click();
  const panel = page.getByRole('dialog', { name: 'Agents status' });
  await expect(panel.locator('.ap-why')).toHaveText('Last attempt failed: agent not installed on this machine');
  const action = panel.locator('.ap-verdict-action');
  await expect(action).toHaveCount(1);
  await expect(action).toHaveText('Repair setup');
  await page.screenshot({ path: info.outputPath('agents-panel-down.png') });

  await action.click();
  await expect(panel.getByText('Feedback repo token')).toBeVisible();
  await page.screenshot({ path: info.outputPath('agents-panel-repair.png') });
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
