// @vitest-environment node
import { createTempFixtures } from './temp-fixtures.mjs';
import { afterAll, describe, expect, it } from 'vitest';
import {
  copyFileSync,
  cpSync,
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  writeFileSync,
} from 'node:fs';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { probeNetworkSandbox } from './browser-preflight.mjs';
import * as smoke from './e2e-smoke.mjs';
import {
  buildServiceFixtures,
  buildServiceObjectResponses,
  createServer,
  fixtureHealthVersion,
  launchChromium,
  proposalExecutionEditedPrompt,
  resolvePlaywright,
  routeServiceFixtures,
  systemStatusFixture,
} from './screens-lib.mjs';

const scriptSource = fileURLToPath(new URL('./e2e-smoke.mjs', import.meta.url));
const librarySource = fileURLToPath(new URL('./screens-lib.mjs', import.meta.url));
const preflightSource = fileURLToPath(new URL('./browser-preflight.mjs', import.meta.url));
const webDir = fileURLToPath(new URL('../', import.meta.url));
const distSource = path.join(webDir, 'dist');
const playwrightDir = path.join(webDir, 'node_modules', 'playwright');

const fixtures = createTempFixtures();
const { mkdtempSync } = fixtures;
afterAll(() => fixtures.dispose());

function serverJson(pathname, state = {}) {
  const server = createServer(distSource, state);
  const handler = server.listeners('request')[0];
  return new Promise((resolve, reject) => {
    let status = 0;
    let body = '';
    const response = {
      writeHead(nextStatus) { status = nextStatus; },
      end(chunk = '') {
        body += String(chunk);
        try { resolve({ status, body: JSON.parse(body) }); } catch (error) { reject(error); }
      },
    };
    handler({ method: 'GET', url: pathname }, response);
  });
}

async function routedHandlers(fixtures = buildServiceFixtures()) {
  const handlers = new Map();
  await routeServiceFixtures({
    route(routePattern, handler) { handlers.set(routePattern, handler); },
  }, fixtures);
  return handlers;
}

async function routedJsonFrom(handlers, pattern, pathname = pattern.replaceAll('**', ''), options = {}) {
  const handler = handlers.get(pattern);
  expect(handler, `fixture route ${pattern}`).toBeTypeOf('function');
  let response;
  await handler({
    request: () => ({
      method: () => options.method ?? 'GET',
      url: () => `http://fixture${pathname}`,
      postData: () => options.body ?? null,
    }),
    fulfill(value) { response = value; },
  });
  return JSON.parse(response.body);
}

async function routedJson(pattern, fixtures = buildServiceFixtures(), pathname = pattern.replaceAll('**', ''), options = {}) {
  return routedJsonFrom(await routedHandlers(fixtures), pattern, pathname, options);
}

function fixtureModule() {
  const dir = path.join(mkdtempSync(path.join(tmpdir(), 'smoke-pw-fixture-')), 'playwright');
  mkdirSync(dir, { recursive: true });
  const executable = path.join(dir, 'missing-browser');
  writeFileSync(path.join(dir, 'package.json'), JSON.stringify({
    name: 'playwright',
    version: '1.59.1',
    main: 'index.js',
  }));
  writeFileSync(
    path.join(dir, 'index.js'),
    `module.exports = { chromium: { executablePath: () => ${JSON.stringify(executable)} } };\n`,
  );
  return dir;
}

// Run copies from an isolated web/tools shape so repo node_modules cannot be
// found unless a test explicitly supplies PLAYWRIGHT_MODULE_PATH.
function spawnCopiedScript(env, args = []) {
  const root = mkdtempSync(path.join(tmpdir(), 'e2e-smoke-spawn-'));
  const tools = path.join(root, 'tools');
  const dist = path.join(root, 'dist');
  mkdirSync(tools, { recursive: true });
  mkdirSync(dist);
  copyFileSync(scriptSource, path.join(tools, 'e2e-smoke.mjs'));
  copyFileSync(librarySource, path.join(tools, 'screens-lib.mjs'));
  copyFileSync(preflightSource, path.join(tools, 'browser-preflight.mjs'));
  writeFileSync(path.join(dist, 'index.html'), '<!doctype html><div id="root"></div>');
  writeFileSync(path.join(dist, 'webdist.json'), `${JSON.stringify({ webVersion: 'test' })}\n`);
  return spawnSync(
    process.execPath,
    [
      path.join(tools, 'e2e-smoke.mjs'),
      ...(args.includes('--dist') ? [] : ['--dist', dist]),
      ...args,
    ],
    {
      env: { PATH: process.env.PATH, ...env },
      encoding: 'utf8',
      timeout: 90_000,
    },
  );
}

