import { expect, test } from './fixtures';

// The automation form in a real browser: a failed list load can be retried,
// the form is a modal dialog that keeps Tab inside, Escape closes it, a
// changed form asks before it is thrown away, and an automation whose pane
// has closed cannot be saved until another pane is picked.
test('automation form keeps what was typed and the list recovers from a failed load', async ({ page }) => {
  const now = Date.now();
  const iso = (ms: number) => new Date(ms).toISOString();
  const schedules = [
    {
      id: 'orphan', name: 'Standup nudge', kind: 'prompt', prompt: 'Post the standup', paneId: 'pane-that-closed',
      cron: '0 9 * * 1-5', enabled: true, nextFire: iso(now + 3_600_000), createdAt: iso(now - 86_400_000),
      fireCount: 2, skipCount: 0, errorCount: 0,
    },
  ];
  let failing = true;
  await page.route('**/api/schedules', (route) => (
    failing ? route.fulfill({ status: 500, body: 'boom' }) : route.fulfill({ json: schedules })
  ));
  await page.route('**/api/schedules/*', (route) => {
    const id = new URL(route.request().url()).pathname.split('/').pop()!;
    const one = schedules.find((s) => s.id === id);
    return one ? route.fulfill({ json: one }) : route.fallback();
  });

  await page.goto('/');
  const header = page.locator('.section-header-toggle', { hasText: 'Automations' });
  await expect(header).toBeVisible();
  const section = page.locator('.sidebar-section').filter({ has: header });
  const failure = section.getByRole('alert');
  // The section may start collapsed, depending on saved sidebar state.
  if ((await header.getAttribute('aria-expanded')) === 'false') await header.click();
  await expect(failure).toContainText('Couldn’t load automations.');
  await expect(section.getByText(/No automations yet/)).toHaveCount(0);

  failing = false;
  await failure.getByRole('button', { name: 'Retry' }).click();
  const rows = page.locator('.sched-row');
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText('(closed pane)');
  await expect(section.getByRole('alert')).toHaveCount(0);

  // New automation: a labelled modal dialog with focus inside it.
  await page.getByRole('button', { name: 'New automation' }).first().click();
  const dialog = page.getByRole('dialog', { name: 'New automation' });
  await expect(dialog).toBeVisible();
  const focusInside = () => page.evaluate(() => !!document.activeElement?.closest('.schedule-dialog'));
  expect(await focusInside()).toBe(true);

  // Untouched: Escape just closes.
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);

  await page.getByRole('button', { name: 'New automation' }).first().click();
  await dialog.getByRole('button', { name: /Start from scratch/ }).click();
  // From scratch starts daily, not hourly.
  await expect(dialog.getByRole('combobox', { name: 'Runs' })).toHaveValue('0 9 * * *');
  const task = dialog.getByPlaceholder(/What should Claude do each run/);
  await task.fill('Run the tests and report only what failed.');

  // Tab never leaves the dialog.
  for (let i = 0; i < 40; i++) {
    await page.keyboard.press('Tab');
    expect(await focusInside()).toBe(true);
  }
  for (let i = 0; i < 5; i++) {
    await page.keyboard.press('Shift+Tab');
    expect(await focusInside()).toBe(true);
  }

  // Changed: Escape asks first, and "Keep editing" keeps every word.
  await page.keyboard.press('Escape');
  const question = page.getByRole('dialog', { name: 'Discard this automation?' });
  await expect(question).toBeVisible();
  await question.getByRole('button', { name: 'Keep editing' }).click();
  await expect(question).toHaveCount(0);
  await expect(task).toHaveValue('Run the tests and report only what failed.');

  // Escape on the question itself answers the question, not the form.
  await page.keyboard.press('Escape');
  await expect(question).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(question).toHaveCount(0);
  await expect(dialog).toBeVisible();
  await expect(task).toHaveValue('Run the tests and report only what failed.');

  // A click outside asks too; Discard closes.
  await page.mouse.click(5, 5);
  await expect(question).toBeVisible();
  await question.getByRole('button', { name: 'Discard' }).click();
  await expect(dialog).toHaveCount(0);

  // Editing an automation whose pane has closed: named as such, Save blocked.
  // The ⋯ button shows on hover only; right-click opens the same menu.
  await rows.first().click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Edit…' }).click();
  const editing = page.getByRole('dialog', { name: 'Edit automation' });
  await expect(editing).toBeVisible();
  await expect(editing.getByRole('combobox', { name: 'Target pane' })).toHaveValue('pane-that-closed');
  await expect(editing.getByRole('alert')).toContainText('The pane this automation types into is closed.');
  await expect(editing.getByRole('button', { name: 'Save' })).toBeDisabled();
  await page.keyboard.press('Escape');
  await expect(editing).toHaveCount(0);
});
