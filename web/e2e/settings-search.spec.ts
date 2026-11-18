import { devices } from 'playwright/test';
import { expect, test } from './fixtures';

// ST-281: search in Settings, desktop and phone. Typing finds the row, the
// count is announced, and Enter opens its tab with focus on the row.

test('desktop: "/" focuses search, Enter lands on the row', async ({ page }) => {
  await page.goto('/');
  await page.locator('.topbar button[aria-label="Settings"]').click();
  const dialog = page.locator('.settings-modal[role="dialog"]');
  await expect(dialog).toBeVisible();
  await dialog.getByRole('tab', { name: 'Terminal & panes' }).click();

  await page.locator('body').press('/');
  const box = dialog.getByRole('combobox', { name: 'Search settings' });
  await expect(box).toBeFocused();
  await box.fill('font');
  await expect(dialog.getByRole('status')).toContainText(/settings? found/);
  const results = dialog.getByRole('listbox', { name: 'Matching settings' });
  await expect(results.getByRole('option').first()).toContainText('Font size');
  await expect(results.getByRole('option').first()).toContainText('Appearance › Text size');
  await box.press('Enter');

  await expect(dialog.getByRole('tab', { selected: true })).toHaveText('Appearance');
  await expect(dialog.getByRole('spinbutton', { name: 'Font size' })).toBeFocused();
  await expect(box).toHaveValue('');

  // Nothing matches: said once, no list.
  await box.fill('zzqx');
  await expect(dialog.getByRole('status')).toHaveText('No settings match');
  await expect(results).toHaveCount(0);
  // Esc clears the search first, then closes Settings.
  await box.press('Escape');
  await expect(box).toHaveValue('');
  await expect(dialog).toBeVisible();
});

// A phone context (touch, mobile UA, phone viewport) without switching browser.
const { defaultBrowserType: _browser, ...phone } = devices['Pixel 7'];

test.describe('phone', () => {
  test.use(phone);

  test('search fits the phone and lands on a phone row', async ({ page }) => {
    await page.goto('/');
    // A phone opens Settings from the command palette's "Settings: <tab>".
    await page.locator('button[title="Command palette"]').click();
    const palette = page.getByRole('dialog', { name: 'Command palette' });
    await palette.getByRole('textbox').fill('Settings: Terminal');
    await palette.getByText('Settings: Terminal & panes', { exact: true }).click();
    const dialog = page.locator('.settings-modal[role="dialog"]');
    await expect(dialog.getByRole('tab', { selected: true })).toHaveText('Terminal & panes');
    await expect(dialog).toBeVisible();
    const box = dialog.getByRole('combobox', { name: 'Search settings' });
    await box.fill('line height');
    const option = dialog.getByRole('listbox', { name: 'Matching settings' }).getByRole('option').first();
    await expect(option).toContainText('Line height');
    // No sideways scroll at phone width.
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await option.click();
    await expect(dialog.getByRole('tab', { selected: true })).toHaveText('Appearance');
    await expect(dialog.getByRole('spinbutton', { name: 'Line height' })).toBeFocused();
  });
});
