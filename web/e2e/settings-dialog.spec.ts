import { readFile } from 'node:fs/promises';
import { readFileSync } from 'node:fs';
import type { BrowserContext, Page, Route } from 'playwright/test';
import type { WorkflowRetentionReport, WorkflowSettings } from '../src/api';
import {
  defaultSelection,
  renderSupportBundle,
  type SupportBundleResponse,
} from '../src/lib/supportBundle';
import { expect, test } from './fixtures';

// Playwright loads this test as native ESM, unlike Vite's JSON transformer.
const supportBundleFixture = JSON.parse(readFileSync(
  new URL('../testdata/support-bundle-2026-09-06.json', import.meta.url), 'utf8',
)) as SupportBundleResponse;

type SupportBundleRouteState = 'loaded' | 'empty' | 'remote' | 'fail-once';

async function installSupportBundleRoute(context: BrowserContext, state: SupportBundleRouteState) {
  let requests = 0;
  await context.route('**/api/support/bundle*', async (route: Route) => {
    requests += 1;
    if (state === 'remote') {
      await route.fulfill({ status: 403, contentType: 'text/plain', body: 'support bundles are local-only' });
      return;
    }
    if (state === 'fail-once' && requests === 1) {
      await route.fulfill({ status: 500, contentType: 'text/plain', body: 'fixture collection failed' });
      return;
    }
    const body = state === 'empty'
      ? {
          ...supportBundleFixture,
          sections: supportBundleFixture.sections.map((section) => ({
            ...section,
            text: '',
            bytes: 0,
            lines: 0,
            empty: true,
            note: 'No fixture data is available for this source.',
          })),
        }
      : supportBundleFixture;
    await route.fulfill({
      status: 200,
      contentType: 'application/json; charset=utf-8',
      body: JSON.stringify(body),
    });
  });
  return () => requests;
}

async function openSupportBundleFromPalette(page: Page) {
  await page.keyboard.press('Control+K');
  const palette = page.getByRole('dialog', { name: 'Command palette' });
  await expect(palette).toBeVisible();
  await palette.getByText('Create support bundle…', { exact: true }).click();
  const dialog = page.getByRole('dialog', { name: 'Create support bundle' });
  await expect(dialog).toBeVisible();
  return dialog;
}

async function openSupportBundleFromHelp(page: Page) {
  await page.locator('.topbar').getByRole('button', { name: 'Help' }).click();
  await page.getByRole('menuitem', { name: 'Create support bundle…' }).click();
  const dialog = page.getByRole('dialog', { name: 'Create support bundle' });
  await expect(dialog).toBeVisible();
  return dialog;
}

test('settings opens, switches a tab, and closes', async ({ page }) => {
  await page.goto('/');
  const trigger = page.locator('.topbar button[aria-label="Settings"]');
  await expect(trigger).toHaveCount(1);
  await trigger.click();
  const dialog = page.locator('.settings-modal[role="dialog"]');
  await expect(dialog).toBeVisible();
  const tabs = dialog.locator('[role="tab"]');
  await expect(tabs.first()).toBeVisible();
  await tabs.last().click();
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
});

test('support bundle command opens preview and deselection controls the browser download', async ({ context, page }) => {
  const requestCount = await installSupportBundleRoute(context, 'loaded');
  await page.goto('/');
  await expect(page.locator('.app .topbar')).toBeVisible();
  const dialog = await openSupportBundleFromPalette(page);

  const versions = dialog.locator('section', { hasText: 'Versions and runtime' });
  await expect(versions).toBeVisible();
  const close = dialog.getByRole('button', { name: 'Close' });
  const save = dialog.getByRole('button', { name: 'Save bundle' });
  await expect(close).toBeFocused();
  await save.focus();
  await page.keyboard.press('Tab');
  await expect(close).toBeFocused();
  await page.keyboard.press('Shift+Tab');
  await expect(save).toBeFocused();

  await versions.getByRole('button', { name: 'Preview' }).click();
  await expect(versions.locator('pre')).toHaveText(supportBundleFixture.sections[0].text);

  const logs = dialog.locator('section', { hasText: 'Recent daemon logs' });
  await logs.getByRole('checkbox').uncheck();
  const paneText = dialog.locator('section', { hasText: 'Focused pane tail' });
  await expect(paneText.getByRole('checkbox')).not.toBeChecked();

  const downloadPromise = page.waitForEvent('download');
  await dialog.getByRole('button', { name: 'Save bundle' }).click();
  const download = await downloadPromise;
  expect(download.suggestedFilename()).toBe(supportBundleFixture.filename);
  const downloadPath = await download.path();
  expect(downloadPath).not.toBeNull();
  const saved = await readFile(downloadPath!, 'utf8');
  const selected = defaultSelection(supportBundleFixture);
  selected.delete('logs');
  expect(saved).toBe(renderSupportBundle(supportBundleFixture, selected));
  expect(saved).not.toContain(supportBundleFixture.sections.find((section) => section.id === 'logs')!.text);
  expect(saved).not.toContain(supportBundleFixture.sections.find((section) => section.id === 'paneText')!.text);
  expect(requestCount()).toBe(1);
});

