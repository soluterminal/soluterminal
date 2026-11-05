import { test, expect, type APIRequestContext, type Page } from 'playwright/test';
import { createHash, generateKeyPairSync, randomBytes } from 'node:crypto';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

// ST-9 item 7 against the real daemon, in the launcher's "seeded-approver"
// scenario (scripts/side-daemon-e2e-seed.mjs): one browser key, made fresh
// for this run, is approved at this computer before solud starts. Approving
// goes through that browser's own local signature (localVerifiedDevice);
// the same daemon refuses a browser without it. The asking devices enrol
// over the real V3 routes and compute their own codes here, as a phone
// would. The first-device ("headers") path and a person's own enrolment
// from Windows stay with the Preview check.

interface Seeded { id: string; name: string; spki: string; pkcs8: string }
interface Asking { id: string; spki: string; code: string; enrolmentId: string }

const CROCKFORD = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
const DIALOG = 'Approve a device?';
const CANNOT_APPROVE_HERE = "This screen can't approve devices. Approve from a browser or app you've already approved.";
const REQUEST_CHANGED = 'This request changed since it was shown. Compare the new code before approving.';
const NO_CODE_CONFIRM = 'I am sure: approve without comparing codes';

// The private key reaches the page as an argument: keep it out of traces.
test.use({ trace: 'off' });
test.describe.configure({ mode: 'serial' });

function seeded(): Seeded {
  expect(process.env.SOLU_REAL_E2E_SCENARIO).toBe('seeded-approver');
  return JSON.parse(readFileSync(process.env.SOLU_REAL_E2E_SEEDED ?? '', 'utf8')) as Seeded;
}

const sha = (s: string) => createHash('sha256').update(s).digest();

function crockford80(sum: Buffer): string {
  const n = BigInt('0x' + sum.subarray(0, 10).toString('hex'));
  let out = '';
  for (let i = 0; i < 16; i++) {
    if (i > 0 && i % 4 === 0) out += '-';
    out += CROCKFORD[Number((n >> BigInt(75 - 5 * i)) & 31n)];
  }
  return out;
}

function newKey(): string {
  return generateKeyPairSync('ec', { namedCurve: 'prime256v1' }).publicKey.export({ type: 'spki', format: 'der' }).toString('base64');
}

const deviceId = (spki: string) => 'dev_' + sha(spki).toString('base64url').slice(0, 22);

/** A device asking over V3 commit/reveal, computing its own code. */
async function askV3(request: APIRequestContext, name: string, spki = newKey()): Promise<Asking> {
  const id = deviceId(spki);
  const nonce = randomBytes(32).toString('hex');
  const commit = sha(['SOLU-DEVICE-COMMIT-V3', id, nonce].join('\n')).toString('hex');
  const enrol = await request.post('/api/remote-devices/enroll', { data: { name, publicKey: spki, codeVersion: 3, commit } });
  expect(enrol.status()).toBe(200);
  const committed = await enrol.json();
  expect(committed).toMatchObject({ id, status: 'committed', codeVersion: 3 });
  const code = crockford80(sha(['SOLU-DEVICE-CODE-V3', spki, committed.daemonIdentity, committed.enrolmentId, nonce, committed.daemonNonce].join('\n')));
  const reveal = await request.post('/api/remote-devices/enroll/reveal', { data: { id, enrolmentId: committed.enrolmentId, nonce } });
  expect(reveal.status()).toBe(200);
  expect(await reveal.json()).toMatchObject({ id, status: 'pending', codeVersion: 3, enrolmentId: committed.enrolmentId });
  return { id, spki, code, enrolmentId: committed.enrolmentId };
}

async function deviceStatus(request: APIRequestContext, id: string) {
  const res = await request.get(`/api/remote-devices/${id}/status`);
  expect(res.ok()).toBeTruthy();
  return res.json();
}

function daemonLog(): string {
  const path = join(process.cwd(), 'test-results/side-daemon/solud.log');
  return existsSync(path) ? readFileSync(path, 'utf8') : '';
}

