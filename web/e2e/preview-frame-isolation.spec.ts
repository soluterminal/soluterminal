import { test, expect } from './fixtures';

// In the desktop app the preview tile frames a pane's service on a preview
// origin of its own (the daemon's or the router's). A page there reaches
// neither the app's document nor its desktop bridge.
const PREVIEW_ORIGIN = 'http://127.0.0.1:42917';

test('a framed preview cannot reach the app document or the desktop bridge', async ({ page }) => {
  const workspace = { name: 'Preview isolation', paneIds: [], layout: null, activeAt: '2026-11-08T00:00:00Z' };
  const port = {
    port: 5173, addr: '127.0.0.1', pid: 42, process: 'node', cmdline: 'node vite', cwd: '/home/developer/app',
    firstAt: '2026-11-08T00:00:00Z', proto: 'tcp', scheme: 'http', webStatus: 'ready',
    workspace: workspace.name, paneId: 'web-pane', paneTitle: 'App', previewPath: '/api/proxy/5173~aXNv/',
  };
  await page.addInitScript(() => {
    localStorage.setItem('solu.activeWorkspace', 'Preview isolation');
    localStorage.setItem('solu.sidebarOpenMiddle.v2', JSON.stringify(['ports']));
    localStorage.setItem('solu.ports.view.v3', JSON.stringify({ scope: 'workspace', includeOther: false }));
    if (window.top !== window) return; // the bridge exists in the app's own frame only
    const w = window as unknown as Record<string, unknown>;
    // Calls the page makes are marked fromPreview, apart from the app's own.
    const calls: string[] = [];
    w.__bridgeCalls = calls;
    w.__SOLU_TAURI__ = true;
    w.__TAURI_INTERNALS__ = {
      invoke: (cmd: string, args?: { fromPreview?: boolean }) => {
        if (args?.fromPreview) calls.push(cmd);
        // A 0.0.1241 shell: the first that frames previews at all.
        return Promise.resolve(cmd === 'browser_pane_ping' && !args?.fromPreview ? '0.0.1241' : null);
      },
    };
    window.addEventListener('message', (e) => {
      if (e.data && typeof e.data === 'object' && 'previewProbe' in e.data) w.__probe = e.data.previewProbe;
    });
  });
  await page.route('**/api/panes', (route) => route.fulfill({ json: [] }));
  await page.route('**/api/workspaces**', (route) =>
    route.fulfill({ json: route.request().method() === 'POST' ? workspace : [workspace] }),
  );
  await page.route('**/api/ports', (route) => route.fulfill({ json: [port] }));
  await page.route('**/api/preview-origin', (route) => route.fulfill({ json: { origin: PREVIEW_ORIGIN, prefix: '/b/host' } }));
  await page.route(`${PREVIEW_ORIGIN}/**`, (route) =>
    route.fulfill({
      contentType: 'text/html',
      body: `<h1>Isolated preview</h1><script>
        const out = {};
        try { out.doc = parent.document.body ? 'reached' : 'empty'; } catch (e) { out.doc = 'refused'; }
        try {
          const bridge = parent.__TAURI_INTERNALS__;
          out.bridge = bridge ? 'reached' : 'absent';
          if (bridge && bridge.invoke) for (const cmd of ['browser_pane_ping', 'plugin:opener|open_url', 'desktop_set_backend_mode', 'apply_shell_update', 'desktop_update_this_computer']) bridge.invoke(cmd, { fromPreview: true });
        } catch (e) { out.bridge = 'refused'; }
        out.own = typeof window.__TAURI_INTERNALS__;
        top.postMessage({ previewProbe: out }, '*');
      </script>`,
    }),
  );

  await page.goto('/');
  const panel = page.locator('#sidebar .services-panel');
  await panel.getByRole('button', { name: 'Open preview', exact: true }).click();
  const frame = page.frameLocator('.browser-pane iframe');
  await expect(frame.getByRole('heading', { name: 'Isolated preview' })).toBeVisible();
  await expect(page.locator('.browser-pane iframe')).toHaveAttribute('src', `${PREVIEW_ORIGIN}/b/host/api/proxy/5173~aXNv/`);

  await expect.poll(() => page.evaluate(() => (window as unknown as { __probe?: unknown }).__probe)).toEqual({
    doc: 'refused',
    bridge: 'refused',
    own: 'undefined',
  });
  expect(await page.evaluate(() => (window as unknown as { __bridgeCalls: string[] }).__bridgeCalls)).toEqual([]);
});
