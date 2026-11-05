import { expect, test } from './fixtures';

// All requests use the isolated fixture server; no installed daemon is used.
for (const embedded of [false, true]) {
  for (const native of ['browser', 'async plugin'] as const) {
    test(`service and device settings cancel with Shift+Tab and Enter, then confirm (${native}, ${embedded ? 'Shinoque Code frame' : 'standalone'})`, async ({ context, page }) => {
      await page.addInitScript((asyncConfirm) => {
        const win = window as unknown as { confirm: () => boolean | Promise<boolean>; nativeConfirmCalls: number };
        win.nativeConfirmCalls = 0;
        win.confirm = () => {
          win.nativeConfirmCalls++;
          return asyncConfirm ? Promise.resolve(false) : false;
        };
      }, native === 'async plugin');

      let restarts = 0;
      let mode = 'report';
      let modeWrites = 0;
      let policy = 'any';
      let policyWrites = 0;
      let devices = [{ id: 'dev_fixture', name: 'Preview browser', status: 'approved', createdAt: '', approvedVia: 'first-device', surface: 'local' }];
      const proof = () => ({ mode, effective: mode, target: 'local/dmn_confirm_fixture', serverTime: Date.now(), envOff: false, approvedDevices: 1, canBootstrap: false, device: 'approved' });
      await context.route('**/api/human-proof*', async (route) => {
        if (route.request().method() === 'PUT') {
          mode = route.request().postDataJSON().mode;
          modeWrites++;
        }
        await route.fulfill({ json: proof() });
      });
      await context.route(/\/api\/remote-devices(?:\/|\?|$)/, async (route) => {
        if (route.request().method() === 'PUT') {
          policy = route.request().postDataJSON().policy;
          policyWrites++;
        }
        await route.fulfill({ json: { policy, devices } });
      });
      await context.route('**/api/service/restart', async (route) => {
        if (route.request().method() === 'POST') {
          expect(route.request().postDataJSON()).toEqual({ pid: 41 });
          restarts++;
        }
        await route.fulfill({ json: { supported: true, pid: 41, daemonIdentity: 'dmn_confirm_fixture', heldDeliveries: 2, putBackReceipts: 1 } });
      });
      await context.route('**/api/health*', (route) => route.fulfill({ json: { pid: restarts ? 42 : 41, daemonIdentity: 'dmn_confirm_fixture', dev: true } }));

      if (embedded) {
        await page.route('**/confirm-host', (route) => route.fulfill({
          contentType: 'text/html',
          body: `<script>window.addEventListener('message', event => {
            if (event.data?.source === 'soluterminal' && event.data.kind === 'ready')
              event.source.postMessage({ source: 'shinoque', v: 1, kind: 'visibility', visible: true }, event.origin);
          });</script><iframe id="confirmation-code-frame" title="SoluTerminal Code"
            src="/?embed=shinoque&theme=light" style="width:100%;height:850px"></iframe>`,
        }));
      }
      await page.goto(embedded ? '/confirm-host' : '/');
      const app = embedded ? page.frameLocator('#confirmation-code-frame') : page;
      if (embedded) {
        await expect(app.locator('html')).toHaveAttribute('data-theme', 'light');
        await expect(app.locator('.topbar-app-icon')).toHaveCount(0);
      }
      await app.locator('.topbar button[aria-label="Settings"]').click();
      const settings = app.locator('.settings-modal[role="dialog"]');
      const dialog = app.locator('.app-dialog[role="dialog"]');
      const cancelWithKeyboard = async () => {
        await expect(dialog.getByRole('button', { name: 'Confirm', exact: true })).toBeFocused();
        await page.keyboard.press('Shift+Tab');
        await expect(dialog.getByRole('button', { name: 'Cancel', exact: true })).toBeFocused();
        await page.keyboard.press('Enter');
        await expect(dialog).toBeHidden();
      };
      await settings.getByRole('tab', { name: 'Devices & updates', exact: true }).click();
      const restart = settings.getByRole('button', { name: 'Restart background service', exact: true });
      await restart.click();
      await expect(dialog).toContainText('2 held deliveries and 1 put-back receipts');
      expect(restarts).toBe(0);
      await cancelWithKeyboard();
      await expect(dialog).toBeHidden();
      expect(restarts).toBe(0);
      await restart.click();
      await dialog.getByRole('button', { name: 'Confirm', exact: true }).click();
      await expect.poll(() => restarts).toBe(1);
      await expect(settings.getByRole('status')).toContainText('Background service restarted. PID 41 → 42.');

      await settings.getByRole('tab', { name: 'Privacy & security', exact: true }).click();
      const require = settings.getByRole('combobox', { name: 'Require approved devices for actions that need you' });
      await require.selectOption('require');
      await expect(dialog).toContainText('Only these can then act for you on connections that can’t be traced:');
      await expect(dialog).toContainText('Preview browser (asked at this computer, approved as the first device)');
      expect(modeWrites).toBe(0);
      await cancelWithKeyboard();
      await expect(require).toHaveValue('report');
      expect(modeWrites).toBe(0);
      await require.selectOption('require');
      await dialog.getByRole('button', { name: 'Confirm', exact: true }).click();
      await expect(require).toHaveValue('require');
      expect(modeWrites).toBe(1);

      // Only the fixture loses its approved devices, to exercise the warning.
      devices = [];
      await settings.getByRole('tab', { name: 'Devices & updates', exact: true }).click();
      await settings.getByRole('tab', { name: 'Privacy & security', exact: true }).click();
      await expect(settings.getByText('No devices have asked yet.', { exact: false })).toBeVisible();
      const remote = settings.getByRole('combobox').filter({ has: page.locator('option[value="any"]') });
      await remote.selectOption('approved');
      await expect(dialog).toHaveText(/No device is approved yet, so your phone and other browsers will lose access until you approve them here. Continue\?/);
      expect(policyWrites).toBe(0);
      await cancelWithKeyboard();
      await expect(remote).toHaveValue('any');
      expect(policyWrites).toBe(0);
      await remote.selectOption('approved');
      await dialog.getByRole('button', { name: 'Confirm', exact: true }).click();
      await expect(remote).toHaveValue('approved');
      expect(policyWrites).toBe(1);
      expect(await app.locator('html').evaluate(() => (window as unknown as { nativeConfirmCalls: number }).nativeConfirmCalls)).toBe(0);
    });
  }
}
