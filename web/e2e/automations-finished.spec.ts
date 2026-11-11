import { expect, test } from './fixtures';

// A finished automation reads "done" (or "missed"), sits under its own heading
// with no switch to tick, stays out of the header count, and one button clears
// them all. The dialog says "automation" as the section does.
test('finished automations read done and clear in one go', async ({ page }, info) => {
  const now = Date.now();
  const iso = (ms: number) => new Date(ms).toISOString();
  const base = { kind: 'task', prompt: 'Run the tests', cwd: '/repo', createdAt: iso(now - 86_400_000), skipCount: 0, errorCount: 0 };
  let schedules = [
    { ...base, id: 'live', name: 'Nightly tests', cron: '0 2 * * *', enabled: true, nextFire: iso(now + 3_600_000), fireCount: 3 },
    { ...base, id: 'paused', name: 'Weekly digest', cron: '0 9 * * 1', enabled: false, nextFire: null, fireCount: 1 },
    {
      ...base, id: 'once-done', name: 'Ship reminder', cron: '', runAt: iso(now - 7_200_000), enabled: false, nextFire: null,
      fireCount: 1, runs: [{ at: iso(now - 7_200_000), status: 'fired' }],
    },
    {
      ...base, id: 'once-missed', name: 'Lunch check', cron: '', runAt: iso(now - 3_600_000), enabled: false, nextFire: null,
      fireCount: 0, skipCount: 1, runs: [{ at: iso(now - 3_600_000), status: 'skipped', detail: 'pane busy' }],
    },
  ];
  const deleted: string[] = [];
  await page.route('**/api/schedules', (route) => route.fulfill({ json: schedules }));
  await page.route('**/api/schedules/*', (route) => {
    const id = new URL(route.request().url()).pathname.split('/').pop()!;
    if (route.request().method() === 'DELETE') {
      deleted.push(id);
      schedules = schedules.filter((s) => s.id !== id);
      return route.fulfill({ status: 204, body: '' });
    }
    const one = schedules.find((s) => s.id === id);
    return one ? route.fulfill({ json: one }) : route.fallback();
  });

  await page.goto('/');
  // The header counts only the two that can still run.
  const header = page.getByRole('button', { name: 'Automations 2', exact: true });
  await expect(header).toBeVisible();
  const rows = page.locator('.sched-row');
  if (await rows.count() === 0) await header.click();
  await expect(rows).toHaveCount(4);

  // Live and paused first, then the finished ones under one heading.
  await expect(rows.nth(0)).toContainText('Nightly tests');
  await expect(rows.nth(1)).toContainText('Weekly digest · paused');
  await expect(rows.nth(2)).toContainText('Ship reminder · done');
  await expect(rows.nth(3)).toContainText('Lunch check · missed');
  await expect(page.locator('.sched-finished-head')).toHaveCount(1);
  await expect(page.locator('.sched-row.finished input[type="checkbox"]')).toHaveCount(0);
  await expect(page.locator('.sched-row:not(.finished) input[type="checkbox"]')).toHaveCount(2);
  await page.screenshot({ path: info.outputPath('automations-finished.png') });

  await page.getByRole('button', { name: 'Clear finished' }).first().click();
  const confirm = page.getByRole('dialog').filter({ hasText: 'Clear 2 finished automations?' });
  await expect(confirm).toContainText('Their run history and reports are removed too.');
  await page.screenshot({ path: info.outputPath('automations-clear-confirm.png') });
  await confirm.getByRole('button', { name: 'Clear finished' }).click();

  await expect(rows).toHaveCount(2);
  await expect(page.locator('.sched-finished-head')).toHaveCount(0);
  expect(deleted.sort()).toEqual(['once-done', 'once-missed']);

  await page.getByRole('button', { name: 'New automation' }).first().click();
  await expect(page.getByText('New automation', { exact: true })).toBeVisible();
  await expect(page.getByText('Start with a recipe')).toBeVisible();
  await page.screenshot({ path: info.outputPath('automations-new-dialog.png') });
});
