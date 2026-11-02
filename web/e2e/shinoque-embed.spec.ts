import { expect, test } from './fixtures';
import { createServer, type IncomingHttpHeaders } from 'node:http';
import type { AddressInfo } from 'node:net';
import { resolve } from 'node:path';
import { createServer as createFixtureServer } from '../tools/screens-lib.mjs';

test('Shinoque window skips optional sign-in without changing standalone or Settings sign-in', async ({ page }) => {
  // Match window mode's browser topology: UI and Code use the same loopback
  // hostname on separate ports. The fixture serves the Code build, not solud.
  const codeServer = createFixtureServer(resolve('dist'));
  const codeNavigation = new Promise<IncomingHttpHeaders>((resolveHeaders) => {
    codeServer.prependListener('request', (request) => {
      if (request.url?.startsWith('/?embed=shinoque&')) resolveHeaders(request.headers);
    });
  });
  await new Promise<void>((resolveReady) => codeServer.listen(0, '127.0.0.1', resolveReady));
  const codeOrigin = new URL(`http://window.shinoque.localhost:${(codeServer.address() as AddressInfo).port}`);
  await page.route('**/api/config', (route) => route.fulfill({ json: {
    supabaseUrl: 'https://auth.example.test', supabaseAnonKey: 'fixture-key',
    isLocal: true, accountOptional: true,
  } }));
  const server = createServer((_req, res) => {
    res.setHeader('Content-Type', 'text/html');
    res.end(`<script>
      window.embedMessages = [];
      window.addEventListener('message', event => {
        if (event.data?.source === 'soluterminal') {
          window.embedMessages.push(event.data);
          if (event.data.kind === 'ready') event.source.postMessage(
            { source: 'shinoque', v: 1, kind: 'visibility', visible: true }, event.origin);
        }
      });
    </script><iframe title="SoluTerminal Code" src="${codeOrigin.origin}/?embed=shinoque&theme=light"
      style="width:100%;height:700px"></iframe>`);
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  try {
    const port = (server.address() as AddressInfo).port;
    expect(String(port)).not.toBe(codeOrigin.port);
    await page.goto(`http://window.shinoque.localhost:${port}/`);
    // Confirm the browser's classification, rather than assuming it from the
    // URL. Production uses base/base+1; arbitrary distinct fixture ports have
    // the same site classification. No server-side origin exception is needed.
    // Read the headers received on the wire; interception APIs may expose the
    // request before Chromium adds its Sec-Fetch-* headers.
    const navigationHeaders = await codeNavigation;
    expect(navigationHeaders['sec-fetch-site']).toBe('same-site');
    expect(navigationHeaders['sec-fetch-mode']).toBe('navigate');
    expect(navigationHeaders['sec-fetch-dest']).toBe('iframe');
    const frame = page.frameLocator('iframe[title="SoluTerminal Code"]');
    await expect(frame.locator('.topbar')).toBeVisible();
    await expect(frame.getByRole('button', { name: /skip for now/i })).toHaveCount(0);
    await expect(frame.locator('.statusbar-backend')).toHaveCount(0);
    await expect(frame.locator('.topbar-app-icon')).toHaveCount(0);
    await expect.poll(() => page.evaluate(() =>
      (window as Window & { embedMessages?: Array<{ kind?: string }> })
        .embedMessages?.some((message) => message.kind === 'ready'))).toBe(true);
    await expect(frame.locator('html')).toHaveAttribute('data-theme', 'light');
    await expect.poll(() => frame.locator('html').evaluate(() => localStorage.getItem('solu.localNoAccount'))).toBeNull();
    await frame.getByRole('button', { name: 'Settings', exact: true }).click();
    await frame.getByRole('tab', { name: 'Account', exact: true }).click();
    await frame.getByRole('button', { name: 'Create free account or sign in' }).click();
    await expect(frame.getByLabel(/^email$/i)).toBeVisible();
    // The same-site frame and standalone page share the Code origin's storage.
    await page.goto(`${codeOrigin.origin}/`);
    await expect(page.getByLabel(/^email$/i)).toBeVisible();
    await expect(page.getByRole('button', { name: /skip for now/i })).toBeVisible();
  } finally {
    await Promise.all([server, codeServer].map((current) => new Promise<void>((resolveDone, reject) => {
      current.close((error) => error ? reject(error) : resolveDone());
      current.closeAllConnections();
    })));
  }
});

test('loopback parent receives ready and controls embedded first paint', async ({ page }) => {
  await page.route('**/shinoque-host', (route) => route.fulfill({
    contentType: 'text/html',
    body: `<script>
      window.embedMessages = [];
      window.addEventListener('message', event => {
        if (event.data && event.data.source === 'soluterminal') window.embedMessages.push(event.data);
      });
    </script><iframe title="SoluTerminal Code" src="/?embed=shinoque&theme=light" style="width:100%;height:700px"></iframe>`,
  }));
  await page.goto('/shinoque-host');
  const frame = page.frameLocator('iframe[title="SoluTerminal Code"]');
  await expect(frame.locator('.topbar')).toBeVisible();
  await expect.poll(() => page.evaluate(() => (window as Window & { embedMessages?: Array<{kind?: string}> }).embedMessages?.some((m) => m.kind === 'ready'))).toBe(true);
  await expect(frame.locator('html')).toHaveAttribute('data-theme', 'light');
  await expect(frame.locator('.topbar-app-icon')).toHaveCount(0);
  await frame.getByRole('button', { name: 'Settings', exact: true }).click();
  const appTheme = frame.getByRole('combobox', { name: 'App theme', exact: true });
  await expect(appTheme).toBeDisabled();
  await expect(appTheme).toHaveValue('light');
  await expect(frame.getByText('Follows Shinoque’s theme. Change it in Shinoque Settings → Appearance.')).toBeVisible();
  const terminalTheme = frame.locator('.settings-group').filter({ has: frame.getByRole('heading', { name: 'Terminal theme', exact: true }) }).getByRole('combobox');
  await expect(terminalTheme).toBeEnabled();
  await terminalTheme.selectOption({ index: 1 });
  await expect(terminalTheme).not.toHaveValue('auto');
  await terminalTheme.selectOption('auto');
  await page.evaluate(() => window.frames[0].postMessage({ source: 'shinoque', v: 1, kind: 'init', theme: 'dark' }, location.origin));
  await expect(frame.locator('html')).not.toHaveAttribute('data-theme', 'light');
  await expect(appTheme).toHaveValue('dark');
  await page.evaluate(() => window.frames[0].postMessage({ source: 'shinoque', v: 1, kind: 'theme', theme: 'light' }, location.origin));
  await expect(frame.locator('html')).toHaveAttribute('data-theme', 'light');
  await expect(appTheme).toHaveValue('light');
  await frame.locator('body').press('Escape');
  await frame.locator('body').press('Control+K');
  const embeddedPalette = frame.getByRole('dialog', { name: 'Command palette' });
  await embeddedPalette.locator('input').fill('Switch to dark theme');
  await expect(embeddedPalette.getByText('Switch to dark theme', { exact: true })).toHaveCount(0);
  await embeddedPalette.locator('input').press('Enter');
  await expect(frame.locator('html')).toHaveAttribute('data-theme', 'light');
  await embeddedPalette.locator('input').press('Escape');
  await page.locator('iframe').evaluate((iframe: HTMLIFrameElement) => { iframe.src = '/?theme=light'; });
  await expect(frame.locator('.topbar')).toBeVisible();
  await expect(frame.locator('.topbar-app-icon')).toHaveCount(0);
  await page.goto('/');
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  const standaloneTheme = page.getByRole('combobox', { name: 'App theme', exact: true });
  await expect(standaloneTheme).toBeEnabled();
  await standaloneTheme.selectOption('light');
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
  await standaloneTheme.selectOption('dark');
  await expect(page.locator('html')).not.toHaveAttribute('data-theme', 'light');
  await page.keyboard.press('Escape');
  await page.keyboard.press('Control+K');
  const standalonePalette = page.getByRole('dialog', { name: 'Command palette' });
  await standalonePalette.locator('input').fill('Switch to light theme');
  await standalonePalette.getByText('Switch to light theme', { exact: true }).click();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
});