/** Give this browser context the seeded approved key, as IndexedDB holds it. */
async function installKey(page: Page, key: Seeded) {
  await page.goto('/api/health?identity=1');
  await page.evaluate(async ({ spki, pkcs8 }) => {
    const der = (b: string) => Uint8Array.from(atob(b), (c) => c.charCodeAt(0));
    const alg = { name: 'ECDSA', namedCurve: 'P-256' };
    const privateKey = await crypto.subtle.importKey('pkcs8', der(pkcs8), alg, false, ['sign']);
    const publicKey = await crypto.subtle.importKey('spki', der(spki), alg, true, ['verify']);
    await new Promise<void>((resolve, reject) => {
      const open = indexedDB.open('soluterminal-device', 1);
      open.onupgradeneeded = () => open.result.createObjectStore('keys');
      open.onerror = () => reject(open.error);
      open.onsuccess = () => {
        const db = open.result;
        const tx = db.transaction('keys', 'readwrite');
        tx.objectStore('keys').put({ privateKey, publicKey }, 'device-p256');
        tx.oncomplete = () => { db.close(); resolve(); };
        tx.onerror = () => reject(tx.error);
      };
    });
  }, { spki: key.spki, pkcs8: key.pkcs8 });
}

const path = (url: string) => new URL(url).pathname;

/** Load the app and answer the first-run integration question with "Not now". */
async function start(page: Page, key?: Seeded) {
  await page.addInitScript(() => {
    localStorage.setItem('solu.seenWelcome', '1');
    localStorage.setItem('solu.seenChecklist', '1');
  });
  if (key) await installKey(page, key);
  const proof = page.waitForResponse((r) => path(r.url()) === '/api/human-proof' && r.request().method() === 'GET');
  const integration = page.waitForResponse((r) => path(r.url()) === '/api/settings/agent-integration' && r.request().method() === 'GET');
  await page.goto('/');
  const standing = await proof;
  if ((await (await integration).json()).needsAnswer) {
    await page.getByRole('dialog').getByRole('button', { name: 'Not now', exact: true }).click();
  }
  // Which key this browser asked about, and the daemon's answer for it.
  return { key: new URL(standing.url()).searchParams.get('device'), ...(await standing.json()) };
}

async function openRemoteDevices(page: Page) {
  const listed = page.waitForResponse((r) => path(r.url()) === '/api/remote-devices' && r.request().method() === 'GET');
  await page.locator('.topbar button[aria-label="Settings"]').click();
  const settings = page.getByRole('dialog', { name: 'Settings' });
  await settings.getByRole('tab', { name: 'Privacy & security', exact: true }).click();
  const response = await listed;
  expect(response.ok()).toBeTruthy();
  return { settings, response, list: await response.json() };
}

function approvePosts(page: Page): string[] {
  const seen: string[] = [];
  page.on('request', (r) => {
    if (r.method() === 'POST' && /^\/api\/remote-devices\/[^/]+\/approve$/.test(path(r.url()))) seen.push(r.postData() ?? '');
  });
  return seen;
}