function browserEnv() {
  const env = { PLAYWRIGHT_MODULE_PATH: playwrightDir };
  for (const key of ['HOME', 'XDG_CACHE_HOME', 'PLAYWRIGHT_BROWSERS_PATH']) {
    if (process.env[key]) env[key] = process.env[key];
  }
  return env;
}

function copyDistWithTokenRenamed(source, token) {
  const broken = path.join(mkdtempSync(path.join(tmpdir(), 'e2e-smoke-broken-')), 'dist');
  cpSync(source, broken, { recursive: true });
  const pending = [broken];
  let replacements = 0;
  while (pending.length > 0) {
    const dir = pending.pop();
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const filename = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        pending.push(filename);
        continue;
      }
      if (!/\.(?:css|html|js)$/.test(entry.name)) continue;
      const source = readFileSync(filename, 'utf8');
      const rewritten = source.replaceAll(token, `${token}-broken`);
      if (rewritten !== source) {
        writeFileSync(filename, rewritten);
        replacements += 1;
      }
    }
  }
  expect(replacements, `built dist files containing ${token}`).toBeGreaterThan(0);
  return broken;
}

describe('e2e smoke infrastructure failures', () => {
  it('runs credentials API-key evidence capture against the verified bundle', async () => {
    const calls = [];
    const output = [];
    const browser = { fixture: 'verified smoke browser' };
    const result = await smoke.ensureCredentialsKeysSmokeEvidence('/verified/dist', {
      browser,
      loadCapture: async () => ({
        ensureCredentialsKeysEvidence: async (options) => {
          calls.push(options);
          return {
            captures: [
              'credentials-api-key-add-dark.png',
              'credentials-api-key-add-light.png',
            ],
            created: true,
          };
        },
      }),
      write: (message) => output.push(message),
    });

    expect(calls).toEqual([{ currentDist: '/verified/dist', browser }]);
    expect(result).toEqual({
      captures: [
        'credentials-api-key-add-dark.png',
        'credentials-api-key-add-light.png',
      ],
      created: true,
    });
    expect(output.join('')).toContain(
      'Credentials API-key evidence verified: credentials-api-key-add-dark.png, credentials-api-key-add-light.png (captured)',
    );
  });

  it('rejects a stale default bundle before launching Playwright', () => {
    const dist = path.join(mkdtempSync(path.join(tmpdir(), 'e2e-smoke-stale-')), 'dist');
    mkdirSync(dist);
    writeFileSync(path.join(dist, 'index.html'), '<!doctype html><div id="root"></div>');
    writeFileSync(path.join(dist, 'webdist.json'), `${JSON.stringify({
      webVersion: 'test',
      sourceSha: '0000000000000000000000000000000000000000',
    })}\n`);

    expect(() => smoke.ensureDist(dist, false, false)).toThrowError(
      /web bundle provenance mismatch: .* sourceSha is "0000000000000000000000000000000000000000", current HEAD is [0-9a-f]+; rebuild with "npm run build" or pass --build/,
    );
  });

  it('rebuilds a stale default bundle instead of failing', () => {
    const dist = path.join(mkdtempSync(path.join(tmpdir(), 'e2e-smoke-stale-rebuild-')), 'dist');
    mkdirSync(dist);
    writeFileSync(path.join(dist, 'index.html'), '<!doctype html><div id="root"></div>');
    writeFileSync(path.join(dist, 'webdist.json'), `${JSON.stringify({
      webVersion: 'test',
      sourceSha: '0000000000000000000000000000000000000000',
    })}\n`);

    expect(smoke.staleDefaultDistReason(dist, false, false)).toMatch(
      /web bundle provenance mismatch: .* sourceSha is "0000000000000000000000000000000000000000"/,
    );
    expect(smoke.staleDefaultDistReason(dist, true, false)).toBe('');
    expect(smoke.staleDefaultDistReason(dist, false, true)).toBe('');
  });

  it('keeps a current default bundle without rebuilding', () => {
    const dist = path.join(mkdtempSync(path.join(tmpdir(), 'e2e-smoke-current-')), 'dist');
    mkdirSync(dist);
    writeFileSync(path.join(dist, 'index.html'), '<!doctype html><div id="root"></div>');
    const sourceSha = spawnSync('git', ['rev-parse', 'HEAD'], {
      cwd: webDir,
      encoding: 'utf8',
    }).stdout.trim();
    writeFileSync(path.join(dist, 'webdist.json'), `${JSON.stringify({ webVersion: 'test', sourceSha })}\n`);

    expect(smoke.staleDefaultDistReason(dist, false, false)).toBe('');
  });

  it('lets a missing default bundle fall through to the normal build', () => {
    const dist = path.join(mkdtempSync(path.join(tmpdir(), 'e2e-smoke-missing-default-')), 'dist');
    mkdirSync(dist);

    expect(smoke.staleDefaultDistReason(dist, false, false)).toBe('');
  });

  it('treats a default bundle without a manifest as stale', () => {
    const dist = path.join(mkdtempSync(path.join(tmpdir(), 'e2e-smoke-no-manifest-')), 'dist');
    mkdirSync(dist);
    writeFileSync(path.join(dist, 'index.html'), '<!doctype html><div id="root"></div>');

    expect(smoke.staleDefaultDistReason(dist, false, false)).toMatch(
      /web bundle provenance mismatch: cannot read/,
    );
  });

  it('reads the dist web version only when the manifest contract is satisfied', () => {
    const dist = path.join(mkdtempSync(path.join(tmpdir(), 'e2e-smoke-web-version-')), 'dist');
    mkdirSync(dist);
    writeFileSync(path.join(dist, 'index.html'), '<!doctype html><div id="root"></div>');

    expect(() => smoke.readDistWebVersion(dist)).toThrowError(/cannot read webVersion/);

    writeFileSync(path.join(dist, 'webdist.json'), `${JSON.stringify({ webVersion: '1.2.3' })}\n`);
    expect(smoke.readDistWebVersion(dist)).toBe('1.2.3');
  });

  it('exits 78 and lists every tried module candidate when Playwright is absent', () => {
    const result = spawnCopiedScript({});
    expect(result.status).toBe(78);
    expect(result.stderr).toContain('Playwright module missing or incompatible');
    expect(result.stderr).toContain('node_modules/playwright: not found');
    expect(result.stderr).toContain('node_modules/playwright-core: not found');
  });

  it('exits 79 with install guidance when Chromium is absent', () => {
    const result = spawnCopiedScript({ PLAYWRIGHT_MODULE_PATH: fixtureModule() });
    expect(result.status).toBe(79);
    expect(result.stderr).toContain('chromium executable not found');
    expect(result.stderr).toContain('npx playwright install chromium');
  });

  it('rejects --build with a custom --dist before attempting a build', () => {
    const customDist = path.join(mkdtempSync(path.join(tmpdir(), 'smoke-custom-dist-')), 'dist');
    mkdirSync(customDist);
    writeFileSync(path.join(customDist, 'index.html'), '<!doctype html><div id="root"></div>');
    const result = spawnCopiedScript({}, ['--dist', customDist, '--build']);
    expect(result.status).toBe(2);
    expect(result.stderr).toContain('--build cannot be combined with --dist');
    expect(result.stdout).not.toContain('Building web UI');
  });

  it('rejects a missing custom --dist instead of building the default directory', () => {
    const customDist = path.join(mkdtempSync(path.join(tmpdir(), 'smoke-missing-dist-')), 'missing');
    const result = spawnCopiedScript({}, ['--dist', customDist]);
    expect(result.status).toBe(2);
    expect(result.stderr).toContain('custom --dist must contain index.html');
    expect(result.stdout).not.toContain('Building web UI');
  });

  it('rejects a custom --dist without webdist.json before reporting reuse', () => {
    const customDist = path.join(mkdtempSync(path.join(tmpdir(), 'smoke-incomplete-dist-')), 'dist');
    mkdirSync(customDist);
    writeFileSync(path.join(customDist, 'index.html'), '<!doctype html><div id="root"></div>');

    const result = spawnCopiedScript({}, ['--dist', customDist]);

    expect(result.status).toBe(2);
    expect(result.stderr).toContain('custom --dist must contain webdist.json');
    expect(result.stderr).toContain(path.join(customDist, 'webdist.json'));
    expect(result.stdout).not.toContain('Reusing web build');
  });
});

