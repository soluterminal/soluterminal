import type { BrowserContext, Page, Route } from '@playwright/test';
import {
  CREDENTIALS_KEYS_HEALTH_FIXTURE,
  CREDENTIALS_KEYS_LOCAL_ONLY_MESSAGE,
  installCredentialsKeysState,
} from '../tools/capture-credentials-keys.mjs';
import { expect, test } from './fixtures';

const machineID = 'machine-credentials';
const userID = 'user-credentials';
const supabaseOrigin = 'https://fixture.supabase.co';

const user = {
  id: userID,
  aud: 'authenticated',
  role: 'authenticated',
  email: 'operator@example.test',
  app_metadata: {},
  user_metadata: {},
  created_at: '2026-09-03T00:00:00.000Z',
};

const session = {
  access_token: 'fixture-access-token',
  refresh_token: 'fixture-refresh-token',
  token_type: 'bearer',
  expires_in: 31_536_000,
  expires_at: 4_102_444_800,
  user,
};

function json(route: Route, value: unknown) {
  return route.fulfill({
    status: 200,
    contentType: 'application/json; charset=utf-8',
    body: JSON.stringify(value),
  });
}

async function installCloudCredentialsScenario(
  context: BrowserContext,
  page: Page,
  { isLocal }: { isLocal: boolean },
) {
  await page.addInitScript(installCredentialsKeysState, 'dark');
  await page.addInitScript(({ storedSession, selectedMachineID }) => {
    localStorage.setItem('sb-fixture-auth-token', JSON.stringify(storedSession));
    if (selectedMachineID) localStorage.setItem('solu.selectedMachineId', selectedMachineID);
  }, { storedSession: session, selectedMachineID: isLocal ? '' : machineID });

  await context.route('**/api/config', (route) => json(route, {
    supabaseUrl: supabaseOrigin,
    supabaseAnonKey: 'fixture-anon-key',
    isLocal,
  }));
  await context.route(`${supabaseOrigin}/**`, (route) => {
    const pathname = new URL(route.request().url()).pathname;
    if (pathname.startsWith('/rest/v1/users')) return json(route, { id: userID });
    if (pathname === '/auth/v1/user') return json(route, user);
    if (pathname === '/auth/v1/token') return json(route, session);
    return json(route, {});
  });
  await context.route('**/api/user/prefs', (route) => json(route, {
    prefs: { sidebar: { hidden: [] } },
    updated_at: '2026-09-03T00:00:00.000Z',
  }));
  await context.route('**/api/machines', (route) => json(route, [{
    id: machineID,
    name: 'Remote fixture machine',
    online: true,
    backends: ['host'],
  }]));

  if (!isLocal) {
    await context.route(`**/m/${machineID}/api/**`, (route) => {
      const requestURL = new URL(route.request().url());
      requestURL.pathname = requestURL.pathname.replace(`/m/${machineID}`, '');
      return route.continue({ url: requestURL.toString() });
    });
  }
  await context.route('**/api/workflow/runtimes/health', (route) => (
    json(route, CREDENTIALS_KEYS_HEALTH_FIXTURE)
  ));
  // Sidebar preferences now belong to the viewed daemon (#849). A signed-in
  // scenario must seed that user's daemon prefs instead of relying on the
  // anonymous legacy localStorage entry installed by the capture helper.
  await context.route('**/api/health', async route => {
    const response = await route.fetch({ url: new URL('/api/health', route.request().url()).toString() });
    const health = await response.json();
    return json(route, { ...health, features: [...new Set([...(health.features ?? []), 'ui-prefs.v1'])] });
  });
  await context.route('**/api/settings/ui', route => json(route, {
    prefs: { sidebar: { hidden: [], open: ['credentials'], workspacesCollapsed: true } },
  }));
}

function deepSeekRow(page: Page) {
  const section = page.locator('#sidebar [data-section-key="credentials"]');
  return {
    section,
    row: section.locator('.cred-row', { hasText: 'DeepSeek' }),
  };
}

