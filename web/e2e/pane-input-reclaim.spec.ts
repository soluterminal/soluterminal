import type { Page, WebSocketRoute } from 'playwright/test';
import { expect, test } from './fixtures';
import type { PaneInfo, Workspace } from '../src/types';

// Input ownership after a reconnect (every cloud deploy drops every relayed
// pane socket). A fake daemon socket stands in for /io: it answers claims per
// the test's policy and records every input frame, so these tests prove the
// page re-claims on its own and never loses typing silently.

type Claim = 'grant' | 'deny' | 'ignore' | { delayMs: number };
interface Conn {
  ws: WebSocketRoute;
  claims: string[];
  input: { text: string; at: number }[];
  ownedAt?: number;
}

const pane = { id: 'reclaim-pane', title: 'Reclaim', cwd: '/tmp', cmd: ['bash'], status: 'running',
  pid: 31, cols: 100, rows: 30, state: 'idle', createdAt: '2026-10-31T00:00:00Z', lastActivityMs: 0 } as PaneInfo;
const workspaces: Workspace[] = [
  { name: 'Main', paneIds: [pane.id], panes: [pane], layout: { type: 'pane', paneId: pane.id }, activeAt: '2026-10-31T00:00:00Z' },
];

async function setup(page: Page, policy: (conn: number, reason: string) => Claim, initialOwner: 'none' | 'other' = 'none') {
  // A person's browser: not navigator.webdriver (driven browsers never
  // auto-claim), with a page visibility the test can flip.
  await page.addInitScript(() => {
    const w = window as unknown as { __soluTestHidden: boolean };
    w.__soluTestHidden = false;
    Object.defineProperty(Navigator.prototype, 'webdriver', { configurable: true, get: () => false });
    Object.defineProperty(Document.prototype, 'visibilityState', { configurable: true, get: () => (w.__soluTestHidden ? 'hidden' : 'visible') });
    Object.defineProperty(Document.prototype, 'hidden', { configurable: true, get: () => w.__soluTestHidden });
  });
  await page.route('**/api/panes', route => route.fulfill({ json: [pane] }));
  await page.route('**/api/workspaces**', route => route.fulfill({ json: workspaces }));
  const conns: Conn[] = [];
  await page.routeWebSocket(/\/api\/panes\/.*\/io/, ws => {
    const index = conns.length;
    const conn: Conn = { ws, claims: [], input: [] };
    conns.push(conn);
    const grant = () => {
      conn.ownedAt = Date.now();
      ws.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
    };
    ws.onMessage(message => {
      if (typeof message !== 'string') {
        conn.input.push({ text: Buffer.from(message).toString(), at: Date.now() });
        return;
      }
      let msg: { type?: string; reason?: string };
      try { msg = JSON.parse(message); } catch { return; }
      if (msg.type !== 'claim') return;
      conn.claims.push(msg.reason ?? '');
      const action = policy(index, msg.reason ?? '');
      if (action === 'grant') grant();
      else if (action === 'deny') ws.send(JSON.stringify({ type: 'ownership', owned: false, owner: 'other' }));
      else if (typeof action === 'object') setTimeout(grant, action.delayMs);
    });
    // The daemon's subscribe-time frame: after a reconnect nobody owns it.
    ws.send(JSON.stringify({ type: 'ownership', owned: false, owner: initialOwner }));
    ws.send(Buffer.from('\r\n$ ready\r\n'));
  });
  await page.goto('/?ws=Main');
  const terminal = page.locator(`[data-pane-id="${pane.id}"]`);
  await expect(terminal.locator('.xterm')).toBeVisible();
  return { conns, terminal };
}

const typed = (conn: Conn) => conn.input.map(i => i.text).join('');
const setHidden = (page: Page, hidden: boolean) => page.evaluate(h => {
  (window as unknown as { __soluTestHidden: boolean }).__soluTestHidden = h;
  document.dispatchEvent(new Event('visibilitychange'));
}, hidden);
const lifecycleNames = (page: Page) => page.evaluate(() =>
  (window as unknown as { __SOLU_LIFECYCLE__: { dump: () => { name: string }[] } }).__SOLU_LIFECYCLE__.dump().map(e => e.name));

test('a reconnect re-claims and typing during the claim is delivered once owned', async ({ page }) => {
  const { conns, terminal } = await setup(page, conn => (conn === 0 ? 'grant' : { delayMs: 2500 }));
  await expect.poll(() => conns[0]?.ownedAt).toBeTruthy();
  await terminal.locator('.xterm').click();
  await page.keyboard.type('a');
  await expect.poll(() => typed(conns[0])).toContain('a');

  await conns[0].ws.close({ code: 4000, reason: 'relay tunnel moved' });
  await expect.poll(() => conns[1]?.claims ?? []).toContain('adopt-unowned');
  // Typed while the claim is in flight: held, not dropped.
  await page.keyboard.type('echo hi');
  await expect.poll(() => typed(conns[1]), { timeout: 5000 }).toContain('echo hi');
  const owned = conns[1].ownedAt!;
  for (const frame of conns[1].input.filter(f => /[echoi ]/.test(f.text))) expect(frame.at).toBeGreaterThanOrEqual(owned);
  expect(await lifecycleNames(page)).toContain('pane.input.reclaimed');
  expect(await lifecycleNames(page)).not.toContain('pane.input.dropped');
  await expect(page.getByRole('button', { name: 'Type here' })).not.toBeVisible();
});