test('refused side: a browser without the approved key sees no Approve, and a forced approval is refused', async ({ page, request }) => {
  const key = seeded();
  const asking = await askV3(request, 'Phone asking (refused side)');
  const standing = await start(page);
  expect(standing.key).not.toBe(key.id);
  expect(standing.approvedDevices).toBe(1);
  expect(standing.device).not.toBe('approved');
  await expect(page.getByRole('region', { name: "This screen isn't approved yet" })).toBeVisible();

  const { settings, response, list } = await openRemoteDevices(page);
  expect(response.request().headers()['x-solu-device']).toBeUndefined();
  expect(list.canManageDevices).toBe(false);
  const row = settings.locator('.settings-row', { hasText: 'Phone asking (refused side)' });
  await expect(row).toContainText(asking.code);
  await expect(row.getByRole('button', { name: 'Compare codes…' })).toHaveCount(0);
  await expect(settings.getByText(CANNOT_APPROVE_HERE)).toBeVisible();

  // Forced past the UI, from this page and from a plain program.
  const forced = await page.evaluate(async ({ id, enrolmentId }) => {
    const res = await fetch(`/api/remote-devices/${id}/approve`, {
      method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ enrolmentId }),
    });
    return { status: res.status, body: await res.text() };
  }, asking);
  expect(forced.status).toBe(403);
  const program = await request.post(`/api/remote-devices/${asking.id}/approve`, { data: { enrolmentId: asking.enrolmentId } });
  expect(program.status()).toBe(403);
  test.info().annotations.push({ type: 'forced-approval', description: JSON.stringify({ page: forced, program: { status: program.status(), body: await program.text() } }) });

  expect(await deviceStatus(request, asking.id)).toMatchObject({ status: 'pending', enrolmentId: asking.enrolmentId });
  expect(daemonLog()).not.toContain(`remote-devices: approved ${asking.id}`);
});

test('an approved browser approves a V3 request after comparing codes; Enter approves nothing', async ({ page, request }) => {
  const key = seeded();
  const asking = await askV3(request, 'Phone asking (approve)');
  const posts = approvePosts(page);
  const standing = await start(page, key);
  expect(standing).toMatchObject({ key: key.id, device: 'approved', approvedDevices: 1 });

  const { settings, response, list } = await openRemoteDevices(page);
  // The list read is signed with the seeded key, and the daemon says this screen may approve.
  expect(response.request().headers()['x-solu-device']).toBe(key.id);
  expect(list.canManageDevices).toBe(true);
  await settings.locator('.settings-row', { hasText: 'Phone asking (approve)' }).getByRole('button', { name: 'Compare codes…' }).click();

  const dialog = page.getByRole('dialog', { name: DIALOG });
  await expect(dialog.getByTestId('approve-device-name')).toHaveText('Phone asking (approve)');
  // The code the device computed itself, shown large and labelled V3.
  await expect(dialog.getByTestId('approve-device-code')).toHaveText(asking.code);
  await expect(dialog.getByText('V3', { exact: true })).toBeVisible();
  const approve = dialog.getByRole('button', { name: 'Codes match: approve' });
  await expect(approve).toBeEnabled();
  await expect(approve).not.toBeFocused();

  // Enter, on the dialog and on Approve itself, approves nothing.
  await page.keyboard.press('Enter');
  await approve.focus();
  await page.keyboard.press('Enter');
  await page.waitForTimeout(750);
  expect(posts).toEqual([]);
  expect(await deviceStatus(request, asking.id)).toMatchObject({ status: 'pending' });

  const [approved] = await Promise.all([
    page.waitForResponse((r) => path(r.url()) === `/api/remote-devices/${asking.id}/approve`),
    approve.click(),
  ]);
  expect(approved.status()).toBe(200);
  expect(approved.request().headers()['x-solu-device']).toBe(key.id);
  expect(approved.request().postDataJSON()).toEqual({ enrolmentId: asking.enrolmentId });
  await expect(dialog).toHaveCount(0);

  expect(await deviceStatus(request, asking.id)).toMatchObject({ status: 'approved', approvedVia: `device:${key.id}` });
  await expect.poll(daemonLog).toContain(`remote-devices: approved ${asking.id} (Phone asking (approve)) via device:${key.id}`);
  expect(posts).toHaveLength(1);
});

