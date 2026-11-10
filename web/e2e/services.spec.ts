import { test, expect } from './fixtures';

test('workspace services filter, preview, shared preferences and stale recovery', async ({
  page,
}, info) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('console', (message) => {
    if (message.type() === 'error' && /same key/.test(message.text())) errors.push(message.text());
  });
  const workspace = {
    name: 'Services acceptance',
    paneIds: [],
    layout: null,
    activeAt: '2026-10-24T00:00:00Z',
  };
  const base = {
    addr: '127.0.0.1',
    pid: 42,
    process: 'node',
    cmdline: 'node vite',
    cwd: '/home/developer/apps/dashboard',
    firstAt: '2026-10-24T00:00:00Z',
    proto: 'tcp',
    scheme: 'http',
    webStatus: 'ready',
    workspace: workspace.name,
    paneId: 'web-pane',
    paneTitle: 'Dashboard',
  };
  const ports = [
    { ...base, port: 5173, backend: 'win' as const, previewPath: '/api/proxy/5173~b2xk/' },
    { ...base, port: 7000, workspace: 'Other project', paneTitle: 'Other app' },
    {
      ...base,
      port: 5432,
      process: 'postgres',
      webStatus: 'internal',
      scheme: undefined,
      paneTitle: 'Database',
    },
    {
      ...base,
      port: 8000,
      paneId: undefined,
      workspace: undefined,
      paneTitle: 'External app',
      webStatus: 'unverified',
      scheme: undefined,
      previewPath: '/api/proxy/8000~ZXh0ZXJuYWw/',
    },
  ];
  let offline = false;
  let checks = 0;
  await page.addInitScript(() => {
    localStorage.setItem('solu.activeWorkspace', 'Services acceptance');
    localStorage.setItem('solu.sidebarOpenMiddle.v2', JSON.stringify(['ports']));
    localStorage.setItem(
      'solu.ports.view.v3',
      JSON.stringify({ scope: 'workspace', includeOther: false }),
    );
  });
  await page.route('**/api/panes', (route) => route.fulfill({ json: [] }));
  await page.route('**/api/workspaces**', (route) =>
    route.fulfill({ json: route.request().method() === 'POST' ? workspace : [workspace] }),
  );
  await page.route('**/api/ports', (route) =>
    offline ? route.fulfill({ status: 503, body: 'offline' }) : route.fulfill({ json: ports }),
  );
  await page.route('**/api/ports/probe', async (route) => {
    checks++;
    const service = ports.find((p) => p.port === 8000)!;
    service.scheme = 'http';
    service.webStatus = 'ready';
    await route.fulfill({ json: service });
  });
  await page.route('**/b/win/api/proxy/5173~**', (route) =>
    route.fulfill({
      contentType: 'text/html',
      body: route.request().url().includes('~bmV3/') ? '<h1>Restarted preview</h1>' : '<h1>Dashboard preview</h1>',
    }),
  );
  await page.goto('/');
  const panel = page.locator('#sidebar .services-panel');
  await expect(panel.getByText(':5173', { exact: true })).toBeVisible();
  await expect(panel.getByText(':7000', { exact: true })).toHaveCount(0);
  await expect(panel.getByText(':5432', { exact: true })).toHaveCount(0);
  await panel.getByRole('button', { name: 'Open preview', exact: true }).click();
  await expect(
    page.frameLocator('.browser-pane iframe').getByRole('heading', { name: 'Dashboard preview' }),
  ).toBeVisible();
  ports[0] = { ...ports[0], pid: 84, previewPath: '/api/proxy/5173~bmV3/' };
  await page.getByTitle('Reload', { exact: true }).click();
  await expect(
    page.frameLocator('.browser-pane iframe').getByRole('heading', { name: 'Restarted preview' }),
  ).toBeVisible();
  await page.getByText('1 service', { exact: true }).click();
  const popover = page.getByRole('dialog', { name: 'Services', exact: true });
  await popover.getByRole('combobox', { name: 'Service scope' }).selectOption('all');
  await expect(panel.getByRole('combobox')).toHaveValue('all');
  await expect(panel.getByText(':7000', { exact: true })).toBeVisible();
  await page.keyboard.press('Escape');
  expect(checks).toBe(0);
  const external = panel.locator('.port-row').filter({ hasText: ':8000' });
  await expect(external.getByRole('button', { name: 'Open preview', exact: true })).toBeDisabled();
  await external.getByRole('button', { name: 'Check web service', exact: true }).click();
  // Confirmed as a web service, but no pane started it: it opens in the
  // person's own browser, never the preview tile.
  await expect(external.getByRole('button', { name: 'Open in browser', exact: true })).toBeEnabled();
  await expect(external.getByRole('button', { name: 'Open preview', exact: true })).toBeDisabled();
  await expect(external.getByRole('button', { name: 'Open preview', exact: true })).toHaveAttribute(
    'title',
    'Preview shows services started in a soluterminal pane; use Open in browser',
  );
  expect(checks).toBe(1);
  await panel.getByRole('checkbox').check();
  const database = panel.locator('.port-row').filter({ hasText: ':5432' });
  await expect(database.getByRole('button', { name: 'Open preview', exact: true })).toBeDisabled();
  for (const theme of ['dark', 'light']) {
    await page.evaluate((value) => {
      document.documentElement.dataset.theme = value;
    }, theme);
    await panel.screenshot({ path: info.outputPath(`services-${theme}.png`) });
  }
  offline = true;
  await panel.getByRole('button', { name: 'Refresh services' }).click();
  await expect(panel.getByRole('status')).toContainText('Could not refresh services');
  await expect(panel.getByText(':5173', { exact: true })).toBeVisible();
  offline = false;
  await panel.getByRole('button', { name: 'Refresh services' }).click();
  await expect(panel.getByRole('status')).toHaveCount(0);
  expect(errors).toEqual([]);
});