describe('e2e smoke browser setup and assertions', () => {
  it('opens each smoke surface on a fresh page with preferences installed before navigation', async () => {
    const events = [];
    let pageNumber = 0;
    const context = {
      async newPage() {
        pageNumber += 1;
        const label = `page-${pageNumber}`;
        events.push(`${label}:created`);
        return {
          async addInitScript(_script, theme) { events.push(`${label}:preferences:${theme}`); },
          setDefaultTimeout(timeout) { events.push(`${label}:timeout:${timeout}`); },
          setDefaultNavigationTimeout(timeout) { events.push(`${label}:navigation-timeout:${timeout}`); },
          async goto(url) { events.push(`${label}:goto:${url}`); },
        };
      },
    };
    const attach = (page) => events.push(page ? `page-${pageNumber}:attached` : 'missing-page');

    expect(smoke.openSmokePage).toBeTypeOf('function');
    await smoke.openSmokePage(context, attach, 'dark', 'http://smoke/');
    await smoke.openSmokePage(context, attach, 'dark', 'http://smoke/#view=tasks');

    expect(events).toEqual([
      'page-1:created',
      'page-1:attached',
      'page-1:preferences:dark',
      'page-1:timeout:5000',
      'page-1:navigation-timeout:15000',
      'page-1:goto:http://smoke/',
      'page-2:created',
      'page-2:attached',
      'page-2:preferences:dark',
      'page-2:timeout:5000',
      'page-2:navigation-timeout:15000',
      'page-2:goto:http://smoke/#view=tasks',
    ]);
  });

  it('installs localStorage preferences on an already-created page', async () => {
    const calls = [];
    const page = { addInitScript: async (...args) => { calls.push(args); } };
    expect(smoke.installStoragePreferences).toBeTypeOf('function');
    await smoke.installStoragePreferences(page, 'dark');
    expect(calls).toHaveLength(1);
    expect(calls[0][1]).toBe('dark');
  });

  it('keeps every asserted selector aligned with the rendered source', () => {
    expect(smoke.smokeSurfaceSelectors).toMatchObject({
      appShell: '.app',
      sidebar: '#sidebar',
      sidebarSection: '.sidebar-section',
      settingsTrigger: 'button[aria-label="Settings"]',
      settingsModal: '.settings-modal[role="dialog"]',
      settingsTab: '.settings-nav [role="tab"]',
      systemPage: '.system-page[role="dialog"]',
      systemKpis: '.system-kpis',
      systemResources: '.system-resources',
      systemPlaceholder: '.system-state',
    });
    const app = readFileSync(path.join(webDir, 'src', 'App.tsx'), 'utf8');
    const topbar = readFileSync(path.join(webDir, 'src', 'components', 'Topbar.tsx'), 'utf8');
    const settings = readFileSync(path.join(webDir, 'src', 'components', 'SettingsModal.tsx'), 'utf8');
    const system = readFileSync(path.join(webDir, 'src', 'components', 'system', 'SystemPage.tsx'), 'utf8');
    expect(app).toContain('className="app"');
    expect(app).toContain('id="sidebar"');
    expect(topbar).toContain('aria-label="Settings"');
    expect(settings).toContain('settings-modal');
    expect(system).toContain('className="system-page" role="dialog"');
    expect(system).toContain('className="system-kpis"');
    expect(system).toContain('system-resources');
    expect(system).toContain('className="system-state"');
  });

  it('documents negative repros for every key smoke surface', () => {
    const source = readFileSync(scriptSource, 'utf8');
    expect(source).toContain('App-shell breakage repro');
    expect(source).toContain('Sidebar breakage repro');
    expect(source).toContain('Settings breakage repro');
    expect(source).toContain('System breakage repro');
  });

  it('never allowlists page errors and starts with no console-error exceptions', () => {
    expect(smoke.consoleErrorAllowlist).toEqual([]);
    expect(smoke.unexpectedRuntimeErrors([
      { kind: 'console', detail: 'fixture console error' },
      { kind: 'page', detail: 'fixture page error' },
    ])).toEqual([
      { kind: 'console', detail: 'fixture console error' },
      { kind: 'page', detail: 'fixture page error' },
    ]);
  });
});

