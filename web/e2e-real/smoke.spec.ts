import { test, expect } from 'playwright/test';

test('web bundle loads its pane list from the launched solud', async ({ page, request }) => {
  const identity = await request.get('/api/health?identity=1');
  expect(identity.ok()).toBeTruthy();
  expect(await identity.json()).toMatchObject({ status: 'ok', version: 'dev', pid: Number(process.env.SOLU_REAL_E2E_PID) });

  // Only suppress onboarding overlays; all API and websocket traffic is real.
  await page.addInitScript(() => {
    localStorage.setItem('solu.seenWelcome', '1');
    localStorage.setItem('solu.seenChecklist', '1');
  });
  const loaded = page.waitForResponse(response => new URL(response.url()).pathname === '/api/panes' && response.request().method() === 'GET');
  await page.goto('/');
  const panes = await loaded;
  expect(panes.ok()).toBeTruthy();
  expect(await panes.json()).toEqual([]);
  // Fresh HOME also asks about global agent integration. Decline through the
  // actual UI before opening a shell; never register personal agent settings.
  await page.getByRole('button', { name: 'Not now', exact: true }).click();
  const [response] = await Promise.all([
    page.waitForResponse(response => new URL(response.url()).pathname === '/api/panes' && response.request().method() === 'POST'),
    page.getByRole('button', { name: 'Open a terminal', exact: true }).click(),
  ]);
  // Same-host Linux can trace Playwright. Preserve the daemon's refusal;
  // automation is not proof of a human caller, even through the actual UI.
  expect(response.status()).toBe(403);
  expect(await response.json()).toMatchObject({ error: 'human_required', caller: 'unverified' });
  expect(await (await request.get('/api/panes')).json()).toEqual([]);
  await expect(page.getByText('No sessions here yet', { exact: true })).toBeVisible();
});
