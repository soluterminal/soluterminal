import type { Route } from 'playwright/test';
import { installCredentialsKeysState } from '../tools/capture-credentials-keys.mjs';
import { expect, test } from './fixtures';

function json(route: Route, value: unknown) {
  return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(value) });
}

for (const theme of ['dark', 'light']) {
  test(`team quota refresh and owner policy remain visible (${theme})`, async ({ context, page }) => {
    await page.addInitScript(installCredentialsKeysState, theme);
    let refreshed = false;
    const now = Math.floor(Date.now() / 1000);
    const usage = () => ({
      fiveH: { available: false, utilPct: 0, pacePct: 0, resetEpoch: 0, resetInLabel: '' },
      sevenD: { available: refreshed, utilPct: refreshed ? 64 : 0, pacePct: 0, resetEpoch: now + 86400, resetInLabel: '1d' },
      available: refreshed, stale: !refreshed, fetchedAtEpoch: refreshed ? now : 0, source: 'broker',
    });
    const accounts = () => ({ active: 'team__shared', currentUnsaved: false, liveAuthPresent: false, accounts: [
      { key: 'team__shared', name: 'shared', active: true, teamManaged: true, authKind: 'broker', plan: 'pro', usage: usage() },
      { key: 'personal', name: 'personal', active: false, teamManaged: false, authKind: 'access_token' },
    ] });
    await context.route('**/api/claude/accounts', route => json(route, { active: '', accounts: [], machineWide: true, currentUnsaved: false }));
    await context.route('**/api/claude/refresh', route => json(route, { active: '', accounts: [], machineWide: true, currentUnsaved: false }));
    await context.route('**/api/codex/accounts', route => json(route, accounts()));
    await context.route('**/api/codex/refresh', route => { refreshed = true; return json(route, accounts()); });
    await context.route('**/api/codex/autoswitch', route => {
      expect(route.request().method()).toBe('GET');
      return json(route, {
        enabled: true, management: 'team-broker', pinned: '', switchThresholdPct: 85, softWarnPct: 80,
        pool: { team__shared: true, team__next: true },
        state: refreshed ? 'queued' : 'usage-unknown', pendingTarget: refreshed ? 'team__next' : '',
        statusLine: refreshed ? 'Next request will evaluate switching to next: earliest weekly reset with available capacity' : 'Team usage is missing or stale; refresh usage to evaluate automatic switching',
      });
    });
    await page.goto('/');
    const section = page.locator('#sidebar [data-section-key="credentials"]');
    await expect(section).toBeVisible();
    await expect(section.getByText(/Team usage is missing or stale/)).toBeVisible();
    const toggle = section.locator('.cred-autoswitch-toggle-codex');
    await expect(toggle).toBeDisabled();
    await expect(toggle).toHaveAttribute('title', /Managed by the team owner/);
    const shared = section.locator('.cred-row').filter({ hasText: 'shared' });
    await expect(shared.getByText('Codex', { exact: true })).toHaveAttribute('title', /PRO plan/);
    const personal = section.locator('.cred-row').filter({ hasText: 'personal' });
    await personal.hover();
    await expect(personal.locator('.cred-pin, button.cred-pool-chip')).toHaveCount(0);
    await section.locator('.cred-refresh').click();
    await expect(shared.getByText('64%')).toBeVisible();
    await expect(shared.getByText('5h', { exact: true })).toHaveCount(0);
    await expect(shared.getByText(/Usage checked/)).toHaveAttribute('title', /Team broker usage/);
    await expect(section.getByText(/Next request will evaluate switching to next/)).toBeVisible();
    await expect(toggle).toBeDisabled();
  });
  test(`Claude team selection preserves owner controls (${theme})`, async ({ context, page }) => {
    await page.addInitScript(installCredentialsKeysState, theme);
    let active = '';
    const selected: string[] = [];
    const unavailableUsage = {
      fiveH: { available: false, utilPct: 0, pacePct: 0, resetEpoch: 0, resetInLabel: '' },
      sevenD: { available: false, utilPct: 0, pacePct: 0, resetEpoch: 0, resetInLabel: '' },
      available: false, stale: true, fetchedAtEpoch: 0,
    };
    const accounts = () => ({ active, machineWide: true, currentUnsaved: false, accounts: [
      { name: 'team__ready', brokerReady: true, active: active === 'team__ready', usage: unavailableUsage, error: 'Team usage refresh unavailable; retrying' },
      { name: 'team__unready', brokerReady: false, active: false, usage: unavailableUsage, error: 'Owner credential rotation required' },
    ] });
    await context.route('**/api/claude/accounts', route => json(route, accounts()));
    await context.route('**/api/claude/accounts/*/activate', route => {
      const name = decodeURIComponent(new URL(route.request().url()).pathname.split('/').at(-2) ?? '');
      selected.push(name);
      active = name;
      return json(route, { active });
    });
    await context.route('**/api/claude/autoswitch', route => json(route, {
      enabled: true, management: 'team-broker', pinned: '', pool: { team__ready: true }, switchThresholdPct: 85, softWarnPct: 80,
      state: 'usage-unknown', statusLine: 'Team usage is missing or stale',
    }));
    await page.goto('/');
    const section = page.locator('#sidebar [data-section-key="credentials"]');
    const ready = section.locator('.cred-row').filter({ hasText: 'team__ready' });
    const unready = section.locator('.cred-row').filter({ hasText: 'team__unready' });
    await expect(ready).toBeVisible();
    await unready.click();
    await expect(unready.getByRole('button', { name: 'Switch', exact: true })).toHaveCount(0);
    await ready.hover();
    await expect(ready.locator('.cred-pin, button.cred-pool-chip, .cred-rename, .cred-delete')).toHaveCount(0);
    await ready.click();
    expect(selected).toEqual([]);
    await expect(ready.getByText(/New Claude requests using the team broker/)).toBeVisible();
    await ready.getByRole('button', { name: 'Switch', exact: true }).click();
    await expect.poll(() => selected).toEqual(['team__ready']);
    await expect(ready).toHaveClass(/is-active/);
  });

}