function geminiRow(page: Page) {
  const section = page.locator('#sidebar [data-section-key="credentials"]');
  return {
    section,
    row: section.locator('.cred-row', { hasText: 'Gemini' }),
  };
}

test('Gemini signed-out readiness and key mutation stay bound to gemini-headless', async ({ context, page }) => {
  await installCloudCredentialsScenario(context, page, { isLocal: true });
  const health = structuredClone(CREDENTIALS_KEYS_HEALTH_FIXTURE);
  const gemini = health.runtimes.find((runtime) => runtime.id === 'gemini-headless');
  if (!gemini) throw new Error('Gemini fixture row missing');
  gemini.signedIn = false;
  gemini.status = 'signed_out';
  await context.route('**/api/workflow/runtimes/health', (route) => json(route, health));
  const mutations: Array<{ method: string; body: unknown }> = [];
  await context.route('**/api/workflow/runtimes/gemini-headless/secrets/GEMINI_API_KEY', async (route) => {
    const method = route.request().method();
    mutations.push({
      method,
      body: method === 'PUT' ? route.request().postDataJSON() : null,
    });
    gemini.signedIn = method === 'PUT';
    gemini.status = method === 'PUT' ? 'unknown' : 'signed_out';
    gemini.secrets[0]!.configured = method === 'PUT';
    await route.fulfill({ status: 204, body: '' });
  });
  await page.goto('/');

  const { section, row } = geminiRow(page);
  await expect(section).toBeVisible();
  await expect(row).toContainText('Not connected — sign in or add an API key');
  await row.getByRole('button', { name: 'Add key' }).click();
  await page.getByLabel('Gemini GEMINI_API_KEY API key').fill('gemini-browser-key');
  await row.getByRole('button', { name: 'Save' }).click();
  await expect.poll(() => mutations).toEqual([{
    method: 'PUT',
    body: { value: 'gemini-browser-key' },
  }]);
  await row.getByRole('button', { name: 'Remove' }).click();
  await row.locator('.cred-inline-confirm').getByRole('button', { name: 'Remove' }).click();
  await expect.poll(() => mutations).toEqual([
    { method: 'PUT', body: { value: 'gemini-browser-key' } },
    { method: 'DELETE', body: null },
  ]);
});

function claudeAccount(name: string, active: boolean) {
  return {
    name,
    active,
    subscriptionType: 'Max',
    usage: {
      fiveH: { utilPct: 20, resetEpoch: 0, resetInLabel: '', pacePct: 0 },
      sevenD: { utilPct: 30, resetEpoch: 0, resetInLabel: '', pacePct: 0 },
      fetchedAtEpoch: 0,
      stale: false,
      available: true,
    },
    error: '',
  };
}

async function installClaudePinScenario(
  context: BrowserContext,
  page: Page,
  { pinned = '', activationStatus = 200, delayActivation = false } = {},
) {
  await page.addInitScript(installCredentialsKeysState, 'dark');
  let active = 'northwind';
  let currentPinned = pinned;
  const events: string[] = [];
  const pinUpdates: string[] = [];
  let releaseActivation = () => {};
  const activationGate = delayActivation
    ? new Promise<void>((resolve) => { releaseActivation = resolve; })
    : Promise.resolve();
  let noteActivationStarted = () => {};
  const activationStarted = new Promise<void>((resolve) => { noteActivationStarted = resolve; });
  const accountsResponse = () => ({
    active,
    machineWide: true,
    currentUnsaved: false,
    accounts: [claudeAccount('northwind', active === 'northwind'), claudeAccount('work-acct', active === 'work-acct')],
  });
  const autoSwitchResponse = () => ({
    enabled: true,
    pinned: currentPinned,
    switchThresholdPct: 85,
    softWarnPct: 90,
    pool: { northwind: true, 'work-acct': true },
    state: currentPinned ? 'pinned' : 'ok',
    statusLine: currentPinned ? `Pinned to ${currentPinned}` : 'Auto',
  });

  await context.route('**/api/claude/accounts', (route) => json(route, accountsResponse()));
  await context.route('**/api/claude/accounts/*/activate', async (route) => {
    events.push('activate');
    noteActivationStarted();
    await activationGate;
    if (activationStatus !== 200) {
      return route.fulfill({
        status: activationStatus,
        contentType: 'application/json; charset=utf-8',
        body: JSON.stringify({ error: 'target credentials unavailable' }),
      });
    }
    active = decodeURIComponent(new URL(route.request().url()).pathname.split('/').at(-2) ?? '');
    return json(route, { active });
  });
  await context.route('**/api/claude/autoswitch', (route) => {
    if (route.request().method() === 'PUT') {
      const body = route.request().postDataJSON() as { pinned?: string };
      events.push('pin');
      currentPinned = body.pinned ?? currentPinned;
      pinUpdates.push(currentPinned);
    }
    return json(route, autoSwitchResponse());
  });

  return { activationStarted, events, pinUpdates, releaseActivation };
}