describe('e2e smoke server fixtures', () => {
  it('serves representative resource data for the System cockpit', async () => {
    const response = await serverJson('/api/system/status');
    expect(response.status).toBe(200);
    expect(response.body).toMatchObject({
      platform: { os: 'linux', procfs: true },
      daemon: { rssMB: expect.any(Number) },
      disk: {
        freeMB: expect.any(Number), totalMB: expect.any(Number),
        freeBytes: expect.any(Number), usedBytes: expect.any(Number), totalBytes: expect.any(Number),
        growth: { day: expect.objectContaining({ window: '24h' }), week: expect.objectContaining({ window: '168h' }) },
        cleanup: expect.objectContaining({ scope: expect.any(String), operations: expect.any(Array) }),
      },
      level: { overall: 'ok' },
    });
    const disk = response.body.disk;
    const protections = new Set(disk.breakdown.map((row) => row.protection));
    expect(protections.has('managed')).toBe(true);
    expect(protections.has('read-only')).toBe(true);
    expect(protections.has('protected')).toBe(true);
    expect(disk.breakdown.find((row) => row.category === 'shared_go_build')).toMatchObject({ protection: 'read-only', cleanup: 'none' });
    expect(disk.breakdown.find((row) => row.category === 'managed_go_build')).toMatchObject({ protection: 'managed', cleanup: 'generated_storage_janitor' });
    expect(disk.breakdown.find((row) => row.category === 'external_user_cache')).toMatchObject({ cleanup: 'none', complete: false });
    expect(disk.cleanup.state).toBe('guest_only_pending_host_reclaim');
    expect(disk.cleanup.message).toBe('Linux space freed; Windows image still allocated; maintenance needed');
  });

  it('keeps the producer and cleanup surfaces in the System cockpit script', () => {
    const cockpit = readFileSync(scriptSource, 'utf8');
    expect(cockpit).toContain('.system-producers');
    expect(cockpit).toContain('.system-cleanup');
    expect(cockpit).toContain('Linux space freed; Windows image still allocated; maintenance needed');
  });

  it('can align daemon health with the dist web version while capture defaults stay dev-like', async () => {
    const aligned = await serverJson('/api/health', { healthVersion: '0.0.998' });
    const captureDefault = await serverJson('/api/health');
    expect(aligned.status).toBe(200);
    expect(aligned.body.version).toBe('0.0.998');
    expect(aligned.body.releaseReadiness.runningVersion).toBe('0.0.998');
    expect(captureDefault.body.version).toBe('dev');
  });

  it('reads the standalone fixture server version from its built dist', () => {
    const dist = mkdtempSync(path.join(tmpdir(), 'screens-lib-version-'));
    writeFileSync(path.join(dist, 'webdist.json'), `${JSON.stringify({ webVersion: '0.0.1035' })}\n`);
    expect(fixtureHealthVersion(dist)).toBe('0.0.1035');
  });

  it('aligns the standalone Playwright fixture server with its built bundle', () => {
    const dist = path.join(mkdtempSync(path.join(tmpdir(), 'screens-fixture-version-')), 'dist');
    mkdirSync(dist);
    writeFileSync(path.join(dist, 'webdist.json'), JSON.stringify({ webVersion: '0.0.1035' }));

    expect(fixtureHealthVersion(dist)).toBe('0.0.1035');
    expect(fixtureHealthVersion(path.join(dist, 'missing'))).toBe('dev');
  });

  it('serves a typed disabled release train instead of the array API fallback', async () => {
    const response = await serverJson('/api/workflow/release-train');
    expect(response.status).toBe(200);
    expect(response.body).toMatchObject({
      policy: { enabled: false },
      state: { phase: 'disabled', train: null, parked: null, restartPending: null },
      preview: null,
      events: [],
    });
  });

  it('serves typed ambient app fixtures without triggering browser console errors', async () => {
    const expected = new Map([
      ['/api/config', {}],
      ['/api/update/status', { available: false, applying: false }],
      ['/api/recovery/settings', { resumeAfterRestart: 'manual' }],
      ['/api/native/settings', { promptSuggestions: false }],
      ['/api/update/settings', { autoApply: 'off' }],
      ['/api/aria/status', { paused: false }],
      ['/api/aria/auto-pending', { pending: [] }],
      ['/api/claude/accounts', { accounts: [] }],
      ['/api/claude/autoswitch', { enabled: false, state: 'paused' }],
      ['/api/claude/team', { available: false }],
      ['/api/codex/accounts', { accounts: [], liveAuthPresent: false }],
      ['/api/codex/team', { available: false }],
      ['/api/muse/accounts', { accounts: [], liveAuthPresent: false }],
      ['/api/muse/team', { available: false }],
      ['/api/feedback/inbox', { events: [] }],
      ['/api/agents/status', { fixAgent: null, cloud: null }],
      ['/api/enter-gap', { claudeMs: 400, defaultMs: 100 }],
      ['/api/workflow/runtimes/health', {
        runtimes: expect.any(Array),
        resolution: expect.any(Array),
      }],
    ]);
    for (const [pathname, shape] of expected) {
      const response = await serverJson(pathname);
      expect(response.status, pathname).toBe(200);
      expect(response.body, pathname).toMatchObject(shape);
    }
    for (const pathname of [
      '/api/agents/reports',
      '/api/workflow/activation',
      '/api/workflow/suggestions',
      '/api/workflow/workstreams',
    ]) {
      expect(await serverJson(pathname), pathname).toEqual({ status: 200, body: [] });
    }
  });
});

