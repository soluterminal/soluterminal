import type { Page } from 'playwright/test';
import { expect, test } from './fixtures';
import { DEEPSEEK_PRICING_CAPTURE_FIXTURE, installCredentialsKeysState } from '../tools/capture-credentials-keys.mjs';
import { dismissDaemonBanners } from '../tools/screens-lib.mjs';

// All provider/account responses are fixtures. No key, paid request, or live
// account is used, including when testing unavailable balances and retries.
async function openPricing(page: Page, theme: string, overrides: Record<string, unknown> = {}) {
  await page.addInitScript(installCredentialsKeysState, theme);
  await page.clock.setFixedTime(new Date(DEEPSEEK_PRICING_CAPTURE_FIXTURE.generatedAt));
  let requests = 0;
  let unavailable = false;
  const report = { ...DEEPSEEK_PRICING_CAPTURE_FIXTURE, ...overrides };
  const responses: Record<string, unknown> = {
    '/api/harnesses/deepseek': { installed: true, authenticationStatus: 'not-tested', accounts: [
      { name: 'fixture-only', source: 'personal', selected: true, billing: 'api', model: report.requestedModel },
    ] },
    '/api/harnesses/deepseek/usage': { importedAt: 0, sources: [], rowCount: 0, currency: 'CNY', costTotal: 0, keys: [] },
    '/api/harnesses/deepseek/balance': { status: 'unavailable', available: false, balances: [] },
    '/api/harnesses/deepseek/team': { available: false, members: [] },
  };
  await page.route('**/api/harnesses/**', async route => {
    const url = new URL(route.request().url());
    expect(route.request().method()).toBe('GET');
    if (url.pathname === '/api/harnesses/deepseek/pricing') {
      requests++;
      return route.fulfill(unavailable ? { status: 503, body: 'Fixture daemon unavailable' } : {
        contentType: 'application/json', body: JSON.stringify(report),
      });
    }
    return route.fulfill({ contentType: 'application/json', body: JSON.stringify(
      responses[url.pathname] ?? { installed: true, authenticationStatus: 'no-account', accounts: [] },
    ) });
  });
  await page.goto('/');
  await dismissDaemonBanners(page);
  const harness = page.getByLabel('DeepSeek Harness credentials');
  await harness.getByRole('button', { name: 'Toggle DeepSeek usage and spend' }).click();
  const badge = page.getByRole('button', { name: /^DeepSeek pricing:/ });
  await expect(badge).not.toContainText('loading');
  return { badge, requests: () => requests, fail: (value: boolean) => { unavailable = value; } };
}

test.use({ timezoneId: 'Asia/Singapore' });

for (const theme of ['dark', 'light']) {
  test(`pricing stays usable without balance or CSV (${theme})`, async ({ page }) => {
    const { badge, requests } = await openPricing(page, theme);
    await expect(badge).toContainText('Off-peak · 50% cheaper');
    await expect(badge).toContainText('34 min');
    await expect(badge).toContainText('SGT');
    await expect(page.getByText('Balance: unavailable', { exact: true })).toBeVisible();
    await badge.hover();
    const details = page.getByRole('dialog', { name: 'DeepSeek pricing details' });
    await expect(details).toBeVisible();
    await details.hover();
    await expect(details.getByLabel('Current rates')).toContainText('CNY 0.02');
    await expect(details.getByLabel('Current rates')).toContainText('CNY 4');
    await expect(details.getByRole('checkbox')).not.toBeChecked();
    const count = requests();
    await page.waitForTimeout(2_100); // More than two display ticks; no pricing polling.
    expect(requests()).toBe(count);
    await page.keyboard.press('Escape');
    await expect(details).toBeHidden();
    await expect(badge).toBeFocused();
    await badge.press('Enter');
    await expect(details).toBeVisible();
    await page.setViewportSize({ width: 420, height: 900 });
    const bounds = await details.boundingBox();
    expect(bounds).not.toBeNull();
    expect(bounds!.x).toBeGreaterThanOrEqual(0);
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(420);
    expect(await details.evaluate(el => el.scrollWidth <= el.clientWidth + 1)).toBe(true);
    await details.getByRole('button', { name: 'Close DeepSeek pricing details' }).click();
    await expect(details).toBeHidden();
  });
}

test('a dismissed popover stays closed when it uncovers its badge under the pointer', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 560 });
  const { badge } = await openPricing(page, 'dark');
  const details = page.getByRole('dialog', { name: 'DeepSeek pricing details' });
  await badge.click();
  await expect(details).toBeVisible();
  const target = (await badge.boundingBox())!;
  const panel = (await details.boundingBox())!;
  const x = target.x + target.width / 2;
  const y = target.y + target.height / 2;
  // The short viewport clamps the popover over its own badge.
  expect(x > panel.x && x < panel.x + panel.width && y > panel.y && y < panel.y + panel.height).toBe(true);
  await page.mouse.move(x, y);
  await page.keyboard.press('Escape');
  await expect(details).toBeHidden();
  // Give the browser frames to re-hit-test the still pointer.
  await page.waitForTimeout(250);
  await expect(details).toBeHidden();
  await expect(badge).toHaveAttribute('aria-expanded', 'false');
  // Moving onto the badge is a real hover and still opens the details.
  await page.mouse.move(x + 4, y);
  await expect(details).toBeHidden();
  await page.mouse.move(target.x + target.width + 40, y - 80);
  await page.mouse.move(x, y);
  await expect(details).toBeVisible();
});

test('a failed wake refresh withdraws stale discounts and recovers', async ({ page }) => {
  const fixture = await openPricing(page, 'dark');
  await expect(fixture.badge).toContainText('50% cheaper');
  fixture.fail(true);
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await expect(fixture.badge).toHaveAccessibleName('DeepSeek pricing: Pricing unavailable');
  await fixture.badge.click();
  const details = page.getByRole('dialog', { name: 'DeepSeek pricing details' });
  await expect(details.getByLabel('Current rates')).toHaveCount(0);
  await expect(details.getByRole('checkbox')).toHaveCount(0);
  fixture.fail(false);
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await expect(fixture.badge).toContainText('50% cheaper');
});

for (const [name, overrides, copy] of [
  ['unknown model', { requestedModel: 'future-model', canonicalModel: '', priced: false, currentRates: undefined }, 'Pricing unavailable for selected model'],
  ['expired calendar', { state: 'schedule-only', holidayVerified: false, currentRates: undefined, nextChange: undefined, savingsPercent: undefined }, 'Schedule only · holidays unverified'],
] as const) {
  test(`${name} cannot advertise a current discount`, async ({ page }) => {
    const { badge } = await openPricing(page, 'light', overrides);
    await expect(badge).toContainText(copy);
    await expect(badge).not.toContainText('50% cheaper');
    await badge.click();
    await expect(page.getByRole('dialog', { name: 'DeepSeek pricing details' }).getByLabel('Current rates')).toHaveCount(0);
  });
}