test('support bundle Help entry cancels through button and Escape without a download', async ({ context, page }) => {
  const requestCount = await installSupportBundleRoute(context, 'loaded');
  let downloads = 0;
  page.on('download', () => { downloads += 1; });
  await page.goto('/');
  await expect(page.locator('.app .topbar')).toBeVisible();

  let dialog = await openSupportBundleFromHelp(page);
  await expect(dialog.getByText('Versions and runtime', { exact: true })).toBeVisible();
  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).not.toBeVisible();

  dialog = await openSupportBundleFromHelp(page);
  await expect(dialog.getByText('Versions and runtime', { exact: true })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(dialog).not.toBeVisible();
  expect(requestCount()).toBe(2);
  expect(downloads).toBe(0);
});

test('support bundle collection failure hides Save and Retry performs a fresh request', async ({ context, page }) => {
  const requestCount = await installSupportBundleRoute(context, 'fail-once');
  await page.goto('/');
  await expect(page.locator('.app .topbar')).toBeVisible();
  const dialog = await openSupportBundleFromPalette(page);

  await expect(dialog.getByText('fixture collection failed', { exact: true })).toBeVisible();
  await expect(dialog.getByRole('button', { name: 'Save bundle' })).toHaveCount(0);
  await dialog.getByRole('button', { name: 'Retry' }).click();
  await expect(dialog.getByText('Versions and runtime', { exact: true })).toBeVisible();
  await expect(dialog.getByRole('button', { name: 'Save bundle' })).toBeEnabled();
  expect(requestCount()).toBe(2);
});

test('support bundle relay rejection explains local-only behavior without Save', async ({ context, page }) => {
  const requestCount = await installSupportBundleRoute(context, 'remote');
  await page.goto('/');
  await expect(page.locator('.app .topbar')).toBeVisible();
  const dialog = await openSupportBundleFromPalette(page);

  await expect(dialog.getByText(/Diagnostics never travel over the cloud relay/)).toBeVisible();
  await expect(dialog.getByRole('button', { name: 'Save bundle' })).toHaveCount(0);
  expect(requestCount()).toBe(1);
});

test('support bundle empty state disables Save and does not download', async ({ context, page }) => {
  const requestCount = await installSupportBundleRoute(context, 'empty');
  let downloads = 0;
  page.on('download', () => { downloads += 1; });
  await page.goto('/');
  await expect(page.locator('.app .topbar')).toBeVisible();
  const dialog = await openSupportBundleFromPalette(page);

  await expect(dialog.getByText(/No diagnostic data is available yet/)).toBeVisible();
  await expect(dialog.getByRole('button', { name: 'Save bundle' })).toBeDisabled();
  expect(requestCount()).toBe(1);
  expect(downloads).toBe(0);
});

test('appearance text sizes can be typed over the current value', async ({ page }) => {
  await page.goto('/');
  await page.locator('.topbar button[aria-label="Settings"]').click();
  const dialog = page.locator('.settings-modal[role="dialog"]');
  await expect(dialog).toBeVisible();
  await dialog.getByRole('tab', { name: 'Appearance' }).click();

  // Before the fix every keystroke was clamped: "1" became 10 and "18" became 22.
  const fontSize = dialog.getByRole('spinbutton', { name: 'Font size' });
  await fontSize.fill('');
  await fontSize.pressSequentially('18');
  await expect(fontSize).toHaveValue('18');
  await fontSize.blur();
  await expect(fontSize).toHaveValue('18');
  await fontSize.press('Backspace');
  await fontSize.press('Backspace');
  await expect(fontSize).toHaveValue('');
  await fontSize.pressSequentially('16');
  await fontSize.press('Enter');
  await expect(fontSize).toHaveValue('16');

  // "0" used to fall back to the old value, so 0.9 could never be typed.
  const sidebarSize = dialog.getByRole('spinbutton', { name: 'Sidebar text size' });
  await sidebarSize.fill('');
  await sidebarSize.pressSequentially('0.9');
  await sidebarSize.blur();
  await expect(sidebarSize).toHaveValue('0.9');
  expect(await page.evaluate(() => localStorage.getItem('solu.uiScale'))).toBe('0.9');
  expect(await page.evaluate(() => localStorage.getItem('solu.terminalFontSize'))).toBe('16');
});