test('Take over intent survives a slow claim', async ({ page }) => {
  const { conns, terminal } = await setup(page, conn => conn === 1 ? 'ignore' : 'grant');
  await expect.poll(() => conns[0]?.ownedAt).toBeTruthy();
  // A real peer displaces us. The user's explicit click establishes intent.
  conns[0].ws.send(JSON.stringify({ type: 'ownership', owned: false, owner: 'other' }));
  await page.getByRole('button', { name: 'Type here' }).click();
  await expect.poll(() => conns[0].claims).toContain('takeover');
  await expect(page.getByRole('button', { name: 'Type here' })).not.toBeVisible();

  await conns[0].ws.close({ code: 4000, reason: 'relay tunnel moved' });
  await expect.poll(() => conns[1]?.claims ?? []).toContain('takeover');
  await terminal.locator('.xterm').click();
  await page.keyboard.type('slow');
  // The grant takes longer than the bounded input hold. Expiry must report
  // the lost typing, while preserving the user's intent for future sockets.
  await expect(page.getByRole('button', { name: 'Type here' })).toBeVisible({ timeout: 5000 });
  expect(typed(conns[1])).not.toContain('slow');
  conns[1].ws.send(JSON.stringify({ type: 'ownership', owned: true, owner: 'you' }));
  await expect(page.getByRole('button', { name: 'Type here' })).not.toBeVisible();
  await conns[1].ws.close({ code: 4000, reason: 'relay tunnel moved again' });
  // A remembered owner alone would only adopt-unowned. This proves the
  // explicit Take over intent survived the slow claim and local reset.
  await expect.poll(() => conns[2]?.claims ?? []).toContain('takeover');
  await terminal.locator('.xterm').click();
  await page.keyboard.type('ok');
  await expect.poll(() => typed(conns[2])).toContain('ok');
});

test('an initial ghost owner preserves the pending claim and its retries', async ({ page }) => {
  let claims = 0;
  const { conns, terminal } = await setup(page, () => ++claims === 1 ? 'ignore' : 'grant', 'other');
  await expect.poll(() => conns[0]?.claims ?? []).toEqual(['first-open']);
  await terminal.locator('.xterm').click();
  await page.keyboard.type('ghost');
  await expect(page.getByRole('button', { name: 'Type here' })).not.toBeVisible();
  await expect.poll(() => conns[0].claims, { timeout: 5000 }).toEqual(['first-open', 'first-open']);
  await expect.poll(() => conns[0].ownedAt).toBeTruthy();
  await terminal.locator('.xterm').click();
  await page.keyboard.type('ok');
  await expect.poll(() => typed(conns[0])).toContain('ok');
});

test('a pane hidden at reconnect keeps its claim and sends it when shown', async ({ page }) => {
  const { conns, terminal } = await setup(page, () => 'grant');
  await expect.poll(() => conns[0]?.ownedAt).toBeTruthy();
  await setHidden(page, true);
  await conns[0].ws.close({ code: 4000, reason: 'relay tunnel moved' });
  await expect.poll(() => conns.length).toBe(2);
  await page.waitForTimeout(800);
  expect(conns[1].claims).toEqual([]);
  expect(await lifecycleNames(page)).toContain('pane.ownership.claim-deferred');
  await setHidden(page, false);
  await expect.poll(() => conns[1].claims).toEqual(['adopt-unowned']);
  await expect.poll(() => conns[1].ownedAt).toBeTruthy();
  await terminal.locator('.xterm').click();
  await page.keyboard.type('pwd');
  await expect.poll(() => typed(conns[1])).toContain('pwd');
});

test('an unanswered claim is retried, then an unowned reply leads to a claim', async ({ page }) => {
  test.setTimeout(40_000);
  let answer = false;
  const { conns } = await setup(page, () => (answer ? 'grant' : 'ignore'));
  await expect.poll(() => conns[0]?.claims ?? []).toEqual(['first-open']);
  // The watchdog clears the unanswered claim after 3s and retries (bounded).
  await expect.poll(() => conns[0].claims.length, { timeout: 12_000 }).toBe(3);
  await page.waitForTimeout(3500);
  expect(conns[0].claims).toHaveLength(3);
  // The daemon now says nobody owns the pane: this visible pane claims.
  answer = true;
  conns[0].ws.send(JSON.stringify({ type: 'ownership', owned: false, owner: 'none' }));
  await expect.poll(() => conns[0].claims.at(-1)).toBe('adopt-unowned');
  await expect.poll(() => conns[0].ownedAt).toBeTruthy();
});

test('typing the daemon never accepts shows the claim banner instead of vanishing', async ({ page }) => {
  let grantTakeover = false;
  const { conns, terminal } = await setup(page, (conn, reason) => {
    if (conn === 0) return 'grant';
    return grantTakeover && reason === 'takeover' ? 'grant' : 'ignore';
  });
  await expect.poll(() => conns[0]?.ownedAt).toBeTruthy();
  await conns[0].ws.close({ code: 4000, reason: 'relay tunnel moved' });
  await expect.poll(() => conns[1]?.claims ?? []).toContain('adopt-unowned');
  await terminal.locator('.xterm').click();
  await page.keyboard.type('lost?');
  await expect(page.getByRole('button', { name: 'Type here' })).toBeVisible({ timeout: 4000 });
  expect(typed(conns[1])).not.toContain('lost?');
  expect(await lifecycleNames(page)).toContain('pane.input.dropped');
  grantTakeover = true;
  await page.getByRole('button', { name: 'Type here' }).click();
  await expect.poll(() => conns[1].ownedAt).toBeTruthy();
  await terminal.locator('.xterm').click();
  await page.keyboard.type('ok');
  await expect.poll(() => typed(conns[1])).toContain('ok');
});