test('cloud sign-in against the local daemon keeps the DeepSeek key editor available', async ({ context, page }) => {
  await installCloudCredentialsScenario(context, page, { isLocal: true });
  await page.goto('/');

  const { section, row } = deepSeekRow(page);
  await expect(section).toBeVisible();
  await expect(row.getByRole('button', { name: 'Add key' })).toBeVisible();
  await expect(row.locator('.cred-note')).toHaveCount(0);
  await row.getByRole('button', { name: 'Add key' }).click();
  await expect(page.getByLabel('DeepSeek DEEPSEEK_API_KEY API key')).toBeVisible();
});

test('cloud relay session replaces the DeepSeek key editor with local-only guidance', async ({ context, page }) => {
  await installCloudCredentialsScenario(context, page, { isLocal: false });
  await page.goto('/');

  const { section, row } = deepSeekRow(page);
  await expect(section).toBeVisible();
  await expect(row.locator('.cred-note')).toHaveText(CREDENTIALS_KEYS_LOCAL_ONLY_MESSAGE);
  await expect(row.getByRole('button', { name: 'Add key' })).toHaveCount(0);
  await expect(row.getByRole('button', { name: 'Replace key' })).toHaveCount(0);
  await expect(row.getByRole('button', { name: 'Remove' })).toHaveCount(0);
  await expect(page.getByLabel('DeepSeek DEEPSEEK_API_KEY API key')).toHaveCount(0);
});

test('pinning an inactive Claude account waits for activation before persisting the pin', async ({ context, page }) => {
  const scenario = await installClaudePinScenario(context, page, { delayActivation: true });
  await page.goto('/');

  const row = page.locator('#sidebar [data-section-key="credentials"] .cred-row', { hasText: 'work-acct' });
  await expect(row).toBeVisible();
  await row.hover();
  const pin = row.getByRole('button', { name: 'Pin and switch to account' });
  await expect(pin).toBeVisible();
  await pin.click();
  await scenario.activationStarted;
  expect(scenario.pinUpdates).toEqual([]);

  scenario.releaseActivation();
  await expect.poll(() => scenario.pinUpdates).toEqual(['work-acct']);
  expect(scenario.events).toEqual(['activate', 'pin']);
  await expect(row.getByRole('button', { name: 'Unpin account' })).toHaveAttribute('title', 'Pinned — click to unpin');
  await expect(page.getByText('Pinned & switched to "work-acct"')).toBeVisible();
});

