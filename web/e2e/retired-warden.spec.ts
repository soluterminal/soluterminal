import { expect, test } from './fixtures';

test('retired caretaker destination opens Appearance without Warden calls or controls', async ({ page }) => {
  const calls: string[] = [];
  page.on('request', (request) => {
    if (new URL(request.url()).pathname.startsWith('/api/warden')) calls.push(request.url());
  });
  await page.addInitScript(() => {
    localStorage.setItem('solu.settings.lastCategory', 'agents');
    localStorage.setItem('solu.settings.lastSection', 'agents.warden');
  });
  await page.goto('/');
  await page.locator('.topbar button[aria-label="Settings"]').click();
  const dialog = page.getByRole('dialog', { name: 'Settings' });
  await expect(dialog.getByRole('tab', { name: 'Appearance', exact: true })).toHaveAttribute('aria-selected', 'true');
  await expect(dialog.getByRole('tab', { name: 'Warden' })).toHaveCount(0);
  await expect(dialog.getByRole('checkbox', { name: 'Automatic pane flags' })).toHaveCount(0);
  expect(calls).toEqual([]);
});