// Browser assertions share one immutable snapshot. A concurrent Vite build
// empties web/dist before replacing its files and writes webdist.json last, so
// copying the live directory independently in each test can cross that window
// even after the module-load availability check passed.
// Like the CLI's default-dist path, rebuild an existing web/dist when its
// sourceSha mismatches HEAD; Vitest later passes a snapshot as an explicit
// --dist, which is intentionally served as-is and would bypass that check.
const staleDistReason = smoke.staleDefaultDistReason(distSource, false, false);
if (staleDistReason) smoke.ensureDist(distSource, true, false);

const browserDistSource = (() => {
  if (!existsSync(path.join(distSource, 'index.html'))) return false;
  for (let attempt = 0; attempt < 2; attempt += 1) {
    const snapshot = path.join(mkdtempSync(path.join(tmpdir(), 'e2e-smoke-dist-')), 'dist');
    try {
      cpSync(distSource, snapshot, { recursive: true });
      smoke.readDistWebVersion(snapshot);
      if (!existsSync(path.join(snapshot, 'index.html'))) continue;
      return snapshot;
    } catch {
      // The source may have crossed a rebuild window. Retry the whole copy
      // once into a fresh directory; a partial snapshot is never consumed.
    }
  }
  return false;
})();
const hasDist = browserDistSource !== false;