test('a request that changed since it was shown answers 409: the dialog re-reads and never retries', async ({ page, request }) => {
  const key = seeded();
  const asking = await askV3(request, 'Tablet asking (changed)');
  const posts = approvePosts(page);
  await start(page, key);
  const { settings } = await openRemoteDevices(page);
  await settings.locator('.settings-row', { hasText: 'Tablet asking (changed)' }).getByRole('button', { name: 'Compare codes…' }).click();
  const dialog = page.getByRole('dialog', { name: DIALOG });
  await expect(dialog.getByTestId('approve-device-code')).toHaveText(asking.code);

  // Meanwhile, in another tab of the same approved browser, the request is
  // removed, and the device asks again with the same key.
  const other = await page.context().newPage();
  await start(other);
  const { settings: otherSettings } = await openRemoteDevices(other);
  const [removed] = await Promise.all([
    other.waitForResponse((r) => path(r.url()) === `/api/remote-devices/${asking.id}` && r.request().method() === 'DELETE'),
    otherSettings.locator('.settings-row', { hasText: 'Tablet asking (changed)' }).getByRole('button', { name: 'Remove', exact: true }).click(),
  ]);
  expect(removed.status()).toBe(204);
  const again = await askV3(request, 'Tablet asking (changed)', asking.spki);
  expect(again.id).toBe(asking.id);
  expect(again.enrolmentId).not.toBe(asking.enrolmentId);
  await other.close();

  const approve = dialog.getByRole('button', { name: 'Codes match: approve' });
  const [stale] = await Promise.all([
    page.waitForResponse((r) => path(r.url()) === `/api/remote-devices/${asking.id}/approve`),
    approve.click(),
  ]);
  expect(stale.status()).toBe(409);
  expect(stale.request().postDataJSON()).toEqual({ enrolmentId: asking.enrolmentId });
  await expect(dialog.getByText(REQUEST_CHANGED)).toBeVisible();
  await expect(dialog.getByTestId('approve-device-code')).toHaveText(again.code);
  await page.waitForTimeout(750);
  expect(posts).toHaveLength(1);
  expect(await deviceStatus(request, asking.id)).toMatchObject({ status: 'pending', enrolmentId: again.enrolmentId });

  // Approving the request now shown is the person's next, separate act.
  const [approved] = await Promise.all([
    page.waitForResponse((r) => path(r.url()) === `/api/remote-devices/${asking.id}/approve`),
    approve.click(),
  ]);
  expect(approved.status()).toBe(200);
  expect(approved.request().postDataJSON()).toEqual({ enrolmentId: again.enrolmentId });
  expect(await deviceStatus(request, asking.id)).toMatchObject({ status: 'approved', approvedVia: `device:${key.id}` });
});

test('a request with no V3 code is approved only after the explicit confirmation', async ({ page, request }) => {
  const key = seeded();
  const spki = newKey();
  const enrol = await request.post('/api/remote-devices/enroll', { data: { name: 'Laptop asking (no V3 code)', publicKey: spki, codeVersion: 2 } });
  expect(enrol.status()).toBe(200);
  const id = deviceId(spki);
  expect(await enrol.json()).toMatchObject({ id, status: 'pending', codeVersion: 2 });

  await start(page, key);
  const { settings } = await openRemoteDevices(page);
  await settings.locator('.settings-row', { hasText: 'Laptop asking (no V3 code)' }).getByRole('button', { name: 'Compare codes…' }).click();
  const dialog = page.getByRole('dialog', { name: DIALOG });
  await expect(dialog.getByRole('alert')).toContainText('No code to compare');
  await expect(dialog.getByTestId('approve-device-code')).toHaveCount(0);
  const approve = dialog.getByRole('button', { name: 'Codes match: approve' });
  await expect(approve).toBeDisabled();
  await dialog.getByRole('checkbox', { name: NO_CODE_CONFIRM }).check();
  await expect(approve).toBeEnabled();
  const [approved] = await Promise.all([
    page.waitForResponse((r) => path(r.url()) === `/api/remote-devices/${id}/approve`),
    approve.click(),
  ]);
  expect(approved.status()).toBe(200);
  expect(approved.request().headers()['x-solu-device']).toBe(key.id);
  expect(await deviceStatus(request, id)).toMatchObject({ status: 'approved', approvedVia: `device:${key.id}` });
});