test('a rejected Claude activation leaves the inactive account unpinned', async ({ context, page }) => {
  const scenario = await installClaudePinScenario(context, page, { activationStatus: 409 });
  await page.goto('/');

  const row = page.locator('#sidebar [data-section-key="credentials"] .cred-row', { hasText: 'work-acct' });
  await expect(row).toBeVisible();
  await row.hover();
  const pin = row.getByRole('button', { name: 'Pin and switch to account' });
  await pin.click();

  await expect(page.getByText(/Pin failed because the account couldn't switch:/)).toBeVisible();
  expect(scenario.pinUpdates).toEqual([]);
  await expect(pin).not.toHaveClass(/is-pinned/);
});

test('repeated pin clicks during a delayed rejected activation cannot persist the pin', async ({ context, page }) => {
  const scenario = await installClaudePinScenario(context, page, {
    activationStatus: 409,
    delayActivation: true,
  });
  await page.goto('/');

  const section = page.locator('#sidebar [data-section-key="credentials"]');
  const original = section.locator('.cred-row', { hasText: 'northwind' });
  const row = section.locator('.cred-row', { hasText: 'work-acct' });
  await row.hover();
  const pin = row.getByRole('button', { name: 'Pin and switch to account' });
  await pin.click();
  await scenario.activationStarted;
  await expect(row).toHaveClass(/is-active/);

  await pin.click();
  expect(scenario.pinUpdates).toEqual([]);
  scenario.releaseActivation();

  await expect(page.getByText(/Pin failed because the account couldn't switch:/)).toBeVisible();
  await expect(original).toHaveClass(/is-active/);
  await expect(row).not.toHaveClass(/is-active/);
  await expect(pin).not.toHaveClass(/is-pinned/);
  expect(scenario.pinUpdates).toEqual([]);
});

test('a pinned-but-inactive Claude row repairs activation without changing the pin', async ({ context, page }) => {
  const scenario = await installClaudePinScenario(context, page, { pinned: 'work-acct' });
  await page.goto('/');

  const row = page.locator('#sidebar [data-section-key="credentials"] .cred-row', { hasText: 'work-acct' });
  await expect(row).toBeVisible();
  const repair = row.getByRole('button', { name: 'Switch to pinned account' });
  await expect(repair).toBeVisible();
  await expect(repair).toHaveAttribute('title', 'Pinned, but not active — click to finish switching');
  await repair.click();

  await expect.poll(() => scenario.events).toEqual(['activate']);
  expect(scenario.pinUpdates).toEqual([]);
  await expect(row.getByRole('button', { name: 'Unpin account' })).toHaveClass(/is-pinned/);
  await expect(page.getByText('Switched to pinned account "work-acct"')).toBeVisible();
});

test('the DeepSeek Harness row runs the free Check and the paid Test Connection', async ({ context, page }) => {
  await installCloudCredentialsScenario(context, page, { isLocal: true });
  let authStatus = 'not-tested';
  let checked = false, tested = false;
  const harness = () => ({
    installed: true, configured: true, keyPresent: true, authenticationStatus: authStatus,
    lastTest: authStatus === 'authenticated' ? { status: 'authenticated', at: 1, model: 'deepseek-flash' } : null,
    accounts: [{ name: 'main', source: 'personal', selected: true, billing: 'api', model: 'deepseek-flash' }],
  });
  await context.route('**/api/harnesses/glm', (route) => json(route, { installed: false, configured: false, keyPresent: false, authenticationStatus: 'no-account', accounts: [] }));
  await context.route('**/api/harnesses/deepseek', (route) => json(route, harness()));
  await context.route('**/api/harnesses/deepseek/check', (route) => {
    checked = true; authStatus = 'authenticated';
    return json(route, { status: 'authenticated', httpStatus: 200, at: 1, model: 'deepseek-flash' });
  });
  await context.route('**/api/harnesses/deepseek/test', (route) => {
    tested = true; authStatus = 'authenticated';
    return json(route, { status: 'authenticated', httpStatus: 200, at: 1, model: 'deepseek-flash' });
  });

  await page.goto('/');
  const section = page.locator('[aria-label="DeepSeek Harness credentials"]');
  await expect(section).toBeVisible();
  // A saved key is present but unauthenticated until an explicit check.
  await expect(section.getByText('not tested')).toBeVisible();
  await expect(section.getByRole('button', { name: 'Run' })).toBeVisible();
  // The free, read-only Check verifies the key before any paid request.
  await section.getByRole('button', { name: 'Check' }).click();
  await expect.poll(() => checked).toBe(true);
  await expect(section.getByText('key works')).toBeVisible();
  await section.getByRole('button', { name: 'Test' }).click();
  await expect.poll(() => tested).toBe(true);
  await expect(section.getByText('key works')).toBeVisible();
});