describe.skipIf(!hasDist)('e2e smoke dist provenance', () => {
  it('browser reproductions consume a dist built from the current HEAD', () => {
    expect(() => smoke.assertCurrentDistProvenance(browserDistSource)).not.toThrow();
  });
});

let smokePlaywright = null;
let hasChromium = false;
const browserSandbox = await probeNetworkSandbox();
if (!browserSandbox.blocked && existsSync(path.join(playwrightDir, 'package.json'))) {
  smokePlaywright = await resolvePlaywright();
  try {
    if (smokePlaywright?.chromium?.executablePath() && existsSync(smokePlaywright.chromium.executablePath())) {
      const browser = await launchChromium(smokePlaywright, { headless: true });
      await browser.close();
      hasChromium = true;
    }
  } catch {
    hasChromium = false;
  }
}

describe.skipIf(!hasDist || !hasChromium)('e2e smoke breakage reproduction', () => {
  it('renders the shared workspace without browser runtime errors', () => {
    const result = spawnCopiedScript(browserEnv(), ['--dist', browserDistSource, '--theme', 'dark']);
    const output = `${result.stdout}\n${result.stderr}`;
    expect(result.status, output).toBe(0);
    expect(output).toContain('PASS [dark] system:');
    expect(output).toContain('PASS [dark] console: no console errors or page errors');
  }, 100_000);

  it('waits for the System cockpit data before measuring its layout', async () => {
    const server = createServer(browserDistSource, {
      healthVersion: smoke.readDistWebVersion(browserDistSource),
    });
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
    const baseUrl = `http://127.0.0.1:${server.address().port}/`;
    const browser = await launchChromium(smokePlaywright, { headless: true });
    let context;
    try {
      context = await browser.newContext({
        viewport: { width: 1440, height: 1000 },
        serviceWorkers: 'block',
      });
      await routeServiceFixtures(context, buildServiceFixtures(Date.now()));
      await context.route('**/api/system/status*', async (route) => {
        await new Promise((resolve) => setTimeout(resolve, 1500));
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify(systemStatusFixture()),
        });
      });
      const page = await smoke.openSmokePage(context, () => {}, 'dark', baseUrl);
      await expect(smoke.assertSystemCockpit(page)).resolves.toMatch(
        /cockpit rendered at \d+px and collapsed cleanly at 700px/,
      );
    } finally {
      await context?.close().catch(() => {});
      await browser.close().catch(() => {});
      await new Promise((resolve) => server.close(resolve));
    }
  }, 100_000);

  it('names the missing System cockpit selector instead of a raw TypeError', () => {
    const broken = copyDistWithTokenRenamed(browserDistSource, 'system-kpis');
    const result = spawnCopiedScript(browserEnv(), ['--dist', broken, '--theme', 'dark']);
    const output = `${result.stdout}\n${result.stderr}`;
    const failureLines = output.split('\n').filter((line) => line.startsWith('FAIL [dark]'));
    expect(result.status, output).toBe(1);
    expect(failureLines).toHaveLength(1);
    expect(failureLines[0]).toContain('FAIL [dark] system:');
    expect(failureLines[0]).toContain('.system-kpis');
    expect(failureLines[0]).not.toContain('getComputedStyle');
    expect(failureLines[0]).not.toContain('TypeError');
  }, 100_000);

  it.each([
    ['sidebar', 'sidebar-section'],
    ['settings', 'settings-modal'],
    ['system', 'system-page'],
  ])('reports only the matching %s assertion when its rendered token is renamed', (surface, token) => {
    const broken = copyDistWithTokenRenamed(browserDistSource, token);
    const result = spawnCopiedScript(browserEnv(), ['--dist', broken, '--theme', 'dark']);
    const output = `${result.stdout}\n${result.stderr}`;
    const failureLines = output.split('\n').filter((line) => line.startsWith('FAIL [dark]'));
    expect(result.status, output).toBe(1);
    expect(failureLines).toHaveLength(1);
    expect(failureLines[0]).toContain(`FAIL [dark] ${surface}:`);
  }, 100_000);

  it('fails the app-shell assertion when the React root is renamed', () => {
    const broken = path.join(mkdtempSync(path.join(tmpdir(), 'e2e-smoke-broken-')), 'dist');
    cpSync(browserDistSource, broken, { recursive: true });
    const index = path.join(broken, 'index.html');
    const original = readFileSync(index, 'utf8');
    expect(original).toContain('id="root"');
    writeFileSync(index, original.replace('id="root"', 'id="broken"'));

    const result = spawnCopiedScript(browserEnv(), ['--dist', broken, '--theme', 'dark']);
    expect(result.status).not.toBe(0);
    expect(`${result.stdout}\n${result.stderr}`).toContain('FAIL [dark] app shell:');
  }, 100_000);
});
